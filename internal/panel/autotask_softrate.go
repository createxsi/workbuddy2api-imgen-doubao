package panel

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 任务链撞上游 growth 域软限流（429）时的重试预算：一轮全量任务要连打几十次
// 列表/接受/领奖，撞上一次就整条链失败，而等一两分钟即可恢复（动作都幂等）。
// 等待取 Retry-After，缺省按 softRateGap 线性递增，均封顶 softRateGapMax。
var (
	softRateRetries = 2
	softRateGap     = 15 * time.Second
	softRateGapMax  = 60 * time.Second
	softRateSleep   = time.Sleep
)

// taskSoftRate 账号级限流窗口：重试用尽后记下，同轮后续动作快失败，不再逐个睡。
type taskSoftRate struct {
	mu    sync.Mutex
	until map[string]time.Time
}

func (s *taskSoftRate) remaining(uid string) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := time.Until(s.until[uid])
	if d <= 0 {
		delete(s.until, uid)
		return 0
	}
	return d
}

func (s *taskSoftRate) note(uid string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.until == nil {
		s.until = map[string]time.Time{}
	}
	s.until[uid] = time.Now().Add(d)
}

func isSoftRateErr(err error) bool {
	var ue *upstream.Error
	return errors.As(err, &ue) && ue.Kind == upstream.ErrSoftRate
}

func softRateWait(err error, attempt int) time.Duration {
	d := time.Duration(attempt) * softRateGap
	var ue *upstream.Error
	if errors.As(err, &ue) && ue.RetryAfter > d {
		d = ue.RetryAfter
	}
	return min(d, softRateGapMax)
}

// listTasksSoftRate 拉任务列表，软限流时等窗口重试；窗口内直接快失败。
func (p *Panel) listTasksSoftRate(a *auth.Auth, mp bool) ([]upstream.Task, error) {
	if d := p.taskRate.remaining(a.UID); d > 0 {
		return nil, fmt.Errorf("上游限流（429），约 %s 后恢复", d.Round(time.Second))
	}
	for attempt := 0; ; attempt++ {
		var tasks []upstream.Task
		var err error
		if mp {
			tasks, err = p.cfg.Upstream.ListTasksMP(a)
		} else {
			tasks, err = p.cfg.Upstream.ListTasks(a)
		}
		if err == nil || !isSoftRateErr(err) {
			return tasks, err
		}
		if attempt >= softRateRetries {
			p.taskRate.note(a.UID, softRateGapMax)
			return nil, fmt.Errorf("上游限流（429），已重试 %d 次仍未恢复，约 %s 后再试：%w",
				softRateRetries, softRateGapMax.Round(time.Second), err)
		}
		d := softRateWait(err, attempt+1)
		log.Printf("autotask %s: 上游限流，%s 后重试（%d/%d）", logfmt.Label(a.UID, a.Nickname), d.Round(time.Second), attempt+1, softRateRetries)
		softRateSleep(d)
	}
}

// runActSoftRate 执行任务动作；撞 429（请求未被执行）时等窗口重发一次。
func (p *Panel) runActSoftRate(a *auth.Auth, act *autoAction) (string, error) {
	if d := p.taskRate.remaining(a.UID); d > 0 {
		return "", fmt.Errorf("上游限流（429），约 %s 后恢复", d.Round(time.Second))
	}
	msg, err := act.run(p, a)
	if err != nil && isSoftRateErr(err) {
		d := softRateWait(err, 1)
		log.Printf("autotask %s: 动作 %s 撞上游限流，%s 后重试一次", logfmt.Label(a.UID, a.Nickname), act.TaskCode, d.Round(time.Second))
		softRateSleep(d)
		msg, err = act.run(p, a)
		if err != nil && isSoftRateErr(err) {
			p.taskRate.note(a.UID, softRateGapMax)
		}
	}
	return msg, err
}

package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"sort"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/cst"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// 账号级摊开（参考 misakano7545/workbuddy2api-panel issue #45 方案 A）。
//
// 同一出口上多个账号在几秒内做同一动作（签到、刷新 token、活跃上报……）是最容易
// 识别的时间形状。开启 schedule.account_spread_minutes 后，**排程触发**的账号遍历
// 任务让每个账号在窗口内自己的偏移上执行：偏移 = hash(任务|账号|CST 日期) % 窗口，
// 当天固定、重启不重排。偏移撞车时按 account_spread_min_gap_seconds 把靠后的推开，
// 而不是等距排列——固定节拍同样可识别。
//
// 面板手动触发恒不摊开（一次点击要的是立刻跑完）。窗口 0 = 关闭，行为与之前一致。

type spreadCtxKey struct{}

// withSpread 标记本次遍历来自排程，需要按 kind 摊开。
func withSpread(ctx context.Context, kind string) context.Context {
	return context.WithValue(ctx, spreadCtxKey{}, kind)
}

// spreadOffset 账号在窗口内的稳定偏移（秒精度）。
func spreadOffset(kind, uid, day string, window time.Duration) time.Duration {
	if window <= 0 {
		return 0
	}
	sum := sha256.Sum256([]byte(kind + "|" + uid + "|" + day))
	secs := int64(window / time.Second)
	if secs <= 0 {
		return 0
	}
	return time.Duration(binary.BigEndian.Uint64(sum[:8])%uint64(secs)) * time.Second
}

// planSpread 按偏移排序账号并施加最小间隔，返回排序后的列表与 uid → 偏移。
func planSpread(list []pool.Status, kind, day string, window, gap time.Duration) ([]pool.Status, map[string]time.Duration) {
	out := make([]pool.Status, len(list))
	copy(out, list)
	offs := make(map[string]time.Duration, len(out))
	for _, st := range out {
		offs[st.UID] = spreadOffset(kind, st.UID, day, window)
	}
	sort.SliceStable(out, func(i, j int) bool { return offs[out[i].UID] < offs[out[j].UID] })
	if gap > 0 {
		for i := 1; i < len(out); i++ {
			prev := offs[out[i-1].UID]
			if offs[out[i].UID] < prev+gap {
				offs[out[i].UID] = prev + gap
			}
		}
	}
	return out, offs
}

// accountsFor 返回本轮遍历的账号序列，以及「轮到该账号前等待」的函数。
// 非排程触发或窗口为 0 时：原顺序，等待函数立即返回 true。
// 等待函数在 ctx 取消时返回 false（调用方应结束本轮）。
func (s *Scheduler) accountsFor(ctx context.Context) ([]pool.Status, func(uid string) bool) {
	list := s.cfg.Pool.List()
	kind, _ := ctx.Value(spreadCtxKey{}).(string)
	s.schedMu.Lock()
	window, gap := s.cfg.AccountSpread, s.cfg.AccountSpreadMinGap
	s.schedMu.Unlock()
	if kind == "" || window <= 0 || len(list) < 2 {
		return list, func(string) bool { return ctx.Err() == nil }
	}
	start := time.Now()
	ordered, offs := planSpread(list, kind, cst.Day(start), window, gap)
	return ordered, func(uid string) bool {
		return sleepCtx(ctx, time.Until(start.Add(offs[uid])))
	}
}

// SetAccountSpread 运行期热改摊开窗口与最小间隔（面板保存配置时调用）。
func (s *Scheduler) SetAccountSpread(window, gap time.Duration) {
	s.schedMu.Lock()
	s.cfg.AccountSpread, s.cfg.AccountSpreadMinGap = clampSpread(window, gap)
	s.schedMu.Unlock()
}

// clampSpread 窗口上限 45 分钟（排程时点按整点，批次需在下个时点前跑完）、间隔非负。
func clampSpread(window, gap time.Duration) (time.Duration, time.Duration) {
	if window < 0 {
		window = 0
	}
	if window > 45*time.Minute {
		window = 45 * time.Minute
	}
	if gap < 0 {
		gap = 0
	}
	return window, gap
}

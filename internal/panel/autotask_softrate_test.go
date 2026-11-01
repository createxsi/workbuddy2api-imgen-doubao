package panel

import (
	"errors"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func TestRunActSoftRateRetriesOnceThenGates(t *testing.T) {
	var slept []time.Duration
	old := softRateSleep
	softRateSleep = func(d time.Duration) { slept = append(slept, d) }
	t.Cleanup(func() { softRateSleep = old })

	p := &Panel{}
	a := &auth.Auth{UID: "u1"}
	calls := 0
	rate := &upstream.Error{Kind: upstream.ErrSoftRate, Status: 429, RetryAfter: 5 * time.Minute}
	act := &autoAction{TaskCode: "x", run: func(*Panel, *auth.Auth) (string, error) {
		calls++
		if calls == 1 {
			return "", rate
		}
		return "ok", nil
	}}
	if msg, err := p.runActSoftRate(a, act); err != nil || msg != "ok" || calls != 2 {
		t.Fatalf("msg=%q err=%v calls=%d", msg, err, calls)
	}
	if len(slept) != 1 || slept[0] != softRateGapMax {
		t.Fatalf("等待应取 Retry-After 并封顶: %v", slept)
	}

	// 连续限流：重试一次后记窗口，后续动作快失败不再打上游。
	calls = 0
	act.run = func(*Panel, *auth.Auth) (string, error) { calls++; return "", rate }
	if _, err := p.runActSoftRate(a, act); !isSoftRateErr(err) || calls != 2 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	if _, err := p.runActSoftRate(a, act); err == nil || calls != 2 {
		t.Fatalf("窗口内应快失败: err=%v calls=%d", err, calls)
	}
	// 非限流错误不重试。
	p2 := &Panel{}
	n := 0
	other := &autoAction{run: func(*Panel, *auth.Auth) (string, error) { n++; return "", errors.New("boom") }}
	if _, err := p2.runActSoftRate(a, other); err == nil || n != 1 {
		t.Fatalf("非限流不应重试: n=%d", n)
	}
}

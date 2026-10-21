// busywait_test.go 钉住「模型繁忙等一次」的判据：只有「纯 14003 模型繁忙、且剩余冷却
// 在等待上限内」才值得在请求内等一等，其余一律立刻上报（等下去只是让客户端干耗）。
package server

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

func TestBusyWaitFor(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		blk  pool.ModelBlockStatus
		want bool
	}{
		{"繁忙且在上限内（上游原文）", pool.ModelBlockStatus{Blocked: true, Reason: `{"code":14003,"msg":"too many requests"}`, Until: now.Add(3 * time.Second)}, true},
		{"繁忙且在上限内（本地标记）", pool.ModelBlockStatus{Blocked: true, Reason: "14003 model busy", Until: now.Add(3 * time.Second)}, true},
		{"繁忙但超出上限", pool.ModelBlockStatus{Blocked: true, Reason: `{"code":14003,"msg":"too many requests"}`, Until: now.Add(30 * time.Second)}, false},
		{"繁忙但无解封时刻", pool.ModelBlockStatus{Blocked: true, Reason: `{"code":14003,"msg":"too many requests"}`}, false},
		{"模型不存在（换号必然同样失败）", pool.ModelBlockStatus{Blocked: true, Reason: "11102 model not found", Until: now.Add(3 * time.Second)}, false},
		{"未被阻塞", pool.ModelBlockStatus{}, false},
	}
	for _, c := range cases {
		if got := busyWaitFor(c.blk, now, 10*time.Second) > 0; got != c.want {
			t.Errorf("%s: got wait=%v want %v", c.name, got, c.want)
		}
	}
}

// 上限为 0（测试/关闭）时恒不等：轮转测试不被这条路径拖慢。
func TestBusyWaitForDisabled(t *testing.T) {
	blk := pool.ModelBlockStatus{Blocked: true, Reason: `{"code":14003,"msg":"too many requests"}`, Until: time.Now().Add(time.Second)}
	if d := busyWaitFor(blk, time.Now(), 0); d != 0 {
		t.Fatalf("cap=0 时不应等待，got %v", d)
	}
}

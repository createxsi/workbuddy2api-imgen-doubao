package panel

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestGrowthPendingClaimOnly 白名单外的任务：已达标未领必须入队（否则永远没人领，
// 积分卡住）；未达标仍不入队；已领/锁定不入队。
func TestGrowthPendingClaimOnly(t *testing.T) {
	const code = "not_in_whitelist_task"
	if autoActionFor(code) != nil {
		t.Fatalf("前置：%s 不应在动作白名单内", code)
	}
	cases := []struct {
		name string
		t    upstream.Task
		want bool
	}{
		{"达标未领", upstream.Task{TaskCode: code, Target: 3, Current: 3}, true},
		{"本地推算可领", upstream.Task{TaskCode: code, Claimable: true}, true},
		{"未达标", upstream.Task{TaskCode: code, Target: 3, Current: 1}, false},
		{"已领", upstream.Task{TaskCode: code, Target: 3, Current: 3, Claimed: true}, false},
		{"锁定", upstream.Task{TaskCode: code, Target: 3, Current: 3, Locked: true}, false},
		{"白名单未达标", upstream.Task{TaskCode: "chat_5", Target: 5, Current: 0}, true},
	}
	for _, c := range cases {
		if got := growthPending(c.t); got != c.want {
			t.Errorf("%s: growthPending=%v want %v", c.name, got, c.want)
		}
	}
}

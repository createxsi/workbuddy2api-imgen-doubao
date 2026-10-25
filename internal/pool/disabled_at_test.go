package pool

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 禁用时刻：禁用时记下、跨重启保留、解冻清零。
func TestDisabledAtPersistsAndClears(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	before := time.Now().Add(-time.Second)
	p.Disable("u1", "manual disable (panel)")
	st, _ := p.Status("u1")
	at, err := time.Parse(time.RFC3339, st.DisabledAt)
	if err != nil || at.Before(before.Truncate(time.Second)) {
		t.Fatalf("disabled_at=%q err=%v", st.DisabledAt, err)
	}
	p.Flush()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st2, _ := p2.Status("u1")
	if !st2.Disabled || st2.DisabledAt != st.DisabledAt {
		t.Fatalf("重启后 disabled=%v at=%q want %q", st2.Disabled, st2.DisabledAt, st.DisabledAt)
	}
	p2.Revive("u1")
	if st3, _ := p2.Status("u1"); st3.DisabledAt != "" {
		t.Fatalf("解冻后应清零：%q", st3.DisabledAt)
	}
}

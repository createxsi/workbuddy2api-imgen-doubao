//go:build !windows

package updater

import (
	"os/exec"
	"testing"
	"time"
)

func TestStartTargetProbe(t *testing.T) {
	f, err := exec.LookPath("false")
	if err != nil {
		t.Skip("false not found")
	}
	if err := startTarget(f, nil, 2*time.Second); err == nil {
		t.Fatal("启动即退出应视为失败")
	}
	s, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not found")
	}
	if err := startTarget(s, []string{"1"}, 100*time.Millisecond); err != nil {
		t.Fatalf("存活过观察期应视为成功：%v", err)
	}
}

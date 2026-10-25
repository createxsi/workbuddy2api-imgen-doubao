//go:build !windows

package updater

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

func setHiddenWindow(*exec.Cmd) {}

func launchCleanup(string, string, int) {}

func waitForProcessExit(pid int, timeout time.Duration) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return os.ErrDeadlineExceeded
}

// isContainer 常见容器运行时的标志。
func isContainer() bool {
	if c := strings.ToLower(os.Getenv("container")); c != "" {
		return true
	}
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		return true
	}
	for _, p := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	if b, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		s := string(b)
		for _, k := range []string{"docker", "kubepods", "containerd", "libpod", "lxc"} {
			if strings.Contains(s, k) {
				return true
			}
		}
	}
	return false
}

// supervisor 返回托管本进程的服务管理器名称；未托管返回 ""。
// 被托管时原地替换会与管理器的重启/文件管理冲突，只提示手动更新。
func supervisor() string {
	switch {
	case os.Getenv("INVOCATION_ID") != "" || os.Getenv("NOTIFY_SOCKET") != "":
		return "systemd"
	case os.Getenv("RC_SVCNAME") != "":
		return "OpenRC"
	case os.Getenv("SUPERVISOR_ENABLED") != "":
		return "supervisord"
	case os.Getenv("pm_id") != "":
		return "pm2"
	}
	return ""
}

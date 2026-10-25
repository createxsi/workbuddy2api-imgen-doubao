//go:build windows

package updater

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
	"unsafe"
)

const (
	createNoWindow        = 0x08000000
	errorInvalidParameter = syscall.Errno(87)
)

var procProcessIdToSessionId = syscall.NewLazyDLL("kernel32.dll").NewProc("ProcessIdToSessionId")

func setHiddenWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

func waitForProcessExit(pid int, timeout time.Duration) error {
	h, err := syscall.OpenProcess(syscall.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		if errors.Is(err, errorInvalidParameter) {
			return nil // 进程已不存在
		}
		return err
	}
	defer syscall.CloseHandle(h)
	ms := uint32(timeout / time.Millisecond)
	ev, err := syscall.WaitForSingleObject(h, ms)
	if err != nil {
		return err
	}
	if ev == syscall.WAIT_OBJECT_0 {
		return nil
	}
	return os.ErrDeadlineExceeded
}

// launchCleanup 更新进程无法删除正在运行的自身：交给新程序在本进程退出后删除。
func launchCleanup(executable, p string, pid int) {
	if executable == "" || p == "" {
		return
	}
	cmd := exec.Command(executable, cleanupArg, strconv.Itoa(pid), p)
	setHiddenWindow(cmd)
	if cmd.Start() == nil {
		_ = cmd.Process.Release()
	}
}

func isContainer() bool { return false }

// supervisor Windows 服务（以及计划任务等以 SYSTEM 运行的托管）位于会话 0；
// 交互登录的用户进程从不在会话 0。
func supervisor() string {
	var session uint32
	r, _, _ := procProcessIdToSessionId.Call(uintptr(os.Getpid()), uintptr(unsafe.Pointer(&session)))
	if r != 0 && session == 0 {
		return "Windows 服务"
	}
	return ""
}

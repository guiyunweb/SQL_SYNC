//go:build !windows

package main

import (
	"errors"
	"os/exec"
	"os/signal"
	"syscall"
)

// startDetached 以新会话（setsid）启动子进程：脱离 GUI 所在的终端/桌面会话，
// 会话结束时不会收到 SIGHUP。
func startDetached(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	return cmd.Start()
}

// processAlive 判断进程是否存在（signal 0 只做检查不发送）。
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func ignoreHangup() { signal.Ignore(syscall.SIGHUP) }

func keepAwake() {}

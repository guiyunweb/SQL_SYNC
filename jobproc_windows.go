//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

const (
	createNewProcessGroup  = 0x00000200
	detachedProcess        = 0x00000008
	createBreakawayFromJob = 0x01000000
	createNoWindow         = 0x08000000
	stillActive            = 259
)

// startDetached 以独立进程启动：不继承控制台、脱离父进程所在作业对象，
// 关闭 GUI 窗口或断开远程桌面（非注销）后继续运行。
// 部分环境的作业对象不允许 breakaway，此时退回不带该标志重试。
func startDetached(cmd *exec.Cmd) error {
	base := uint32(createNewProcessGroup | detachedProcess | createNoWindow)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: base | createBreakawayFromJob, HideWindow: true}
	if err := cmd.Start(); err == nil {
		return nil
	}
	cmd2 := exec.Command(cmd.Path, cmd.Args[1:]...)
	cmd2.Env, cmd2.Dir = cmd.Env, cmd.Dir
	cmd2.SysProcAttr = &syscall.SysProcAttr{CreationFlags: base, HideWindow: true}
	if err := cmd2.Start(); err != nil {
		return err
	}
	*cmd = *cmd2
	return nil
}

// processAlive 通过进程退出码判断是否仍在运行。
func processAlive(pid int) bool {
	const processQueryLimitedInformation = 0x1000
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}

func ignoreHangup() {}

// keepAwake 任务期间阻止系统进入睡眠（ES_CONTINUOUS | ES_SYSTEM_REQUIRED）。
func keepAwake() {
	k32 := syscall.NewLazyDLL("kernel32.dll")
	k32.NewProc("SetThreadExecutionState").Call(uintptr(0x80000000 | 0x00000001))
}

//go:build windows

package main

import (
	"os"
	"syscall"
)

// enableCLIOutput 供 -H windowsgui（GUI 子系统）构建使用：
// 以命令行参数启动时把自身附加到父进程控制台并重定向标准输出，
// 否则 "sqlsync backup ..." 在 cmd/PowerShell 中看不到任何输出。
func enableCLIOutput() {
	if len(os.Args) < 2 {
		return // 双击启动 GUI，无需控制台
	}
	k32 := syscall.NewLazyDLL("kernel32.dll")
	// ATTACH_PARENT_PROCESS = -1
	r, _, _ := k32.NewProc("AttachConsole").Call(^uintptr(0))
	if r == 0 {
		return // 无父控制台（如资源管理器启动）
	}
	for _, name := range []struct {
		dev string
		out **os.File
	}{{"CONOUT$", &os.Stdout}, {"CONOUT$", &os.Stderr}} {
		h, err := syscall.Open(name.dev, syscall.O_WRONLY, 0)
		if err != nil {
			return
		}
		*name.out = os.NewFile(uintptr(h), name.dev)
	}
}

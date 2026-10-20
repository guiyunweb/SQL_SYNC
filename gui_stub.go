//go:build !cgo

package main

import (
	"fmt"
	"os"
)

// runGUI 纯 Go（CGO_ENABLED=0）构建不包含 Fyne 桌面界面，
// 提示后转为打印 CLI 用法。
func runGUI() {
	fmt.Println("此构建未启用 cgo，不包含桌面 GUI。")
	fmt.Println("如需 GUI 请使用带 GUI 的发行包；命令行用法：")
	fmt.Println()
	printUsage()
	os.Exit(0)
}

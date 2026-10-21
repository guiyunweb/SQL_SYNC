//go:build !windows

package main

// workAreaSize 非 Windows 平台不探测屏幕，返回 0 表示使用默认窗口尺寸。
func workAreaSize() (float32, float32) { return 0, 0 }

//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// workAreaSize 返回主显示器工作区（去掉任务栏）的逻辑尺寸（96 DPI 单位），失败返回 0。
func workAreaSize() (float32, float32) {
	user32 := syscall.NewLazyDLL("user32.dll")
	var rc struct{ Left, Top, Right, Bottom int32 }
	const spiGetWorkArea = 0x0030
	if r, _, _ := user32.NewProc("SystemParametersInfoW").Call(spiGetWorkArea, 0, uintptr(unsafe.Pointer(&rc)), 0); r == 0 {
		return 0, 0
	}
	dpi := uintptr(96)
	if p := user32.NewProc("GetDpiForSystem"); p.Find() == nil { // Win10+
		if d, _, _ := p.Call(); d > 0 {
			dpi = d
		}
	}
	scale := float32(dpi) / 96
	return float32(rc.Right-rc.Left) / scale, float32(rc.Bottom-rc.Top) / scale
}

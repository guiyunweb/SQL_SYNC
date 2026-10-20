package main

import (
	"image/png"
	"os"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"

	"sqlsync/internal/progress"

	"time"
)

// TestGUIRenders 用 Fyne 的离屏测试驱动真正渲染一遍主界面并截图，
// 验证布局代码没有 panic、控件正常生成，不依赖操作系统窗口/截屏权限。
func TestGUIRenders(t *testing.T) {
	a := test.NewApp()
	g := &guiApp{app: a, taskRows: map[string]*taskRow{}}
	g.win = a.NewWindow("SQLSync —— 离屏渲染测试")
	content := g.buildUI()
	g.win.SetContent(content)
	g.win.Resize(fyne.NewSize(1200, 820))

	// 模拟真实进度事件（验证进度上报 → UI 更新链路）
	evs := []progress.Event{
		{Kind: progress.KindLog, When: time.Now(), Message: "测试日志行"},
		{Kind: progress.KindTaskStart, When: time.Now(), Task: "mydb/orders#1/4", Message: "开始导出", Total: 75000},
		{Kind: progress.KindTaskUpdate, When: time.Now(), Task: "mydb/orders#1/4", Message: "导出中", Done: 30000, Total: 75000},
		{Kind: progress.KindTaskStart, When: time.Now(), Task: "mydb/kv_no_pk", Message: "开始导出", Total: 5000},
		{Kind: progress.KindTaskDone, When: time.Now(), Task: "mydb/kv_no_pk", Message: "完成，5000 行", Done: 5000, Total: 5000},
		{Kind: progress.KindSummary, When: time.Now(), Task: "mydb", Message: "数据文件完成 1/6", Done: 1, Total: 6},
	}
	for _, e := range evs {
		g.applyEvent(e)
	}

	// 模拟数据库勾选列表（备份页）
	g.bakDBs.Options = []string{"carlsberg_order_auto_management", "carlsberg_d365_auth", "carlsberg_bakdemo", "shop_archive"}
	g.bakDBs.Selected = []string{"carlsberg_bakdemo", "carlsberg_d365_auth"}
	g.bakDBs.Refresh()

	img := g.win.Canvas().Capture()
	if img.Bounds().Dx() < 500 || img.Bounds().Dy() < 400 {
		t.Fatalf("渲染尺寸异常: %v", img.Bounds())
	}
	f, err := os.Create("/tmp/gui_render_test.png")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	t.Log("备份页渲染截图已写入 /tmp/gui_render_test.png", img.Bounds())

	// 切到恢复页再截一张
	g.rstDBs.Options = []string{"carlsberg_bakdemo", "carlsberg_order_auto_management"}
	g.rstDBs.Selected = []string{"carlsberg_bakdemo"}
	g.rstDBs.Refresh()
	g.tabs.SelectIndex(1)
	img2 := g.win.Canvas().Capture()
	f2, err := os.Create("/tmp/gui_render_restore.png")
	if err != nil {
		t.Fatal(err)
	}
	defer f2.Close()
	if err := png.Encode(f2, img2); err != nil {
		t.Fatal(err)
	}
	t.Log("恢复页渲染截图已写入 /tmp/gui_render_restore.png", img2.Bounds())
}

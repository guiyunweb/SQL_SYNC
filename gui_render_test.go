//go:build cgo

package main

import (
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/theme"

	"sqlsync/internal/progress"
)

// fixedVariant 让离屏截图固定使用浅色或深色变体。
type fixedVariant struct {
	*sqlsyncTheme
	v fyne.ThemeVariant
}

func (f fixedVariant) Color(n fyne.ThemeColorName, _ fyne.ThemeVariant) color.Color {
	return f.sqlsyncTheme.Color(n, f.v)
}

// TestGUIRenders 用 Fyne 的离屏测试驱动真正渲染主界面并截图：
// 覆盖浅色/深色、宽屏左右分栏、窄屏上下堆叠，验证布局不 panic、控件正常生成。
// 截图输出到 SQLSYNC_SHOT_DIR（默认系统临时目录）。
func TestGUIRenders(t *testing.T) {
	outDir := os.Getenv("SQLSYNC_SHOT_DIR")
	if outDir == "" {
		outDir = os.TempDir()
	}
	cases := []struct {
		name    string
		variant fyne.ThemeVariant
		size    fyne.Size
		tab     int
	}{
		{"backup_light_1280", theme.VariantLight, fyne.NewSize(1280, 820), 0},
		{"restore_dark_1280", theme.VariantDark, fyne.NewSize(1280, 820), 1},
		{"backup_light_900", theme.VariantLight, fyne.NewSize(900, 720), 0},
		{"backup_light_1024x640", theme.VariantLight, fyne.NewSize(1024, 640), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := test.NewApp()
			defer a.Quit()
			a.Settings().SetTheme(fixedVariant{newTheme(), c.variant})
			g := newGUIApp(a)
			g.win = a.NewWindow("SQLSync —— 离屏渲染测试")
			g.win.SetContent(g.buildUI())
			g.win.Resize(c.size)

			// 模拟运行中的任务（验证进度上报 → UI 更新链路）
			g.jobDir, g.jobKind, g.jobStarted = "fake", "backup", time.Now().Add(-95*time.Second)
			g.setRunningUI(true)
			now := time.Now()
			g.applyEvents([]progress.Event{
				{Kind: progress.KindLog, When: now, Message: "待备份数据库: carlsberg_bakdemo, carlsberg_d365_auth"},
				{Kind: progress.KindLog, When: now, Message: "数据导出任务 18 个，预估 2,350,000 行，8 个 worker 并行"},
				{Kind: progress.KindTaskStart, When: now, Task: "carlsberg_bakdemo/orders#1/4", Message: "开始导出", Total: 500000},
				{Kind: progress.KindTaskUpdate, When: now, Task: "carlsberg_bakdemo/orders#1/4", Message: "导出中", Done: 312000, Total: 500000},
				{Kind: progress.KindTaskStart, When: now, Task: "carlsberg_bakdemo/order_items_with_a_really_long_table_name", Message: "开始导出", Total: 800000},
				{Kind: progress.KindTaskUpdate, When: now, Task: "carlsberg_bakdemo/order_items_with_a_really_long_table_name", Message: "导出中", Done: 120000, Total: 800000},
				{Kind: progress.KindTaskStart, When: now, Task: "carlsberg_d365_auth/kv_no_pk", Message: "开始导出", Total: 5000},
				{Kind: progress.KindTaskDone, When: now, Task: "carlsberg_d365_auth/kv_no_pk", Message: "完成，5,000 行", Done: 5000, Total: 5000},
				{Kind: progress.KindOverall, When: now, Done: 1_034_000, Total: 2_350_000,
					Message: "已导出 1,034,000 / 约 2,350,000 行 · 已写入 86.3 MB · 41,200 行/s"},
			})
			g.updateTimes()

			g.bakDBs.Options = []string{"carlsberg_order_auto_management", "carlsberg_d365_auth", "carlsberg_bakdemo", "shop_archive"}
			g.bakDBs.Selected = []string{"carlsberg_bakdemo", "carlsberg_d365_auth"}
			g.bakDBs.Refresh()
			g.bakCount()
			g.rstDBs.Options = []string{"carlsberg_bakdemo", "carlsberg_order_auto_management"}
			g.rstDBs.Selected = []string{"carlsberg_bakdemo"}
			g.rstDBs.Refresh()
			g.rstCount()
			g.tabs.SelectIndex(c.tab)

			img := g.win.Canvas().Capture()
			if img.Bounds().Dx() < 500 || img.Bounds().Dy() < 400 {
				t.Fatalf("渲染尺寸异常: %v", img.Bounds())
			}
			path := filepath.Join(outDir, "gui_"+c.name+".png")
			f, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if err := png.Encode(f, img); err != nil {
				t.Fatal(err)
			}
			t.Logf("截图: %s %v", path, img.Bounds())
		})
	}
}

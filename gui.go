//go:build cgo

package main

import (
	_ "embed"
	"fmt"
	"image/color"
	"os/exec"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/storage"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"sqlsync/internal/backup"
	"sqlsync/internal/dbconn"
	"sqlsync/internal/progress"
	"sqlsync/internal/restore"
)

//go:embed assets/icon.png
var iconBytes []byte

// guiApp 集中管理 GUI 状态。
type guiApp struct {
	app fyne.App
	win fyne.Window

	// 连接参数控件
	host, port, user, pass, sslCA *widget.Entry
	sslMode                       *widget.Select

	// 后台任务
	jobDir     string    // 当前跟踪的任务目录，空表示空闲
	jobStarted time.Time // 任务开始时间（用于耗时/剩余时间）
	jobKind    string
	startBtns  []*widget.Button
	cancelBtns []*widget.Button

	// 进度展示
	overallBar   *widget.ProgressBar
	overallPct   *canvas.Text
	overallLabel *widget.Label // 引擎上报的进度描述（行数/速度/大小）
	timeLabel    *widget.Label // 已用时间 · 预计剩余
	stateChip    *canvas.Text  // 头部右侧的运行状态
	header       *fyne.Container
	logArea      *widget.Entry
	logLines     []string
	tasksBox     *fyne.Container
	taskRows     map[string]*taskRow
	rowOrder     []string

	bakDBs *widget.CheckGroup // 备份页：服务器数据库勾选列表
	rstDBs *widget.CheckGroup // 恢复页：备份包中的数据库勾选列表
	tabs   *container.AppTabs

	bakCount, rstCount func() // 刷新两个勾选列表的“已选 x / y”计数

	status *widget.Label
}

type taskRow struct {
	label *widget.Label
	bar   *widget.ProgressBar
	done  bool
	box   *fyne.Container
}

const (
	maxVisibleRows = 40   // 界面上最多保留的任务行数，超出则移除最早完成的任务
	maxLogLines    = 3000 // 日志窗口保留的行数
	wideLayoutMin  = 980  // 窗口宽度不小于该值时左右分栏，否则上下堆叠
)

func runGUI() {
	a := app.NewWithID("com.sqlsync.gui")
	a.Settings().SetTheme(newTheme())
	if len(iconBytes) > 0 {
		a.SetIcon(fyne.NewStaticResource("icon.png", iconBytes))
	}
	g := newGUIApp(a)
	g.win = a.NewWindow("SQLSync —— MySQL 备份 / 恢复")
	g.win.SetContent(g.buildUI())
	g.win.Resize(initialWindowSize())
	g.win.CenterOnScreen()
	g.win.SetCloseIntercept(g.confirmClose)
	g.resumeJobs()
	g.win.ShowAndRun()
}

func newGUIApp(a fyne.App) *guiApp {
	return &guiApp{app: a, taskRows: map[string]*taskRow{}}
}

// initialWindowSize 按屏幕工作区自适应：大屏 1280×840，小屏取工作区的 92%。
func initialWindowSize() fyne.Size {
	w, h := float32(1280), float32(840)
	if sw, sh := workAreaSize(); sw > 0 && sh > 0 {
		w = min(w, sw*0.92)
		h = min(h, sh*0.92)
	}
	return fyne.NewSize(max(w, 760), max(h, 560))
}

// buildUI 组装主界面（独立出来便于离屏渲染测试）。
func (g *guiApp) buildUI() fyne.CanvasObject {
	connPanel := g.buildConnPanel()
	g.applyPrefs() // 控件创建后填充上次输入

	g.tabs = container.NewAppTabs(
		container.NewTabItemWithIcon("备份", theme.DocumentSaveIcon(), g.buildBackupTab()),
		container.NewTabItemWithIcon("恢复", theme.ViewRestoreIcon(), g.buildRestoreTab()),
	)
	g.tabs.SetTabLocation(container.TabLocationTop)

	left := container.NewBorder(connPanel, nil, nil, nil, g.tabs)
	overall, details := g.buildMonitorPanel()
	body := container.New(&responsiveSplit{ratio: 0.55}, left, overall, details)

	g.status = widget.NewLabel("就绪")
	g.status.Truncation = fyne.TextTruncateEllipsis
	statusBar := container.NewBorder(nil, nil, widget.NewIcon(theme.InfoIcon()), nil, g.status)

	return container.NewBorder(g.buildHeader(), statusBar, nil, nil, container.NewPadded(body))
}

// responsiveSplit 自适应布局，子对象依次为：参数区、总体进度卡片、任务/日志区。
//   - 宽窗口：参数区在左；右栏上方总体进度、下方任务与日志
//   - 窄窗口：参数区在上、总体进度卡片在下，任务/日志区隐藏（状态栏仍显示最新进度）
type responsiveSplit struct{ ratio float32 }

func (r *responsiveSplit) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	if len(objs) < 3 {
		return
	}
	a, ov, det := objs[0], objs[1], objs[2]
	pad := theme.Padding() * 2
	ovh := ov.MinSize().Height
	if size.Width >= wideLayoutMin {
		if !det.Visible() {
			det.Show()
		}
		lw := max((size.Width-pad)*r.ratio, a.MinSize().Width)
		lw = min(lw, size.Width-pad-max(ov.MinSize().Width, det.MinSize().Width))
		rx, rw := lw+pad, size.Width-lw-pad
		a.Move(fyne.NewPos(0, 0))
		a.Resize(fyne.NewSize(lw, size.Height))
		ov.Move(fyne.NewPos(rx, 0))
		ov.Resize(fyne.NewSize(rw, ovh))
		det.Move(fyne.NewPos(rx, ovh+pad))
		det.Resize(fyne.NewSize(rw, max(size.Height-ovh-pad, det.MinSize().Height)))
		return
	}
	if det.Visible() {
		det.Hide()
	}
	ah := max(size.Height-ovh-pad, a.MinSize().Height)
	a.Move(fyne.NewPos(0, 0))
	a.Resize(fyne.NewSize(size.Width, ah))
	ov.Move(fyne.NewPos(0, ah+pad))
	ov.Resize(fyne.NewSize(size.Width, ovh))
}

// MinSize 宽度按窄排布取最宽者、高度按宽排布取较高者，
// 保证 1366×768 等小屏、远程桌面小窗口也能把窗口缩到屏幕内。
func (r *responsiveSplit) MinSize(objs []fyne.CanvasObject) fyne.Size {
	if len(objs) < 3 {
		return fyne.NewSize(0, 0)
	}
	a, ov, det := objs[0].MinSize(), objs[1].MinSize(), objs[2].MinSize()
	return fyne.NewSize(max(a.Width, ov.Width, det.Width),
		max(a.Height, ov.Height+det.Height+theme.Padding()*2, a.Height+ov.Height+theme.Padding()*2))
}

// buildHeader 顶部渐变品牌横幅，右侧显示运行状态。
func (g *guiApp) buildHeader() fyne.CanvasObject {
	bg := canvas.NewHorizontalGradient(brandPrimary, brandAccent)

	title := canvas.NewText("SQLSync", color.White)
	title.TextSize = 22
	title.TextStyle = fyne.TextStyle{Bold: true}
	sub := canvas.NewText("MySQL 并行备份 / 恢复", color.NRGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xCC})
	sub.TextSize = 13

	g.stateChip = canvas.NewText("● 空闲", color.White)
	g.stateChip.TextSize = 13
	g.stateChip.TextStyle = fyne.TextStyle{Bold: true}

	items := []fyne.CanvasObject{title, container.NewCenter(sub)}
	if len(iconBytes) > 0 {
		logo := canvas.NewImageFromResource(fyne.NewStaticResource("icon.png", iconBytes))
		logo.FillMode = canvas.ImageFillContain
		logo.SetMinSize(fyne.NewSize(30, 30))
		items = append([]fyne.CanvasObject{logo}, items...)
	}
	items = append(items, layout.NewSpacer(), container.NewCenter(g.stateChip))
	g.header = container.NewHBox(items...)
	return container.NewStack(bg, container.NewPadded(container.NewPadded(g.header)))
}

// buildMonitorPanel 运行监控：返回总体进度卡片，以及任务列表 + 日志区。
func (g *guiApp) buildMonitorPanel() (overall, details fyne.CanvasObject) {
	g.overallPct = canvas.NewText("0%", brandPrimary)
	g.overallPct.TextSize = 34
	g.overallPct.TextStyle = fyne.TextStyle{Bold: true}
	g.overallLabel = widget.NewLabel("空闲")
	g.overallLabel.Truncation = fyne.TextTruncateEllipsis
	g.timeLabel = widget.NewLabel("等待开始")
	g.timeLabel.Importance = widget.LowImportance
	g.timeLabel.Truncation = fyne.TextTruncateEllipsis
	g.overallBar = widget.NewProgressBar()
	g.overallBar.Max = 100
	g.overallBar.TextFormatter = func() string { return "" } // 百分比已用大号数字显示

	// 百分比放进固定宽度的格子：文字由 0% 变成 100% 时不会盖住进度条
	pctBox := container.NewGridWrap(fyne.NewSize(110, g.overallPct.MinSize().Height), g.overallPct)
	overall = widget.NewCard("", "", container.NewVBox(
		sectionTitle(theme.MediaFastForwardIcon(), "总体进度"),
		container.NewBorder(nil, nil, pctBox, nil,
			container.NewVBox(layout.NewSpacer(), g.overallBar, layout.NewSpacer())),
		g.overallLabel,
		g.timeLabel,
	))

	g.tasksBox = container.NewVBox()
	tasks := widget.NewCard("", "", container.NewBorder(
		sectionTitle(theme.ListIcon(), "正在执行的任务"), nil, nil, nil,
		container.NewVScroll(g.tasksBox)))

	g.logArea = widget.NewMultiLineEntry()
	g.logArea.Wrapping = fyne.TextWrapWord
	g.logArea.TextStyle = fyne.TextStyle{Monospace: true}
	g.logArea.SetMinRowsVisible(4)
	clearLog := widget.NewButtonWithIcon("", theme.ContentClearIcon(), func() {
		g.logLines = nil
		g.logArea.SetText("")
	})
	clearLog.Importance = widget.LowImportance
	logs := widget.NewCard("", "", container.NewBorder(
		container.NewBorder(nil, nil, sectionTitle(theme.DocumentIcon(), "日志"), clearLog),
		nil, nil, nil, g.logArea))

	split := container.NewVSplit(tasks, logs)
	split.Offset = 0.5
	return overall, split
}

// buildConnPanel 顶部的连接配置卡片。
func (g *guiApp) buildConnPanel() fyne.CanvasObject {
	g.host = widget.NewEntry()
	g.host.SetPlaceHolder("主机，如 116.176.114.10")
	g.port = widget.NewEntry()
	g.port.SetText("3306")
	g.user = widget.NewEntry()
	g.user.SetText("root")
	g.pass = widget.NewPasswordEntry()
	g.pass.SetPlaceHolder("密码")
	g.sslMode = widget.NewSelect(
		[]string{"disable", "preferred", "skip-verify", "verify-ca", "verify-full"},
		nil)
	g.sslMode.SetSelected("preferred")
	g.sslCA = widget.NewEntry()
	g.sslCA.SetPlaceHolder("CA 证书（verify-ca / verify-full）")

	caBtn := widget.NewButtonWithIcon("", theme.FolderOpenIcon(), func() {
		dialog.ShowFileOpen(func(f fyne.URIReadCloser, err error) {
			if f != nil {
				g.sslCA.SetText(f.URI().Path())
				f.Close()
			}
		}, g.win)
	})
	caBtn.Importance = widget.LowImportance

	testBtn := widget.NewButtonWithIcon("测试连接", theme.ConfirmIcon(), func() { g.testConnection() })
	testBtn.Importance = widget.SuccessImportance

	// 主机与端口同行、用户与密码同行，压缩纵向空间
	portBox := container.NewGridWrap(fyne.NewSize(84, g.port.MinSize().Height), g.port)
	server := container.NewBorder(nil, nil, nil,
		container.NewHBox(widget.NewLabel("端口"), portBox), g.host)
	account := container.NewGridWithColumns(2, g.user, g.pass)
	ssl := container.NewBorder(nil, nil, g.sslMode, caBtn, g.sslCA)

	form := widget.NewForm(
		widget.NewFormItem("服务器", server),
		widget.NewFormItem("账号", account),
		widget.NewFormItem("SSL", ssl),
	)
	return widget.NewCard("", "", container.NewBorder(
		container.NewBorder(nil, nil, sectionTitle(theme.ComputerIcon(), "数据库连接"), testBtn),
		nil, nil, nil, form))
}

// sectionTitle 带图标的粗体分组标题。
func sectionTitle(icon fyne.Resource, text string) fyne.CanvasObject {
	return container.NewHBox(widget.NewIcon(icon),
		widget.NewLabelWithStyle(text, fyne.TextAlignLeading, fyne.TextStyle{Bold: true}))
}

// actionBar 页签底部固定的操作按钮栏，靠右对齐。
func actionBar(hint fyne.CanvasObject, buttons ...fyne.CanvasObject) fyne.CanvasObject {
	objs := append([]fyne.CanvasObject{hint, layout.NewSpacer()}, buttons...)
	return container.NewVBox(widget.NewSeparator(), container.NewHBox(objs...))
}

// hint 灰色小字说明。
func hint(text string) *widget.Label {
	l := widget.NewLabel(text)
	l.Importance = widget.LowImportance
	l.Wrapping = fyne.TextWrapWord
	return l
}

// dbPicker 数据库勾选卡片：工具按钮 + 已选计数 + 勾选列表。
// 列表不再单独滚动，随页签内容一起滚动（Fyne 嵌套滚动容器裁剪不可靠，会画到按钮栏上）。
// 返回的 refresh 用于代码直接改写 Options/Selected 之后同步计数。
func dbPicker(cg *widget.CheckGroup, emptyHint string, tools ...fyne.CanvasObject) (fyne.CanvasObject, func()) {
	count := widget.NewLabel("")
	count.Importance = widget.LowImportance
	update := func() {
		switch {
		case len(cg.Options) == 0:
			count.SetText("列表为空")
		case len(cg.Selected) == 0:
			count.SetText(fmt.Sprintf("共 %d 个 · 未勾选（%s）", len(cg.Options), emptyHint))
		default:
			count.SetText(fmt.Sprintf("已选 %d / %d", len(cg.Selected), len(cg.Options)))
		}
	}
	cg.OnChanged = func([]string) { update() }
	update()

	allBtn := widget.NewButtonWithIcon("全选", theme.CheckButtonCheckedIcon(), func() {
		cg.Selected = append([]string(nil), cg.Options...)
		cg.Refresh()
		update()
	})
	clearBtn := widget.NewButtonWithIcon("清空", theme.CheckButtonIcon(), func() {
		cg.Selected = nil
		cg.Refresh()
		update()
	})
	allBtn.Importance = widget.LowImportance
	clearBtn.Importance = widget.LowImportance

	toolbar := container.NewHBox(append(tools, layout.NewSpacer(), allBtn, clearBtn)...)
	return widget.NewCard("", "", container.NewVBox(
		container.NewBorder(nil, nil, sectionTitle(theme.StorageIcon(), "数据库选择"), count),
		toolbar, cg)), update
}

// relayoutAfter 包装计数刷新：勾选列表的选项增减会改变卡片高度，
// 而子控件变高不会自动通知父容器，需要让页签内容与滚动容器重新布局。
func relayoutAfter(update func(), content *fyne.Container, scroll *container.Scroll) func() {
	return func() {
		update()
		content.Refresh()
		scroll.Refresh()
	}
}

// numEntry 带默认值的数字输入框。
func numEntry(def string) *widget.Entry {
	e := widget.NewEntry()
	e.SetText(def)
	return e
}

// compressLevels 压缩级别选项 → gzip 级别。
var compressLevels = map[string]int{"最快（推荐）": 1, "均衡": 6, "最小体积": 9}

// buildBackupTab 备份页签。
func (g *guiApp) buildBackupTab() fyne.CanvasObject {
	g.bakDBs = widget.NewCheckGroup(nil, nil)
	loadBtn := widget.NewButtonWithIcon("从服务器获取", theme.ViewRefreshIcon(), func() { g.loadServerDatabases() })
	picker, updateCount := dbPicker(g.bakDBs, "将备份全部非系统库", loadBtn)

	tables := widget.NewEntry()
	tables.SetPlaceHolder("逗号分隔的表名，留空 = 全部表")
	outDir := widget.NewEntry()
	outDir.SetText("./backups")
	dirBtn := widget.NewButtonWithIcon("", theme.FolderOpenIcon(), func() {
		dialog.ShowFolderOpen(func(f fyne.ListableURI, err error) {
			if f != nil {
				outDir.SetText(f.Path())
			}
		}, g.win)
	})
	workers := numEntry("8")
	shardMin := numEntry("500000")
	shardMax := numEntry("16")
	rowsPerInsert := numEntry("500")
	level := widget.NewSelect([]string{"最快（推荐）", "均衡", "最小体积"}, nil)
	level.SetSelected("最快（推荐）")

	startBtn := widget.NewButtonWithIcon("开始备份", theme.MediaPlayIcon(), func() {
		cfg, err := g.connConfig()
		if err != nil {
			dialog.ShowError(err, g.win)
			return
		}
		g.launch(jobSpec{Kind: "backup", Conn: cfg, Backup: &backup.Options{
			OutDir:        strings.TrimSpace(outDir.Text),
			Databases:     append([]string(nil), g.bakDBs.Selected...),
			Tables:        splitCSV(tables.Text),
			Workers:       atoiDefault(workers.Text, 8),
			ShardMinRows:  int64(atoiDefault(shardMin.Text, 500_000)),
			ShardMax:      atoiDefault(shardMax.Text, 16),
			RowsPerInsert: atoiDefault(rowsPerInsert.Text, 500),
			CompressLevel: compressLevels[level.Selected],
		}})
	})
	startBtn.Importance = widget.HighImportance
	cancelBtn := widget.NewButtonWithIcon("取消", theme.CancelIcon(), func() { g.cancelTask() })
	cancelBtn.Importance = widget.DangerImportance
	cancelBtn.Disable()
	g.startBtns = append(g.startBtns, startBtn)
	g.cancelBtns = append(g.cancelBtns, cancelBtn)

	// 常用项（库选择、输出位置）在上；调优参数收进默认折叠的“高级参数”
	output := widget.NewCard("", "", container.NewVBox(
		sectionTitle(theme.FolderIcon(), "输出"),
		widget.NewForm(
			widget.NewFormItem("输出目录", container.NewBorder(nil, nil, nil, dirBtn, outDir)),
			widget.NewFormItem("压缩", level),
		),
		hint("生成单个 sqlsync_<库名>_<时间>.tar.gz，可直接用 tar / 7-Zip 解开查看")))
	advanced := widget.NewAccordion(widget.NewAccordionItem("高级参数", container.NewVBox(
		widget.NewForm(widget.NewFormItem("表过滤", tables)),
		container.NewGridWithColumns(2,
			widget.NewForm(
				widget.NewFormItem("并行数", workers),
				widget.NewFormItem("分片行数", shardMin),
			),
			widget.NewForm(
				widget.NewFormItem("最大分片", shardMax),
				widget.NewFormItem("每 INSERT 行数", rowsPerInsert),
			),
		),
	)))

	content := container.NewVBox(picker, output, advanced)
	body := container.NewVScroll(content)
	g.bakCount = relayoutAfter(updateCount, content, body)
	return container.NewBorder(nil, actionBar(layout.NewSpacer(), cancelBtn, startBtn), nil, nil, body)
}

// loadServerDatabases 连接服务器拉取非系统库列表填充到勾选框。
func (g *guiApp) loadServerDatabases() {
	cfg, err := g.connConfig()
	if err != nil {
		dialog.ShowError(err, g.win)
		return
	}
	g.appendLog("正在获取数据库列表…")
	go func() {
		dbs, err := dbconn.ListDatabases(cfg)
		fyne.Do(func() {
			if err != nil {
				dialog.ShowError(fmt.Errorf("获取数据库列表失败: %w", err), g.win)
				g.appendLog("获取数据库列表失败: " + err.Error())
				return
			}
			g.bakDBs.Options = dbs
			g.bakDBs.Selected = nil
			g.bakDBs.Refresh()
			g.bakCount()
			g.appendLog(fmt.Sprintf("已获取 %d 个数据库", len(dbs)))
			g.savePrefs()
		})
	}()
}

// buildRestoreTab 恢复页签。
func (g *guiApp) buildRestoreTab() fyne.CanvasObject {
	pkg := widget.NewEntry()
	pkg.SetPlaceHolder("备份包 .tar.gz（也可填旧版备份目录）")

	g.rstDBs = widget.NewCheckGroup(nil, nil)

	scan := func() {
		p := strings.TrimSpace(pkg.Text)
		if p == "" {
			dialog.ShowError(fmt.Errorf("请先选择备份包"), g.win)
			return
		}
		names, err := restore.ListBackedUpDatabases(p)
		if err != nil {
			dialog.ShowError(fmt.Errorf("读取备份失败: %w", err), g.win)
			return
		}
		if len(names) == 0 {
			dialog.ShowInformation("提示", "备份中没有可恢复的数据库", g.win)
			return
		}
		g.rstDBs.Options = names
		g.rstDBs.Selected = nil
		g.rstDBs.Refresh()
		g.rstCount()
		g.appendLog(fmt.Sprintf("备份中包含 %d 个数据库: %s", len(names), strings.Join(names, ", ")))
	}
	fileBtn := widget.NewButtonWithIcon("", theme.FolderOpenIcon(), func() {
		d := dialog.NewFileOpen(func(f fyne.URIReadCloser, err error) {
			if f != nil {
				pkg.SetText(f.URI().Path())
				f.Close()
				scan()
			}
		}, g.win)
		d.SetFilter(storage.NewExtensionFileFilter([]string{".gz", ".tgz"}))
		d.Show()
	})
	scanBtn := widget.NewButtonWithIcon("读取备份包", theme.SearchIcon(), scan)
	picker, updateCount := dbPicker(g.rstDBs, "将恢复包内全部库", scanBtn)

	rename := widget.NewEntry()
	rename.SetPlaceHolder("如 old:new，多个逗号分隔")
	workers := numEntry("8")
	commitBatch := numEntry("200")
	createChk := widget.NewCheck("目标库不存在时自动创建", nil)
	createChk.SetChecked(true)
	recreateChk := widget.NewCheck("恢复前 DROP 目标库（危险）", nil)

	startBtn := widget.NewButtonWithIcon("开始恢复", theme.MediaPlayIcon(), func() {
		run := func() { g.doRestore(pkg, rename, workers, commitBatch, createChk.Checked, recreateChk.Checked) }
		if recreateChk.Checked {
			dialog.ShowConfirm("危险操作",
				"将先 DROP 目标库再恢复，现有数据会丢失。确定继续？", func(ok bool) {
					if ok {
						run()
					}
				}, g.win)
			return
		}
		run()
	})
	startBtn.Importance = widget.HighImportance
	cancelBtn := widget.NewButtonWithIcon("取消", theme.CancelIcon(), func() { g.cancelTask() })
	cancelBtn.Importance = widget.DangerImportance
	cancelBtn.Disable()
	g.startBtns = append(g.startBtns, startBtn)
	g.cancelBtns = append(g.cancelBtns, cancelBtn)

	source := widget.NewCard("", "", container.NewVBox(
		sectionTitle(theme.FileIcon(), "备份包"),
		widget.NewForm(
			widget.NewFormItem("文件", container.NewBorder(nil, nil, nil, fileBtn, pkg)),
			widget.NewFormItem("改名映射", rename),
		),
		container.NewGridWithColumns(2, createChk, recreateChk)))
	advanced := widget.NewAccordion(widget.NewAccordionItem("高级参数",
		container.NewGridWithColumns(2,
			widget.NewForm(widget.NewFormItem("并行数", workers)),
			widget.NewForm(widget.NewFormItem("批量提交", commitBatch)),
		)))

	// 勾选 DROP 时在操作栏显示醒目警示
	warn := widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	warn.Importance = widget.DangerImportance
	recreateChk.OnChanged = func(on bool) {
		if on {
			warn.SetText("⚠ 将先 DROP 目标库")
		} else {
			warn.SetText("")
		}
	}

	content := container.NewVBox(source, picker, advanced)
	body := container.NewVScroll(content)
	g.rstCount = relayoutAfter(updateCount, content, body)
	return container.NewBorder(nil, actionBar(warn, cancelBtn, startBtn), nil, nil, body)
}

func (g *guiApp) doRestore(pkg, rename, workers, commitBatch *widget.Entry, create, recreate bool) {
	cfg, err := g.connConfig()
	if err != nil {
		dialog.ShowError(err, g.win)
		return
	}
	if strings.TrimSpace(pkg.Text) == "" {
		dialog.ShowError(fmt.Errorf("请选择备份包"), g.win)
		return
	}
	renameMap := map[string]string{}
	for _, pair := range splitCSV(rename.Text) {
		parts := strings.SplitN(pair, ":", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			dialog.ShowError(fmt.Errorf("改名映射格式错误: %q，应为 old:new", pair), g.win)
			return
		}
		renameMap[parts[0]] = parts[1]
	}
	g.launch(jobSpec{Kind: "restore", Conn: cfg, Restore: &restore.Options{
		Dir:         strings.TrimSpace(pkg.Text),
		Databases:   append([]string(nil), g.rstDBs.Selected...),
		Rename:      renameMap,
		Create:      create,
		Recreate:    recreate,
		Workers:     atoiDefault(workers.Text, 8),
		CommitBatch: atoiDefault(commitBatch.Text, 200),
	}})
}

// connConfig 从表单构造连接配置。
func (g *guiApp) connConfig() (dbconn.Config, error) {
	port := atoiDefault(g.port.Text, 3306)
	if strings.TrimSpace(g.host.Text) == "" {
		return dbconn.Config{}, fmt.Errorf("请填写主机")
	}
	c := dbconn.Config{
		Host:     strings.TrimSpace(g.host.Text),
		Port:     port,
		User:     strings.TrimSpace(g.user.Text),
		Password: g.pass.Text,
		SSLMode:  g.sslMode.Selected,
		SSLCA:    strings.TrimSpace(g.sslCA.Text),
	}
	return c, nil
}

func (g *guiApp) testConnection() {
	cfg, err := g.connConfig()
	if err != nil {
		dialog.ShowError(err, g.win)
		return
	}
	g.status.SetText("正在连接 " + cfg.Host + " …")
	go func() {
		db, err := dbconn.Open(cfg)
		var ver string
		if err == nil {
			_ = db.QueryRow("SELECT VERSION()").Scan(&ver)
			db.Close()
		}
		fyne.Do(func() {
			if err != nil {
				g.status.SetText("连接失败")
				dialog.ShowError(err, g.win)
				return
			}
			g.savePrefs()
			g.status.SetText("连接成功，MySQL " + ver)
			dialog.ShowInformation("连接成功", "MySQL 版本: "+ver, g.win)
		})
	}()
}

// launch 以后台进程启动任务并开始跟踪进度。
func (g *guiApp) launch(spec jobSpec) {
	if g.jobDir != "" {
		dialog.ShowInformation("提示", "已有任务在运行，请等待完成或先取消", g.win)
		return
	}
	dir, cmd, err := startJob(spec, spec.Conn.Password)
	if err != nil {
		dialog.ShowError(err, g.win)
		return
	}
	go reap(cmd)
	g.savePrefs()
	g.attachJob(dir, false)
}

// reap 回收子进程，避免 Unix 下 GUI 存活期间出现僵尸进程；GUI 退出后子进程照常运行。
func reap(cmd *exec.Cmd) { _ = cmd.Wait() }

// resumeJobs 启动时接上仍在运行的后台任务，并展示期间已结束的任务结果。
func (g *guiApp) resumeJobs() {
	running, unseen := findJobs()
	for i := len(unseen) - 1; i >= 0; i-- { // 从旧到新
		st, err := readStatus(unseen[i])
		if err != nil {
			continue
		}
		g.appendLog(fmt.Sprintf("上次的%s任务（%s 开始）%s", st.title(),
			st.Started.Format("01-02 15:04"), describeResult(st)))
		markSeen(unseen[i])
		if i == 0 && running == "" {
			g.showResult(st)
		}
	}
	if running != "" {
		g.attachJob(running, true)
	}
}

func describeResult(st jobStatus) string {
	switch st.State {
	case "done":
		if st.Output != "" {
			return "已完成：" + st.Output
		}
		return "已完成"
	case "canceled":
		return "已取消"
	default:
		return "失败：" + st.Error
	}
}

// attachJob 跟踪一个后台任务：轮询事件文件刷新界面，结束后展示结果。
func (g *guiApp) attachJob(dir string, resumed bool) {
	st, _ := readStatus(dir)
	g.jobDir, g.jobKind, g.jobStarted = dir, st.Kind, st.Started
	if g.jobStarted.IsZero() {
		g.jobStarted = time.Now()
	}
	g.clearTasks()
	g.setRunningUI(true)
	g.setOverall(0, "启动中…")
	if resumed {
		g.appendLog(fmt.Sprintf("检测到后台%s任务仍在运行，已重新连接（%s 开始）", st.title(), g.jobStarted.Format("15:04:05")))
	} else {
		g.appendLog(fmt.Sprintf("%s任务已在后台进程启动，关闭窗口或断开远程桌面不会中断", st.title()))
	}

	tail := newJobTail(dir)
	go func() {
		tk := time.NewTicker(300 * time.Millisecond)
		defer tk.Stop()
		for range tk.C {
			evs := tail.poll()
			st, err := readStatus(dir)
			finished := err == nil && st.State != "running"
			if !finished && err == nil && st.PID > 0 && !processAlive(st.PID) {
				// 进程已不在但没写最终状态：再读一次确认后判为异常退出
				if st2, _ := readStatus(dir); st2.State == "running" {
					st.State, st.Error, st.Finished = "failed", "后台进程意外退出", time.Now()
					_ = writeStatus(dir, st)
				} else {
					st = st2
				}
				finished = true
			}
			if finished {
				evs = append(evs, tail.poll()...)
			}
			fyne.Do(func() {
				g.applyEvents(evs)
				g.updateTimes()
				if finished {
					g.finishJob(st)
				}
			})
			if finished {
				return
			}
		}
	}()
}

func (g *guiApp) finishJob(st jobStatus) {
	if g.jobDir == "" {
		return
	}
	markSeen(g.jobDir)
	g.jobDir = ""
	g.setRunningUI(false)
	elapsed := st.Finished.Sub(g.jobStarted).Round(time.Second)
	switch st.State {
	case "done":
		g.setOverall(100, st.title()+"完成")
		g.appendLog(fmt.Sprintf("✅ %s完成，耗时 %s", st.title(), elapsed))
		if st.Output != "" {
			g.appendLog("备份包: " + st.Output)
		}
	case "canceled":
		g.setOverall(g.overallBar.Value, "已取消")
		g.appendLog("⏹ 任务已取消")
	default:
		g.setOverall(g.overallBar.Value, "失败")
		g.appendLog("❌ " + st.Error)
	}
	g.timeLabel.SetText(fmt.Sprintf("耗时 %s", elapsed))
	g.showResult(st)
}

func (g *guiApp) showResult(st jobStatus) {
	switch st.State {
	case "done":
		msg := st.title() + "完成"
		if st.Output != "" {
			msg += "\n\n备份包：\n" + st.Output
		}
		dialog.ShowInformation("完成", msg, g.win)
	case "failed":
		dialog.ShowError(fmt.Errorf("%s失败: %s", st.title(), st.Error), g.win)
	}
}

func (g *guiApp) setRunningUI(running bool) {
	for _, b := range g.startBtns {
		if running {
			b.Disable()
		} else {
			b.Enable()
		}
	}
	for _, b := range g.cancelBtns {
		if running {
			b.Enable()
		} else {
			b.Disable()
		}
	}
	if running {
		title := "备份"
		if g.jobKind == "restore" {
			title = "恢复"
		}
		g.stateChip.Text = "● " + title + "中 · 后台运行"
	} else {
		g.stateChip.Text = "● 空闲"
	}
	g.header.Refresh() // 文字变长后需要重新布局，否则会被截断
}

func (g *guiApp) cancelTask() {
	if g.jobDir == "" {
		return
	}
	dir := g.jobDir
	dialog.ShowConfirm("取消任务", "确定取消正在运行的任务？未完成的备份包会被删除。", func(ok bool) {
		if !ok {
			return
		}
		if err := requestCancel(dir); err != nil {
			dialog.ShowError(err, g.win)
			return
		}
		g.appendLog("已请求取消…")
	}, g.win)
}

// confirmClose 任务运行中关闭窗口时说明任务会在后台继续。
func (g *guiApp) confirmClose() {
	if g.jobDir == "" {
		g.win.Close()
		return
	}
	dialog.ShowConfirm("任务在后台运行",
		"关闭窗口不会中断任务，它会在后台进程中继续执行。\n重新打开 SQLSync 会自动接上进度。\n\n确定关闭窗口？",
		func(ok bool) {
			if ok {
				g.win.Close()
			}
		}, g.win)
}

// applyEvents 批量应用进度事件；日志在批末统一刷新一次，避免大量事件时界面卡顿。
func (g *guiApp) applyEvents(evs []progress.Event) {
	if len(evs) == 0 {
		return
	}
	logged := false
	for _, e := range evs {
		logged = g.applyEvent(e) || logged
	}
	if logged {
		g.flushLog()
	}
}

// applyEvent 应用单个事件，返回是否新增了日志行（需调用方 flushLog）。
func (g *guiApp) applyEvent(e progress.Event) bool {
	switch e.Kind {
	case progress.KindLog, progress.KindSummary:
		g.addLogLine(e.When, e.Message)
		return true
	case progress.KindTaskStart, progress.KindTaskUpdate:
		g.upsertTask(e.Task, e)
	case progress.KindTaskDone:
		if r, ok := g.taskRows[e.Task]; ok {
			r.done = true
			r.bar.SetValue(100)
			r.label.Importance = widget.SuccessImportance
			r.label.SetText(fmt.Sprintf("✔ %s  %s", e.Task, e.Message))
			g.trimRows()
		}
	case progress.KindTaskFail:
		if r, ok := g.taskRows[e.Task]; ok {
			r.label.Importance = widget.DangerImportance
			r.label.SetText(fmt.Sprintf("✗ %s  %s", e.Task, e.Message))
		}
		g.addLogLine(e.When, "✗ "+e.Task+" "+e.Message)
		return true
	case progress.KindOverall:
		if e.Total > 0 {
			pct := float64(e.Done) / float64(e.Total) * 100
			if e.Done != e.Total { // 行数是预估值，未结束前封顶 99%
				pct = min(pct, 99)
			}
			g.setOverall(pct, e.Message)
		}
	}
	return false
}

func (g *guiApp) upsertTask(task string, e progress.Event) {
	r, ok := g.taskRows[task]
	if !ok {
		r = &taskRow{label: widget.NewLabel(task), bar: widget.NewProgressBar()}
		r.bar.Max = 100
		r.label.Truncation = fyne.TextTruncateEllipsis
		r.box = container.NewVBox(r.label, r.bar)
		g.taskRows[task] = r
		g.rowOrder = append(g.rowOrder, task)
		g.tasksBox.Add(r.box)
		g.trimRows()
	}
	if e.Total > 0 {
		r.bar.SetValue(min(float64(e.Done)/float64(e.Total)*100, 99))
	}
	if e.Message != "" {
		r.label.SetText(fmt.Sprintf("%s  %s · %s", task, e.Message, progress.Count(e.Done)))
	}
}

// trimRows 控制任务行数量，优先移除最早完成的。
func (g *guiApp) trimRows() {
	visible := len(g.taskRows)
	for visible > maxVisibleRows && len(g.rowOrder) > 0 {
		oldest := g.rowOrder[0]
		r := g.taskRows[oldest]
		if !r.done {
			return // 最早的还在跑就不再清，避免闪烁
		}
		g.tasksBox.Remove(r.box)
		delete(g.taskRows, oldest)
		g.rowOrder = g.rowOrder[1:]
		visible--
	}
}

func (g *guiApp) clearTasks() {
	g.taskRows = map[string]*taskRow{}
	g.rowOrder = nil
	g.tasksBox.Objects = nil
	g.tasksBox.Refresh()
}

func (g *guiApp) setOverall(pct float64, text string) {
	g.overallBar.SetValue(pct)
	g.overallPct.Text = fmt.Sprintf("%.0f%%", pct)
	g.overallPct.Refresh()
	g.overallLabel.SetText(text)
	g.status.SetText(text)
}

// updateTimes 刷新“已用时间 · 预计剩余”。
func (g *guiApp) updateTimes() {
	if g.jobDir == "" {
		return
	}
	elapsed := time.Since(g.jobStarted).Round(time.Second)
	s := "已用 " + elapsed.String()
	if p := g.overallBar.Value; p >= 1 && p < 100 {
		left := time.Duration(float64(elapsed) * (100 - p) / p).Round(time.Second)
		s += " · 预计剩余 " + left.String()
	}
	g.timeLabel.SetText(s)
}

func (g *guiApp) addLogLine(when time.Time, s string) {
	if when.IsZero() {
		when = time.Now()
	}
	g.logLines = append(g.logLines, fmt.Sprintf("[%s] %s", when.Format("15:04:05"), s))
	if n := len(g.logLines); n > maxLogLines {
		g.logLines = append([]string(nil), g.logLines[n-maxLogLines:]...)
	}
}

func (g *guiApp) flushLog() {
	g.logArea.SetText(strings.Join(g.logLines, "\n"))
	g.logArea.CursorRow, g.logArea.CursorColumn = len(g.logLines), 0 // 滚动到底部
	g.logArea.Refresh()
}

func (g *guiApp) appendLog(s string) {
	g.addLogLine(time.Now(), s)
	g.flushLog()
}

func atoiDefault(s string, def int) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n < 0 {
		return def
	}
	return n
}

// applyPrefs / savePrefs 持久化连接参数（密码不落盘）。
func (g *guiApp) applyPrefs() {
	p := g.app.Preferences()
	if v := p.String("host"); v != "" {
		g.host.SetText(v)
	}
	if v := p.String("port"); v != "" {
		g.port.SetText(v)
	}
	if v := p.String("user"); v != "" {
		g.user.SetText(v)
	}
	if v := p.String("sslMode"); v != "" {
		g.sslMode.SetSelected(v)
	}
	if v := p.String("sslCA"); v != "" {
		g.sslCA.SetText(v)
	}
}

func (g *guiApp) savePrefs() {
	p := g.app.Preferences()
	p.SetString("host", g.host.Text)
	p.SetString("port", g.port.Text)
	p.SetString("user", g.user.Text)
	p.SetString("sslMode", g.sslMode.Selected)
	p.SetString("sslCA", g.sslCA.Text)
}

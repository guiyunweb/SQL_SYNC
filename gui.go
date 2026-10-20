//go:build cgo

package main

import (
	"context"
	_ "embed"
	"fmt"
	"image/color"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
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

	// 运行控制
	cancel context.CancelFunc
	running bool
	mu      sync.Mutex

	// 进度展示
	overallBar   *widget.ProgressBar
	overallPct   *canvas.Text
	overallLabel *widget.Label
	logArea      *widget.Entry
	tasksBox     *fyne.Container
	taskRows     map[string]*taskRow
	rowOrder     []string

	bakDBs *widget.CheckGroup // 备份页：服务器数据库勾选列表
	rstDBs *widget.CheckGroup // 恢复页：备份目录中数据库勾选列表

	bakCount, rstCount func() // 刷新两个勾选列表的“已选 x / y”计数
	tabs   *container.AppTabs

	status *widget.Label
}

type taskRow struct {
	label *widget.Label
	bar   *widget.ProgressBar
	done  bool
	box   *fyne.Container
}

const maxVisibleRows = 40 // 界面上最多保留的任务行数，超出则移除最早完成的任务

func runGUI() {
	a := app.NewWithID("com.sqlsync.gui")
	a.Settings().SetTheme(newTheme())
	if len(iconBytes) > 0 {
		a.SetIcon(fyne.NewStaticResource("icon.png", iconBytes))
	}
	g := &guiApp{app: a, taskRows: map[string]*taskRow{}}
	g.win = a.NewWindow("SQLSync —— MySQL 备份 / 恢复")
	content := g.buildUI()
	g.win.SetContent(content)
	g.win.Resize(fyne.NewSize(1200, 820))
	g.win.ShowAndRun()
}

// buildUI 组装主界面（独立出来便于离屏渲染测试）。
func (g *guiApp) buildUI() fyne.CanvasObject {
	connPanel := g.buildConnPanel()
	g.applyPrefs() // 控件创建后填充上次输入

	backupTab := g.buildBackupTab()
	restoreTab := g.buildRestoreTab()

	g.tabs = container.NewAppTabs(
		container.NewTabItemWithIcon("备份", theme.DocumentSaveIcon(), backupTab),
		container.NewTabItemWithIcon("恢复", theme.ViewRestoreIcon(), restoreTab),
	)
	g.tabs.SetTabLocation(container.TabLocationTop)

	// 左侧：连接 + 操作页签；右侧：运行监控
	left := container.NewBorder(connPanel, nil, nil, nil, g.tabs)
	split := container.NewHSplit(left, g.buildMonitorPanel())
	split.Offset = 0.56

	g.status = widget.NewLabel("就绪")
	statusBar := container.NewBorder(nil, nil, widget.NewIcon(theme.InfoIcon()), nil, g.status)

	return container.NewBorder(g.buildHeader(), statusBar, nil, nil, container.NewPadded(split))
}

// buildHeader 顶部品牌横幅。
func (g *guiApp) buildHeader() fyne.CanvasObject {
	bg := canvas.NewRectangle(brandPrimary)

	title := canvas.NewText("SQLSync", color.White)
	title.TextSize = 22
	title.TextStyle = fyne.TextStyle{Bold: true}
	sub := canvas.NewText("MySQL 并行备份 / 恢复工具", color.NRGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xCC})
	sub.TextSize = 13

	row := container.NewHBox(title, container.NewCenter(sub))
	if len(iconBytes) > 0 {
		logo := canvas.NewImageFromResource(fyne.NewStaticResource("icon.png", iconBytes))
		logo.FillMode = canvas.ImageFillContain
		logo.SetMinSize(fyne.NewSize(30, 30))
		row = container.NewHBox(logo, title, container.NewCenter(sub))
	}
	return container.NewStack(bg, container.NewPadded(container.NewPadded(row)))
}

// buildMonitorPanel 右侧的运行监控：总体进度、任务列表、日志。
func (g *guiApp) buildMonitorPanel() fyne.CanvasObject {
	g.overallPct = canvas.NewText("0%", theme.Color(theme.ColorNamePrimary))
	g.overallPct.TextSize = 30
	g.overallPct.TextStyle = fyne.TextStyle{Bold: true}
	g.overallLabel = widget.NewLabel("空闲")
	g.overallBar = widget.NewProgressBar()
	g.overallBar.Max = 100
	overall := widget.NewCard("总体进度", "", container.NewVBox(
		container.NewBorder(nil, nil, g.overallPct, nil, container.NewCenter(g.overallLabel)),
		g.overallBar,
	))

	g.tasksBox = container.NewVBox()
	tasks := widget.NewCard("正在执行的任务", "", container.NewVScroll(g.tasksBox))

	g.logArea = widget.NewMultiLineEntry()
	g.logArea.Wrapping = fyne.TextWrapOff
	g.logArea.TextStyle = fyne.TextStyle{Monospace: true}
	g.logArea.SetMinRowsVisible(8)
	clearLog := widget.NewButtonWithIcon("", theme.ContentClearIcon(), func() { g.logArea.SetText("") })
	clearLog.Importance = widget.LowImportance
	logs := widget.NewCard("", "", container.NewBorder(
		container.NewBorder(nil, nil, widget.NewLabelWithStyle("日志", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}), clearLog),
		nil, nil, nil, g.logArea))

	body := container.NewVSplit(tasks, logs)
	body.Offset = 0.5
	return container.NewBorder(overall, nil, nil, nil, body)
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
	g.sslCA.SetPlaceHolder("CA 证书路径（verify-ca / verify-full 模式）")

	caBtn := widget.NewButtonWithIcon("", theme.FolderOpenIcon(), func() {
		dialog.ShowFileOpen(func(f fyne.URIReadCloser, err error) {
			if f != nil {
				g.sslCA.SetText(f.URI().Path())
			}
		}, g.win)
	})
	caBtn.Importance = widget.LowImportance

	testBtn := widget.NewButtonWithIcon("测试连接", theme.ConfirmIcon(), func() { g.testConnection() })
	testBtn.Importance = widget.SuccessImportance

	// 主机与端口同行、用户与密码同行，压缩纵向空间
	portBox := container.NewGridWrap(fyne.NewSize(90, g.port.MinSize().Height), g.port)
	server := container.NewBorder(nil, nil, nil,
		container.NewHBox(widget.NewLabel("端口"), portBox), g.host)
	account := container.NewGridWithColumns(2, g.user, g.pass)
	ssl := container.NewBorder(nil, nil, g.sslMode, caBtn, g.sslCA)

	form := widget.NewForm(
		widget.NewFormItem("服务器", server),
		widget.NewFormItem("用户/密码", account),
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

// dbPicker 数据库勾选卡片：工具按钮 + 已选计数 + 滚动列表。
// 返回的 refresh 用于代码直接改写 Options/Selected 之后同步计数。
func dbPicker(cg *widget.CheckGroup, hint string, minH float32, tools ...fyne.CanvasObject) (fyne.CanvasObject, func()) {
	count := widget.NewLabel("")
	update := func() {
		if len(cg.Selected) == 0 {
			count.SetText(fmt.Sprintf("共 %d 个 · 未勾选（%s）", len(cg.Options), hint))
		} else {
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

	scroll := container.NewVScroll(cg)
	scroll.SetMinSize(fyne.NewSize(0, minH))
	toolbar := container.NewHBox(append(tools, layout.NewSpacer(), allBtn, clearBtn)...)
	return widget.NewCard("", "", container.NewBorder(
		container.NewVBox(sectionTitle(theme.StorageIcon(), "数据库选择"), toolbar),
		count, nil, nil, scroll)), update
}

func labelWith(name string, obj fyne.CanvasObject) *fyne.Container {
	return container.NewBorder(nil, nil, widget.NewLabel(name), nil, obj)
}

// buildBackupTab 备份页签。
func (g *guiApp) buildBackupTab() fyne.CanvasObject {
	g.bakDBs = widget.NewCheckGroup(nil, nil)
	loadBtn := widget.NewButtonWithIcon("从服务器获取", theme.ViewRefreshIcon(), func() { g.loadServerDatabases() })
	var picker fyne.CanvasObject
	picker, g.bakCount = dbPicker(g.bakDBs, "将备份全部非系统库", 190, loadBtn)

	tables := widget.NewEntry()
	tables.SetPlaceHolder("逗号分隔的表名，留空 = 每个勾选库的全部表")
	outDir := widget.NewEntry()
	outDir.SetText("./backups")
	dirBtn := widget.NewButtonWithIcon("", theme.FolderOpenIcon(), func() {
		dialog.ShowFolderOpen(func(f fyne.ListableURI, err error) {
			if f != nil {
				outDir.SetText(f.Path())
			}
		}, g.win)
	})
	workers := widget.NewEntry()
	workers.SetText("8")
	shardMin := widget.NewEntry()
	shardMin.SetText("2000000")
	shardMax := widget.NewEntry()
	shardMax.SetText("8")
	rowsPerInsert := widget.NewEntry()
	rowsPerInsert.SetText("500")
	chunkRows := widget.NewEntry()
	chunkRows.SetText("2000")
	gzipChk := widget.NewCheck("输出 gzip 压缩", nil)
	gzipChk.SetChecked(true)

	var startBtn *widget.Button
	startBtn = widget.NewButtonWithIcon("开始备份", theme.MediaPlayIcon(), func() {
		cfg, err := g.connConfig()
		if err != nil {
			dialog.ShowError(err, g.win)
			return
		}
		o := backup.Options{
			OutDir:        strings.TrimSpace(outDir.Text),
			Databases:     append([]string(nil), g.bakDBs.Selected...),
			Tables:        splitCSV(tables.Text),
			Workers:       atoiDefault(workers.Text, 8),
			ShardMinRows:  int64(atoiDefault(shardMin.Text, 2_000_000)),
			ShardMax:      atoiDefault(shardMax.Text, 8),
			RowsPerInsert: atoiDefault(rowsPerInsert.Text, 500),
			ChunkRows:     atoiDefault(chunkRows.Text, 2000),
			Gzip:          gzipChk.Checked,
			Report:        g.uiReporter(),
		}
		g.startTask("备份", startBtn, func(ctx context.Context) error {
			dir, err := backup.Run(ctx, cfg, o)
			if err == nil {
				g.info(fmt.Sprintf("备份完成，输出目录：%s", dir))
			}
			return err
		})
	})
	startBtn.Importance = widget.HighImportance
	cancelBtn := widget.NewButtonWithIcon("取消", theme.CancelIcon(), func() { g.cancelTask() })
	cancelBtn.Importance = widget.DangerImportance

	// 路径类参数占整行，数值类调优参数两列排布
	pathForm := widget.NewForm(
		widget.NewFormItem("输出目录", container.NewBorder(nil, nil, nil, dirBtn, outDir)),
		widget.NewFormItem("表过滤", tables),
	)
	tuning := container.NewGridWithColumns(2,
		widget.NewForm(
			widget.NewFormItem("并行数", workers),
			widget.NewFormItem("分片阈值(行)", shardMin),
			widget.NewFormItem("最大分片", shardMax),
		),
		widget.NewForm(
			widget.NewFormItem("块行数", chunkRows),
			widget.NewFormItem("每 INSERT 行数", rowsPerInsert),
			widget.NewFormItem("压缩", gzipChk),
		),
	)
	params := widget.NewCard("", "", container.NewVBox(
		sectionTitle(theme.SettingsIcon(), "备份参数"), pathForm, widget.NewSeparator(), tuning))

	body := container.NewVScroll(container.NewVBox(params, picker))
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
		})
	}()
}

// buildRestoreTab 恢复页签。
func (g *guiApp) buildRestoreTab() fyne.CanvasObject {
	dir := widget.NewEntry()
	dir.SetPlaceHolder("备份目录（根目录或某库子目录）")

	g.rstDBs = widget.NewCheckGroup(nil, nil)

	scan := func() {
		p := strings.TrimSpace(dir.Text)
		if p == "" {
			dialog.ShowError(fmt.Errorf("请先选择备份目录"), g.win)
			return
		}
		names, err := restore.ListBackedUpDatabases(p)
		if err != nil {
			dialog.ShowError(fmt.Errorf("扫描备份目录失败: %w", err), g.win)
			return
		}
		if len(names) == 0 {
			dialog.ShowInformation("提示", "该目录中没有可恢复的数据库（缺少 schema.sql）", g.win)
			return
		}
		g.rstDBs.Options = names
		g.rstDBs.Selected = nil
		g.rstDBs.Refresh()
		g.rstCount()
		g.appendLog(fmt.Sprintf("备份目录中发现 %d 个数据库: %s", len(names), strings.Join(names, ", ")))
	}
	dirBtn := widget.NewButtonWithIcon("", theme.FolderOpenIcon(), func() {
		dialog.ShowFolderOpen(func(f fyne.ListableURI, err error) {
			if f != nil {
				dir.SetText(f.Path())
				scan()
			}
		}, g.win)
	})
	scanBtn := widget.NewButtonWithIcon("扫描目录", theme.SearchIcon(), scan)
	var picker fyne.CanvasObject
	picker, g.rstCount = dbPicker(g.rstDBs, "将恢复目录内全部", 170, scanBtn)

	rename := widget.NewEntry()
	rename.SetPlaceHolder("库改名映射，如 old:new，多个逗号分隔")
	workers := widget.NewEntry()
	workers.SetText("8")
	commitBatch := widget.NewEntry()
	commitBatch.SetText("200")
	createChk := widget.NewCheck("目标库不存在时自动创建", nil)
	createChk.SetChecked(true)
	recreateChk := widget.NewCheck("恢复前 DROP 目标库（危险）", nil)

	var startBtn *widget.Button
	startBtn = widget.NewButtonWithIcon("开始恢复", theme.MediaPlayIcon(), func() {
		if recreateChk.Checked {
			dialog.ShowConfirm("危险操作",
				"将先 DROP 目标库再恢复，现有数据会丢失。确定继续？", func(ok bool) {
					if ok {
						g.doRestore(dir, rename, workers, commitBatch, createChk.Checked, true, startBtn)
					}
				}, g.win)
			return
		}
		g.doRestore(dir, rename, workers, commitBatch, createChk.Checked, false, startBtn)
	})
	startBtn.Importance = widget.HighImportance
	cancelBtn := widget.NewButtonWithIcon("取消", theme.CancelIcon(), func() { g.cancelTask() })
	cancelBtn.Importance = widget.DangerImportance

	pathForm := widget.NewForm(
		widget.NewFormItem("备份目录", container.NewBorder(nil, nil, nil, dirBtn, dir)),
		widget.NewFormItem("改名映射", rename),
	)
	tuning := container.NewGridWithColumns(2,
		widget.NewForm(widget.NewFormItem("并行数", workers)),
		widget.NewForm(widget.NewFormItem("批量提交", commitBatch)),
	)
	params := widget.NewCard("", "", container.NewVBox(
		sectionTitle(theme.SettingsIcon(), "恢复参数"), pathForm, widget.NewSeparator(), tuning,
		container.NewHBox(createChk, recreateChk)))

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

	body := container.NewVScroll(container.NewVBox(params, picker))
	return container.NewBorder(nil, actionBar(warn, cancelBtn, startBtn), nil, nil, body)
}

func (g *guiApp) doRestore(dir, rename, workers, commitBatch *widget.Entry,
	create, recreate bool, startBtn *widget.Button) {

	cfg, err := g.connConfig()
	if err != nil {
		dialog.ShowError(err, g.win)
		return
	}
	if strings.TrimSpace(dir.Text) == "" {
		dialog.ShowError(fmt.Errorf("请选择备份目录"), g.win)
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

	o := restore.Options{
		Dir:         strings.TrimSpace(dir.Text),
		Databases:   append([]string(nil), g.rstDBs.Selected...),
		Rename:      renameMap,
		Create:      create,
		Recreate:    recreate,
		Workers:     atoiDefault(workers.Text, 8),
		CommitBatch: atoiDefault(commitBatch.Text, 200),
		Report:      g.uiReporter(),
	}
	g.startTask("恢复", startBtn, func(ctx context.Context) error {
		if err := restore.Run(ctx, cfg, o); err != nil {
			return err
		}
		g.info("恢复完成")
		return nil
	})
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
	db, err := dbconn.Open(cfg)
	if err != nil {
		dialog.ShowError(err, g.win)
		return
	}
	defer db.Close()
	var ver string
	_ = db.QueryRow("SELECT VERSION()").Scan(&ver)
	g.savePrefs()
	dialog.ShowInformation("连接成功", "MySQL 版本: "+ver, g.win)
}

// startTask 以后台协程跑备份/恢复。
func (g *guiApp) startTask(name string, btn *widget.Button, run func(context.Context) error) {
	g.mu.Lock()
	if g.running {
		g.mu.Unlock()
		return
	}
	g.running = true
	g.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	g.cancel = cancel
	btn.Disable()
	g.clearTasks()
	g.setOverall(0, "运行中…")
	g.appendLog("开始" + name)

	go func() {
		start := time.Now()
		err := run(ctx)
		fyne.Do(func() {
			g.mu.Lock()
			g.running = false
			g.cancel = nil
			g.mu.Unlock()
			btn.Enable()
			if err != nil {
				g.setOverall(0, "失败")
				g.appendLog("❌ " + err.Error())
				if ctx.Err() == nil {
					dialog.ShowError(fmt.Errorf("%s失败: %w", name, err), g.win)
				}
			} else {
				g.setOverall(100, name+"完成")
				g.appendLog(fmt.Sprintf("✅ %s完成，耗时 %s", name, time.Since(start).Round(time.Second)))
			}
		})
	}()
	g.savePrefs()
}

func (g *guiApp) cancelTask() {
	g.mu.Lock()
	c := g.cancel
	g.mu.Unlock()
	if c != nil {
		c()
		g.appendLog("已请求取消…")
	}
}

// uiReporter 把引擎进度事件转到 UI 线程。
func (g *guiApp) uiReporter() progress.Reporter {
	return func(e progress.Event) {
		fyne.Do(func() { g.applyEvent(e) })
	}
}

func (g *guiApp) applyEvent(e progress.Event) {
	switch e.Kind {
	case progress.KindLog:
		g.appendLog(e.Message)
	case progress.KindTaskStart:
		g.upsertTask(e.Task, e)
	case progress.KindTaskUpdate:
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
		g.appendLog("✗ " + e.Task + " " + e.Message)
	case progress.KindSummary:
		if e.Total > 0 {
			g.setOverall(float64(e.Done)/float64(e.Total)*100,
				fmt.Sprintf("%d/%d", e.Done, e.Total))
		}
		g.appendLog(e.Message)
	}
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
		r.bar.SetValue(float64(e.Done) / float64(e.Total) * 100)
	}
	if e.Message != "" {
		r.label.SetText(fmt.Sprintf("%s  %s (%d 行)", task, e.Message, e.Done))
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

func (g *guiApp) appendLog(s string) {
	line := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), s)
	cur := g.logArea.Text
	const maxChars = 200_000
	if len(cur) > maxChars { // 防止长时间运行撑爆文本框
		cur = cur[len(cur)/2:]
	}
	g.logArea.SetText(cur + line + "\n")
	// 滚动到底部：把光标移到末尾
	g.logArea.CursorRow = len(strings.Split(g.logArea.Text, "\n")) - 1
	g.logArea.Refresh()
}

func (g *guiApp) info(msg string) {
	fyne.Do(func() { dialog.ShowInformation("提示", msg, g.win) })
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

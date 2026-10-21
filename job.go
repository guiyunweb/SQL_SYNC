package main

// 后台任务：GUI 不在自身进程里跑备份/恢复，而是拉起一个脱离会话的子进程
// （sqlsync __job <任务目录>）。关闭 GUI 或断开远程桌面后子进程继续运行，
// 进度写入任务目录下的 events.jsonl，结果写入 status.json；
// 重新打开 GUI 时自动找到仍在运行的任务并接上进度。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"sqlsync/internal/backup"
	"sqlsync/internal/dbconn"
	"sqlsync/internal/progress"
	"sqlsync/internal/restore"
)

const (
	jobPwdEnv    = "SQLSYNC_JOB_PWD" // 密码经环境变量传给子进程，不落盘
	jobSpecFile  = "job.json"
	jobEventFile = "events.jsonl"
	jobStatFile  = "status.json"
	jobCancel    = "cancel" // GUI 创建该文件即请求取消
	jobSeen      = "seen"   // GUI 已展示过最终结果
	jobsKeep     = 20       // 保留最近多少个任务目录
)

// jobSpec 任务描述（job.json）。
type jobSpec struct {
	Kind    string           `json:"kind"` // backup | restore
	Conn    dbconn.Config    `json:"conn"` // Password 写盘前清空
	Backup  *backup.Options  `json:"backup,omitempty"`
	Restore *restore.Options `json:"restore,omitempty"`
}

// jobStatus 任务状态（status.json）。
type jobStatus struct {
	State    string    `json:"state"` // running | done | failed | canceled
	PID      int       `json:"pid"`
	Kind     string    `json:"kind"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitempty"`
	Output   string    `json:"output,omitempty"` // 备份包路径
	Error    string    `json:"error,omitempty"`
}

func (s jobStatus) title() string {
	if s.Kind == "restore" {
		return "恢复"
	}
	return "备份"
}

// jobsRoot 任务目录根：用户配置目录/sqlsync/jobs。
func jobsRoot() string {
	base, err := os.UserConfigDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "sqlsync", "jobs")
}

func readStatus(dir string) (jobStatus, error) {
	var st jobStatus
	raw, err := os.ReadFile(filepath.Join(dir, jobStatFile))
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(raw, &st)
}

// writeStatus 先写临时文件再改名，避免读到半个 JSON。
func writeStatus(dir string, st jobStatus) error {
	raw, _ := json.MarshalIndent(st, "", "  ")
	tmp := filepath.Join(dir, jobStatFile+".tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, jobStatFile))
}

// startJob 创建任务目录并以脱离会话的方式拉起子进程，返回任务目录。
func startJob(spec jobSpec, password string) (string, *exec.Cmd, error) {
	root := jobsRoot()
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", nil, err
	}
	pruneJobs(root)
	dir, err := os.MkdirTemp(root, time.Now().Format("20060102_150405_"))
	if err != nil {
		return "", nil, err
	}
	spec.Conn.Password = ""
	raw, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return "", nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, jobSpecFile), raw, 0o600); err != nil {
		return "", nil, err
	}
	// 子进程启动前先写一份 running 状态，GUI 立刻就能识别
	if err := writeStatus(dir, jobStatus{State: "running", Kind: spec.Kind, Started: time.Now()}); err != nil {
		return "", nil, err
	}

	exe, err := os.Executable()
	if err != nil {
		return "", nil, err
	}
	cmd := exec.Command(exe, "__job", dir)
	cmd.Env = append(os.Environ(), jobPwdEnv+"="+password)
	cmd.Dir = dir
	if err := startDetached(cmd); err != nil {
		return "", nil, fmt.Errorf("启动后台任务进程失败: %w", err)
	}
	return dir, cmd, nil
}

// pruneJobs 只保留最近 jobsKeep 个已结束的任务目录。
func pruneJobs(root string) {
	dirs := listJobDirs(root)
	for i := jobsKeep; i < len(dirs); i++ {
		if st, err := readStatus(dirs[i]); err == nil && st.State == "running" {
			continue
		}
		os.RemoveAll(dirs[i])
	}
}

// listJobDirs 按名称（即时间）倒序列出任务目录。
func listJobDirs(root string) []string {
	entries, _ := os.ReadDir(root)
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, filepath.Join(root, e.Name()))
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out
}

// findJobs 找出仍在运行的任务，以及已结束但 GUI 还没展示过结果的任务。
// 状态为 running 但进程已不在的任务会被标记为失败。
func findJobs() (running string, unseen []string) {
	for _, dir := range listJobDirs(jobsRoot()) {
		st, err := readStatus(dir)
		if err != nil {
			continue
		}
		if st.State == "running" {
			if st.PID > 0 && processAlive(st.PID) {
				if running == "" {
					running = dir
				}
				continue
			}
			// 刚启动还没写 PID 的给 10 秒宽限
			if st.PID == 0 && time.Since(st.Started) < 10*time.Second {
				if running == "" {
					running = dir
				}
				continue
			}
			st.State, st.Error, st.Finished = "failed", "后台进程意外退出（可能被系统结束或机器重启）", time.Now()
			_ = writeStatus(dir, st)
		}
		if _, err := os.Stat(filepath.Join(dir, jobSeen)); err != nil {
			unseen = append(unseen, dir)
		}
	}
	return running, unseen
}

func markSeen(dir string) { _ = os.WriteFile(filepath.Join(dir, jobSeen), nil, 0o600) }

func requestCancel(dir string) error {
	return os.WriteFile(filepath.Join(dir, jobCancel), nil, 0o600)
}

// runJobWorker 子进程入口：sqlsync __job <任务目录>。
func runJobWorker(dir string) {
	ignoreHangup() // 远程桌面/终端断开时不随之退出
	keepAwake()    // Windows：任务期间阻止系统睡眠

	st, _ := readStatus(dir)
	st.State, st.PID = "running", os.Getpid()
	if st.Started.IsZero() {
		st.Started = time.Now()
	}

	finish := func(output string, err error) {
		st.Finished, st.Output = time.Now(), output
		switch {
		case err == nil:
			st.State = "done"
		case err == context.Canceled:
			st.State, st.Error = "canceled", "已取消"
		default:
			st.State, st.Error = "failed", err.Error()
		}
		_ = writeStatus(dir, st)
	}

	raw, err := os.ReadFile(filepath.Join(dir, jobSpecFile))
	var spec jobSpec
	if err == nil {
		err = json.Unmarshal(raw, &spec)
	}
	if err != nil {
		finish("", fmt.Errorf("读取任务描述失败: %w", err))
		os.Exit(1)
	}
	st.Kind = spec.Kind
	_ = writeStatus(dir, st)

	ef, err := os.OpenFile(filepath.Join(dir, jobEventFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		finish("", err)
		os.Exit(1)
	}
	defer ef.Close()
	var mu sync.Mutex
	w := bufio.NewWriter(ef)
	report := func(e progress.Event) {
		line, _ := json.Marshal(e)
		mu.Lock()
		w.Write(line)
		w.WriteByte('\n')
		w.Flush()
		mu.Unlock()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { // 轮询取消标记文件（跨平台，GUI 只需建个文件）
		for range time.Tick(time.Second) {
			if _, err := os.Stat(filepath.Join(dir, jobCancel)); err == nil {
				report(progress.Event{Kind: progress.KindLog, When: time.Now(), Message: "收到取消请求，正在停止…"})
				cancel()
				return
			}
		}
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() { <-sig; cancel() }()

	cfg := spec.Conn
	cfg.Password = os.Getenv(jobPwdEnv)
	os.Unsetenv(jobPwdEnv)

	var output string
	switch spec.Kind {
	case "backup":
		if spec.Backup == nil {
			err = fmt.Errorf("任务缺少备份参数")
			break
		}
		o := *spec.Backup
		o.Report = report
		output, err = backup.Run(ctx, cfg, o)
	case "restore":
		if spec.Restore == nil {
			err = fmt.Errorf("任务缺少恢复参数")
			break
		}
		o := *spec.Restore
		o.Report = report
		err = restore.Run(ctx, cfg, o)
	default:
		err = fmt.Errorf("未知任务类型 %q", spec.Kind)
	}
	if err != nil && ctx.Err() != nil {
		err = context.Canceled
	}
	finish(output, err)
}

// jobTail 增量读取任务的 events.jsonl。
type jobTail struct {
	path   string
	offset int64
	rest   []byte
}

func newJobTail(dir string) *jobTail {
	return &jobTail{path: filepath.Join(dir, jobEventFile)}
}

// poll 读出自上次以来新增的完整事件行。
func (t *jobTail) poll() []progress.Event {
	f, err := os.Open(t.path)
	if err != nil {
		return nil
	}
	defer f.Close()
	if _, err := f.Seek(t.offset, 0); err != nil {
		return nil
	}
	buf := make([]byte, 1<<20)
	var out []progress.Event
	for {
		n, err := f.Read(buf)
		if n > 0 {
			t.offset += int64(n)
			t.rest = append(t.rest, buf[:n]...)
			for {
				i := bytes.IndexByte(t.rest, '\n')
				if i < 0 {
					break
				}
				var e progress.Event
				if json.Unmarshal(t.rest[:i], &e) == nil {
					out = append(out, e)
				}
				t.rest = t.rest[i+1:]
			}
		}
		if err != nil || n == 0 {
			break
		}
	}
	return out
}

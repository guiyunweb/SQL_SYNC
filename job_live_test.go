//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"sqlsync/internal/backup"
	"sqlsync/internal/dbconn"
	"sqlsync/internal/progress"
)

// TestMain 让测试二进制也能充当 "__job" 子进程（startJob 会以 os.Executable 拉起自身）。
func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "__job" {
		runJobWorker(os.Args[2])
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func liveCfg(t *testing.T) (dbconn.Config, string) {
	dsn := os.Getenv("SQLSYNC_LIVE_DSN")
	if dsn == "" {
		t.Skip("未设置 SQLSYNC_LIVE_DSN，跳过真实连库测试")
	}
	mc, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	host, port, _ := strings.Cut(mc.Addr, ":")
	p, _ := strconv.Atoi(port)
	return dbconn.Config{Host: host, Port: p, User: mc.User, SSLMode: "preferred"}, mc.Passwd
}

func waitJob(t *testing.T, dir string, timeout time.Duration) jobStatus {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if st, err := readStatus(dir); err == nil && st.State != "running" {
			return st
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("任务 %s 超时未结束", dir)
	return jobStatus{}
}

// TestBackgroundJobLive 后台任务：独立会话运行、进度落盘、完成后产出备份包；取消后不留残包。
// 需要 SQLSYNC_LIVE_DSN 和库 SQLSYNC_LIVE_DB（默认 mysql 以外的任意小库）。
func TestBackgroundJobLive(t *testing.T) {
	cfg, pwd := liveCfg(t)
	dbName := os.Getenv("SQLSYNC_LIVE_DB")
	if dbName == "" {
		t.Skip("未设置 SQLSYNC_LIVE_DB")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // 任务目录隔离到临时目录
	out := t.TempDir()

	dir, cmd, err := startJob(jobSpec{Kind: "backup", Conn: cfg,
		Backup: &backup.Options{OutDir: out, Databases: []string{dbName}, Workers: 4}}, pwd)
	if err != nil {
		t.Fatal(err)
	}
	go cmd.Wait()

	// setsid 后子进程自成进程组组长，与 GUI 不在同一进程组/会话
	child, _ := syscall.Getpgid(cmd.Process.Pid)
	self, _ := syscall.Getpgid(0)
	if child != cmd.Process.Pid || child == self {
		t.Fatalf("子进程未脱离 GUI 会话（pgid=%d, self=%d）", child, self)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, jobSpecFile)); strings.Contains(string(raw), pwd) && pwd != "" {
		t.Fatal("job.json 中出现了明文密码")
	}
	if running, _ := findJobs(); running != dir {
		t.Fatalf("findJobs 应找到运行中的任务 %s，实际 %q", dir, running)
	}

	st := waitJob(t, dir, 5*time.Minute)
	if st.State != "done" {
		t.Fatalf("任务未成功: %+v", st)
	}
	if _, err := os.Stat(st.Output); err != nil || !strings.HasSuffix(st.Output, ".tar.gz") {
		t.Fatalf("备份包不存在: %q %v", st.Output, err)
	}
	evs := newJobTail(dir).poll()
	var overall int
	for _, e := range evs {
		if e.Kind == progress.KindOverall {
			overall++
		}
	}
	if len(evs) == 0 || overall == 0 {
		t.Fatalf("事件文件缺少进度: %d 条, 总体进度 %d 条", len(evs), overall)
	}
	t.Logf("完成: %s，事件 %d 条", st.Output, len(evs))

	// 取消：启动后立刻请求取消，状态应为 canceled 且不留 .partial
	out2 := t.TempDir()
	dir2, cmd2, err := startJob(jobSpec{Kind: "backup", Conn: cfg,
		Backup: &backup.Options{OutDir: out2, Databases: []string{dbName}, Workers: 2}}, pwd)
	if err != nil {
		t.Fatal(err)
	}
	go cmd2.Wait()
	time.Sleep(1500 * time.Millisecond)
	if err := requestCancel(dir2); err != nil {
		t.Fatal(err)
	}
	st2 := waitJob(t, dir2, 2*time.Minute)
	if st2.State == "canceled" {
		left, _ := filepath.Glob(filepath.Join(out2, "*"))
		if len(left) != 0 {
			t.Fatalf("取消后残留文件: %v", left)
		}
	} else if st2.State != "done" { // 小库可能在取消前就已完成
		t.Fatalf("取消后状态异常: %+v", st2)
	}
	t.Logf("取消测试: %s", st2.State)
}

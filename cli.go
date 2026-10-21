package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"sqlsync/internal/backup"
	"sqlsync/internal/dbconn"
	"sqlsync/internal/progress"
	"sqlsync/internal/restore"
)

// connFlags 注册公共连接参数。
func connFlags(fs *flag.FlagSet, c *dbconn.Config) {
	fs.StringVar(&c.Host, "host", "127.0.0.1", "MySQL 主机")
	fs.IntVar(&c.Port, "port", 3306, "MySQL 端口")
	fs.StringVar(&c.User, "u", "root", "用户名")
	fs.StringVar(&c.Password, "p", os.Getenv("MYSQL_PWD"), "密码（默认取环境变量 MYSQL_PWD）")
	fs.StringVar(&c.SSLMode, "ssl-mode", "preferred", "SSL 模式: disable|preferred|skip-verify|verify-ca|verify-full")
	fs.StringVar(&c.SSLCA, "ssl-ca", "", "CA 证书路径（verify-ca/verify-full 模式）")
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ctxWithSignal 收到 Ctrl-C 时取消，做优雅退出。
func ctxWithSignal() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		fmt.Println("\n收到退出信号，正在取消…")
		cancel()
	}()
	return ctx, cancel
}

var lastOverall time.Time

// overallPercent 把总体进度事件格式化为百分比；预估总量偏小时封顶 99%。
func overallPercent(e progress.Event) string {
	if e.Total <= 0 {
		return "--"
	}
	p := float64(e.Done) / float64(e.Total) * 100
	if p >= 100 && e.Done != e.Total {
		p = 99
	}
	return fmt.Sprintf("%3.0f%%", min(p, 100))
}

// consoleReporter 把进度事件输出到终端。
func consoleReporter(e progress.Event) {
	ts := time.Now().Format("15:04:05")
	// 只打印关键事件，导出中的高频进度更新降频打印
	switch e.Kind {
	case progress.KindTaskUpdate:
		return // 单任务高频进度只给 GUI 用，终端看总体进度即可
	case progress.KindOverall:
		if time.Since(lastOverall) < 5*time.Second && e.Done < e.Total {
			return
		}
		lastOverall = time.Now()
		fmt.Printf("[%s] [总体 %s] %s\n", ts, overallPercent(e), e.Message)
	case progress.KindTaskStart, progress.KindTaskDone, progress.KindTaskFail, progress.KindSummary:
		fmt.Printf("[%s] %s %s\n", ts, e.Task, e.Message)
	default:
		fmt.Printf("[%s] %s\n", ts, e.Message)
	}
}

func runBackupCLI(args []string) {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	var cfg dbconn.Config
	connFlags(fs, &cfg)

	var outDir, databases, tables string
	var workers, rowsPerInsert, shardMax, level int
	var shardMinRows int64

	fs.StringVar(&outDir, "out", "./backups", "备份包输出目录")
	fs.StringVar(&databases, "databases", "", "逗号分隔的库名；默认全部非系统库")
	fs.StringVar(&tables, "tables", "", "逗号分隔的表名；默认全部表")
	fs.IntVar(&workers, "workers", 8, "并行 worker 数")
	fs.IntVar(&rowsPerInsert, "rows-per-insert", 500, "每条 INSERT 最多行数")
	fs.Int64Var(&shardMinRows, "shard-min-rows", 500_000, "每个分片的目标行数")
	fs.IntVar(&shardMax, "shard-max", 16, "单表最大分片数")
	fs.IntVar(&level, "level", 1, "gzip 压缩级别 1-9（1 最快）")
	// 已废弃：改为流式读取 + 单个 .tar.gz 输出后不再需要，保留以兼容旧脚本
	fs.Int("chunk-rows", 0, "（已废弃，忽略）")
	fs.Bool("gzip", true, "（已废弃，备份包始终为 .tar.gz）")
	fs.Parse(args)

	cfg.Workers = workers
	ctx, cancel := ctxWithSignal()
	defer cancel()

	start := time.Now()
	pkg, err := backup.Run(ctx, cfg, backup.Options{
		OutDir:        outDir,
		Databases:     splitCSV(databases),
		Tables:        splitCSV(tables),
		Workers:       workers,
		RowsPerInsert: rowsPerInsert,
		ShardMinRows:  shardMinRows,
		ShardMax:      shardMax,
		CompressLevel: level,
		Report:        consoleReporter,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "备份失败:", err)
		os.Exit(1)
	}
	fmt.Printf("✅ 备份完成: %s（耗时 %s）\n", pkg, time.Since(start).Round(time.Second))
}

func runRestoreCLI(args []string) {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	var cfg dbconn.Config
	connFlags(fs, &cfg)

	var dir, databases, rename string
	var create, recreate bool
	var workers, commitBatch int

	fs.StringVar(&dir, "file", "", "备份包 .tar.gz（或旧版备份目录）")
	fs.StringVar(&dir, "dir", "", "同 -file（兼容旧参数名）")
	fs.StringVar(&databases, "databases", "", "只恢复指定的库（逗号分隔，按备份中的库名）")
	fs.StringVar(&rename, "rename", "", "库改名映射 old:new，多个用逗号分隔")
	fs.BoolVar(&create, "create", true, "目标库不存在时自动创建")
	fs.BoolVar(&recreate, "recreate", false, "恢复前 DROP 目标库（危险）")
	fs.IntVar(&workers, "workers", 8, "并行 worker 数")
	fs.IntVar(&commitBatch, "commit-batch", 200, "每个事务提交的语句数")
	fs.Parse(args)

	if dir == "" {
		fmt.Fprintln(os.Stderr, "缺少 -file 备份包路径")
		fs.Usage()
		os.Exit(2)
	}

	renameMap := map[string]string{}
	for _, pair := range splitCSV(rename) {
		parts := strings.SplitN(pair, ":", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			fmt.Fprintf(os.Stderr, "-rename 格式错误: %q（应为 old:new，多个用逗号分隔）\n", pair)
			os.Exit(2)
		}
		renameMap[parts[0]] = parts[1]
	}

	cfg.Workers = workers
	ctx, cancel := ctxWithSignal()
	defer cancel()

	start := time.Now()
	err := restore.Run(ctx, cfg, restore.Options{
		Dir:         dir,
		Databases:   splitCSV(databases),
		Rename:      renameMap,
		Create:      create,
		Recreate:    recreate,
		Workers:     workers,
		CommitBatch: commitBatch,
		Report:      consoleReporter,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "恢复失败:", err)
		os.Exit(1)
	}
	fmt.Printf("✅ 恢复完成（耗时 %s）\n", time.Since(start).Round(time.Second))
}

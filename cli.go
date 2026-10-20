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

// consoleReporter 把进度事件输出到终端。
func consoleReporter(e progress.Event) {
	ts := time.Now().Format("15:04:05")
	// 只打印关键事件，导出中的高频进度更新降频打印
	switch e.Kind {
	case progress.KindTaskUpdate:
		if e.Done > 0 && e.Done%200000 != 0 {
			return
		}
		fmt.Printf("[%s] [进度] %s: %s (已处理 %d 行)\n", ts, e.Task, e.Message, e.Done)
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
	var workers, chunkRows, rowsPerInsert, shardMax int
	var shardMinRows int64
	var gzipOut bool

	fs.StringVar(&outDir, "out", "./backups", "输出目录")
	fs.StringVar(&databases, "databases", "", "逗号分隔的库名；默认全部非系统库")
	fs.StringVar(&tables, "tables", "", "逗号分隔的表名；默认全部表")
	fs.IntVar(&workers, "workers", 8, "并行 worker 数")
	fs.IntVar(&chunkRows, "chunk-rows", 2000, "每次 SELECT 拉取行数")
	fs.IntVar(&rowsPerInsert, "rows-per-insert", 500, "每条 INSERT 最多行数")
	fs.Int64Var(&shardMinRows, "shard-min-rows", 2_000_000, "大表主键分片阈值")
	fs.IntVar(&shardMax, "shard-max", 8, "单表最大分片数")
	fs.BoolVar(&gzipOut, "gzip", true, "输出 gzip 压缩")
	fs.Parse(args)

	cfg.Workers = workers
	ctx, cancel := ctxWithSignal()
	defer cancel()

	start := time.Now()
	dir, err := backup.Run(ctx, cfg, backup.Options{
		OutDir:        outDir,
		Databases:     splitCSV(databases),
		Tables:        splitCSV(tables),
		Workers:       workers,
		RowsPerInsert: rowsPerInsert,
		ChunkRows:     chunkRows,
		ShardMinRows:  shardMinRows,
		ShardMax:      shardMax,
		Gzip:          gzipOut,
		Report:        consoleReporter,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "备份失败:", err)
		os.Exit(1)
	}
	fmt.Printf("✅ 备份完成: %s（耗时 %s）\n", dir, time.Since(start).Round(time.Second))
}

func runRestoreCLI(args []string) {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	var cfg dbconn.Config
	connFlags(fs, &cfg)

	var dir, databases, rename string
	var create, recreate bool
	var workers, commitBatch int

	fs.StringVar(&dir, "dir", "", "备份目录（必填）")
	fs.StringVar(&databases, "databases", "", "只恢复指定的库（逗号分隔，按备份中的库名）")
	fs.StringVar(&rename, "rename", "", "库改名映射 old:new，多个用逗号分隔")
	fs.BoolVar(&create, "create", true, "目标库不存在时自动创建")
	fs.BoolVar(&recreate, "recreate", false, "恢复前 DROP 目标库（危险）")
	fs.IntVar(&workers, "workers", 8, "并行 worker 数")
	fs.IntVar(&commitBatch, "commit-batch", 200, "每个事务提交的语句数")
	fs.Parse(args)

	if dir == "" {
		fmt.Fprintln(os.Stderr, "缺少 -dir 备份目录")
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

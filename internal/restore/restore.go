// Package restore 实现并行恢复：先恢复表结构，再多 worker 并行灌数据。
// 针对大数据量做了会话调优：关闭 foreign_key_checks/unique_checks/autocommit，
// 批量事务提交，INSERT 语句逐行执行。
package restore

import (
	"bufio"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"sqlsync/internal/dbconn"
	"sqlsync/internal/progress"
	"sqlsync/internal/sqlfmt"
)

// Options 恢复参数。
type Options struct {
	Dir       string            // 备份目录：可以是备份根目录，也可以是某个库的子目录
	Databases []string          // 只恢复列出的库（按备份时的库名），为空恢复全部
	Rename    map[string]string // 库改名映射 src:dst

	Create   bool // 目标库不存在时创建
	Recreate bool // 恢复前 DROP 目标库（危险，需显式打开）

	Workers     int // 并行 worker 数
	CommitBatch int // 每个事务提交的语句条数

	Report progress.Reporter
}

func (o *Options) withDefaults() {
	if o.Workers <= 0 {
		o.Workers = 8
	}
	if o.CommitBatch <= 0 {
		o.CommitBatch = 200
	}
	if o.Report == nil {
		o.Report = progress.Nop
	}
}

// dbDir 找到一个待恢复的库目录。
type dbDir struct {
	name     string // 备份中的原库名
	target   string // 恢复到目标的库名
	dir      string
	meta     Meta
	dataFiles []string
}

// Meta 与备份端写出的 meta.json 对应（此处只取需要的字段）。
type Meta struct {
	Database  string `json:"database"`
	Charset   string `json:"charset"`
	Collation string `json:"collation"`
	TotalRows int64  `json:"total_rows"`
}

// Run 执行恢复。
func Run(ctx context.Context, cfg dbconn.Config, o Options) error {
	o.withDefaults()

	src, err := discover(o)
	if err != nil {
		return err
	}
	if len(src) == 0 {
		return fmt.Errorf("目录 %s 中没有可恢复的备份", o.Dir)
	}

	// 建库（或重建）在普通连接上完成
	cfgNoDB := cfg
	cfgNoDB.Database = ""
	admin, err := dbconn.Open(cfgNoDB)
	if err != nil {
		return err
	}
	defer admin.Close()

	for _, d := range src {
		if err := ctx.Err(); err != nil {
			return err
		}

		cs, coll := d.meta.Charset, d.meta.Collation
		if cs == "" {
			cs = "utf8mb4"
		}
		if coll == "" {
			coll = "utf8mb4_general_ci"
		}

		if o.Recreate {
			o.Report(progress.Event{Kind: progress.KindLog, When: time.Now(),
				Message: fmt.Sprintf("⚠ DROP DATABASE %s（Recreate 模式）", d.target)})
			if _, err := admin.ExecContext(ctx,
				"DROP DATABASE IF EXISTS "+sqlfmt.QuoteIdent(d.target)); err != nil {
				return fmt.Errorf("删除库 %s 失败: %w", d.target, err)
			}
		}
		if o.Create || o.Recreate {
			q := fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s DEFAULT CHARACTER SET %s COLLATE %s",
				sqlfmt.QuoteIdent(d.target), cs, coll)
			if _, err := admin.ExecContext(ctx, q); err != nil {
				return fmt.Errorf("创建库 %s 失败: %w", d.target, err)
			}
		}

		// 该库独占一个目标连接（schema 顺序执行 + 数据并发）
		cfgDB := cfg
		cfgDB.Database = d.target
		tdb, err := dbconn.Open(cfgDB)
		if err != nil {
			return err
		}

		o.Report(progress.Event{Kind: progress.KindLog, When: time.Now(),
			Message: fmt.Sprintf("恢复库 %s -> %s: 开始执行 schema.sql", d.name, d.target)})
		if err := applySchema(ctx, tdb, filepath.Join(d.dir, "schema.sql")); err != nil {
			tdb.Close()
			return fmt.Errorf("恢复库 %s 表结构失败: %w", d.target, err)
		}
		o.Report(progress.Event{Kind: progress.KindLog, When: time.Now(),
			Message: fmt.Sprintf("库 %s: 表结构恢复完成，开始灌入数据（%d 个文件，%d worker）",
				d.target, len(d.dataFiles), o.Workers)})

		if err := o.loadData(ctx, tdb, d); err != nil {
			tdb.Close()
			return fmt.Errorf("恢复库 %s 数据失败: %w", d.target, err)
		}
		tdb.Close()

		o.Report(progress.Event{Kind: progress.KindSummary, When: time.Now(), Task: d.target,
			Message: fmt.Sprintf("库 %s 恢复完成（预估总行数 %d）", d.target, d.meta.TotalRows)})
	}
	return nil
}

// discover 在 Dir 下找到全部库目录。
func discover(o Options) ([]dbDir, error) {
	isDBDir := func(dir string) bool {
		st, err := os.Stat(filepath.Join(dir, "schema.sql"))
		return err == nil && !st.IsDir()
	}

	var dirs []struct{ name, path string }
	if isDBDir(o.Dir) {
		dirs = append(dirs, struct{ name, path string }{filepath.Base(o.Dir), o.Dir})
	} else {
		entries, err := os.ReadDir(o.Dir)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			p := filepath.Join(o.Dir, e.Name())
			if isDBDir(p) {
				dirs = append(dirs, struct{ name, path string }{e.Name(), p})
			}
		}
	}

	wanted := map[string]bool{}
	for _, d := range o.Databases {
		wanted[d] = true
	}

	var out []dbDir
	for _, d := range dirs {
		if len(wanted) > 0 && !wanted[d.name] {
			continue
		}
		dd := dbDir{name: d.name, target: d.name, dir: d.path}
		if dst, ok := o.Rename[d.name]; ok && dst != "" {
			dd.target = dst
		}

		// 读 meta（可选）
		var m Meta
		if raw, err := os.ReadFile(filepath.Join(d.path, "meta.json")); err == nil {
			_ = json.Unmarshal(raw, &m)
			if m.Database != "" {
				dd.name = m.Database
			}
		}
		dd.meta = m

		files, err := filepath.Glob(filepath.Join(d.path, "data", "*.sql*"))
		if err != nil {
			return nil, err
		}
		sort.Strings(files)
		dd.dataFiles = files

		out = append(out, dd)
	}
	// 按原库名稳定排序，保证多库时恢复顺序可预期
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// applySchema 顺序执行 schema.sql 中的语句（按标记切分）。
func applySchema(ctx context.Context, db *sql.DB, schemaPath string) error {
	f, err := os.Open(schemaPath)
	if err != nil {
		return err
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, 1<<20)
	var b strings.Builder
	inStmt := false

	lineNo := 0
	for {
		line, err := r.ReadString('\n')
		lineNo++
		if strings.HasPrefix(line, sqlfmt.StmtBegin) {
			inStmt = true
			b.Reset()
		} else if strings.HasPrefix(line, sqlfmt.StmtEnd) {
			inStmt = false
			stmt := strings.TrimSpace(b.String())
			if stmt != "" {
				if _, err := db.ExecContext(ctx, stmt); err != nil {
					snip := stmt
					if len(snip) > 200 {
						snip = snip[:200] + "..."
					}
					return fmt.Errorf("schema.sql 第 %d 行附近语句执行失败: %w\n语句: %s", lineNo, err, snip)
				}
			}
		} else if inStmt {
			b.WriteString(line)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// loadData 并发灌入一个库的全部数据文件。
func (o Options) loadData(ctx context.Context, db *sql.DB, d dbDir) error {
	if len(d.dataFiles) == 0 {
		return nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	total := int64(len(d.dataFiles))
	var doneFiles int64

	fileCh := make(chan string, len(d.dataFiles))
	for _, f := range d.dataFiles {
		fileCh <- f
	}
	close(fileCh)

	var mu sync.Mutex
	var firstErr error
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
		mu.Unlock()
	}

	var wg sync.WaitGroup
	for i := 0; i < o.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := db.Conn(ctx)
			if err != nil {
				fail(err)
				return
			}
			defer conn.Close()

			tune := []string{
				"SET SESSION foreign_key_checks = 0",
				"SET SESSION unique_checks = 0",
				"SET SESSION sql_log_bin = 0",
				"SET SESSION autocommit = 0",
			}
			for _, q := range tune {
				if _, err := conn.ExecContext(ctx, q); err != nil {
					// 普通账号可能没有 sql_log_bin 权限，忽略即可
					o.Report(progress.Event{Kind: progress.KindLog, When: time.Now(),
						Message: "会话调优语句失败（忽略）: " + q + " → " + err.Error()})
				}
			}

			for path := range fileCh {
				if ctx.Err() != nil {
					return
				}
				if err := o.loadFile(ctx, conn, d, path); err != nil {
					fail(err)
					return
				}
				mu.Lock()
				doneFiles++
				df := doneFiles
				mu.Unlock()
				o.Report(progress.Event{Kind: progress.KindSummary, When: time.Now(), Task: d.target,
					Message: fmt.Sprintf("数据文件完成 %d/%d", df, total), Done: df, Total: total})
			}
		}()
	}
	wg.Wait()
	return firstErr
}

// loadFile 在单连接上按批量事务执行一个数据文件。
func (o Options) loadFile(ctx context.Context, conn *sql.Conn, d dbDir, path string) error {
	name := filepath.Base(path)
	task := d.target + "/" + name
	start := time.Now()
	report := func(k progress.Kind, msg string, done int64) {
		o.Report(progress.Event{Kind: k, When: time.Now(), Task: task, Message: msg, Done: done})
	}
	report(progress.KindTaskStart, "开始恢复", 0)

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var rd io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gz.Close()
		rd = gz
	}
	r := bufio.NewReaderSize(rd, 4<<20)

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := true
	defer func() {
		if !committed {
			tx.Rollback()
		}
	}()

	var lines, batch int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			s := strings.TrimSpace(string(line))
			if s != "" && !strings.HasPrefix(s, "--") {
				if _, err := tx.ExecContext(ctx, s); err != nil {
					snip := s
					if len(snip) > 200 {
						snip = snip[:200] + "..."
					}
					return fmt.Errorf("%s 执行失败: %w\n语句: %s", name, err, snip)
				}
				lines++
				batch++
				if batch >= int64(o.CommitBatch) {
					if err := tx.Commit(); err != nil {
						return err
					}
					committed = true
					report(progress.KindTaskUpdate, fmt.Sprintf("已执行 %d 条语句", lines), lines)
					tx, err = conn.BeginTx(ctx, nil)
					if err != nil {
						return err
					}
					committed = false
					batch = 0
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true

	report(progress.KindTaskDone,
		fmt.Sprintf("完成，%d 条语句，耗时 %s", lines, time.Since(start).Round(time.Second)), lines)
	return nil
}

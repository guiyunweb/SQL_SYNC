package restore

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/gzip"

	"sqlsync/internal/dbconn"
	"sqlsync/internal/progress"
	"sqlsync/internal/sqlfmt"
)

// manifestName 与备份端 backup.ManifestName 一致：包内第一个条目。
const manifestName = "sqlsync.json"

// maxEntryBytes 单个数据条目的读入上限（备份端默认约 8~10MB），防止异常包撑爆内存。
const maxEntryBytes = 512 << 20

// manifest 备份包清单（只取恢复需要的字段）。
type manifest struct {
	Format    int `json:"format"`
	Databases []struct {
		Name      string `json:"name"`
		Charset   string `json:"charset"`
		Collation string `json:"collation"`
		EstRows   int64  `json:"est_rows"`
	} `json:"databases"`
}

// isArchive 判断路径是否为备份包文件（而非旧版备份目录）。
func isArchive(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// openArchive 打开备份包并读出清单，返回定位在清单之后的 tar 读取器。
func openArchive(r io.Reader) (*tar.Reader, func() error, manifest, error) {
	var m manifest
	gz, err := gzip.NewReader(r) // 默认支持多 member 拼接
	if err != nil {
		return nil, nil, m, fmt.Errorf("不是有效的 .tar.gz 备份包: %w", err)
	}
	tr := tar.NewReader(gz)
	hdr, err := tr.Next()
	if err != nil {
		gz.Close()
		return nil, nil, m, fmt.Errorf("读取备份包失败: %w", err)
	}
	if hdr.Name != manifestName {
		gz.Close()
		return nil, nil, m, fmt.Errorf("备份包缺少 %s，可能不是 sqlsync 生成的", manifestName)
	}
	if err := json.NewDecoder(io.LimitReader(tr, 16<<20)).Decode(&m); err != nil {
		gz.Close()
		return nil, nil, m, fmt.Errorf("解析 %s 失败: %w", manifestName, err)
	}
	return tr, gz.Close, m, nil
}

// listArchiveDatabases 只读包头清单即可列出库名，不扫描整个包。
func listArchiveDatabases(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	_, closeFn, m, err := openArchive(bufio.NewReader(f))
	if err != nil {
		return nil, err
	}
	defer closeFn()
	out := make([]string, 0, len(m.Databases))
	for _, d := range m.Databases {
		out = append(out, d.Name)
	}
	return out, nil
}

// countingReader 统计已读取的压缩字节数，用于按文件位置计算总体进度。
type countingReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// chunkJob 一个待执行的数据条目。
type chunkJob struct {
	target string // 目标库
	label  string // 进度显示名
	data   []byte
}

// execer 能执行 SQL 的连接（*sql.DB / *sql.Conn 都满足）。
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// runArchive 流式恢复一个 .tar.gz 备份包：
// 按包内顺序读到 <库>/schema.sql 就建库建表，读到数据条目就交给 worker 池并行执行。
func runArchive(ctx context.Context, cfg dbconn.Config, o Options) error {
	f, err := os.Open(o.Dir)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	cr := &countingReader{r: f}
	tr, closeFn, man, err := openArchive(bufio.NewReaderSize(cr, 4<<20))
	if err != nil {
		return err
	}
	defer closeFn()

	// 选择要恢复的库
	wanted := map[string]bool{}
	for _, d := range o.Databases {
		wanted[d] = true
	}
	type dbInfo struct{ target, charset, collation string }
	sel := map[string]dbInfo{}
	var names []string
	for _, d := range man.Databases {
		if len(wanted) > 0 && !wanted[d.Name] {
			continue
		}
		target := d.Name
		if dst, ok := o.Rename[d.Name]; ok && dst != "" {
			target = dst
		}
		sel[d.Name] = dbInfo{target: target, charset: d.Charset, collation: d.Collation}
		names = append(names, d.Name+"→"+target)
	}
	if len(sel) == 0 {
		return fmt.Errorf("备份包中没有要恢复的库")
	}
	o.Report(progress.Event{Kind: progress.KindLog, When: time.Now(),
		Message: fmt.Sprintf("待恢复: %s（%d 个 worker）", strings.Join(names, ", "), o.Workers)})

	cfgNoDB := cfg
	cfgNoDB.Database = ""
	cfgNoDB.Workers = o.Workers
	pool, err := dbconn.Open(cfgNoDB)
	if err != nil {
		return err
	}
	defer pool.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
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

	var stmts atomic.Int64
	jobs := make(chan chunkJob, o.Workers)
	var wg sync.WaitGroup
	for i := 0; i < o.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := pool.Conn(ctx)
			if err != nil {
				fail(err)
				return
			}
			defer conn.Close()
			o.tuneSession(ctx, conn)
			cur := ""
			for job := range jobs {
				if ctx.Err() != nil {
					return
				}
				if job.target != cur {
					if _, err := conn.ExecContext(ctx, "USE "+sqlfmt.QuoteIdent(job.target)); err != nil {
						fail(err)
						return
					}
					cur = job.target
				}
				if err := o.execChunk(ctx, conn, job, &stmts); err != nil {
					fail(err)
					return
				}
			}
		}()
	}

	stopProgress := reportReadProgress(o, cr, st.Size(), &stmts)
	readErr := func() error {
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return fmt.Errorf("读取备份包失败: %w", err)
			}
			if hdr.Typeflag != tar.TypeReg {
				continue
			}
			src, rest, ok := strings.Cut(hdr.Name, "/")
			info, selected := sel[src]
			if !ok || !selected {
				continue // tar 读取器在下次 Next 时自动跳过内容
			}
			switch {
			case rest == "schema.sql":
				if err := o.prepareDatabase(ctx, pool, info.target, info.charset, info.collation); err != nil {
					return err
				}
				o.Report(progress.Event{Kind: progress.KindLog, When: time.Now(),
					Message: fmt.Sprintf("恢复库 %s -> %s: 执行表结构", src, info.target)})
				if err := applySchemaTo(ctx, pool, info.target, tr); err != nil {
					return fmt.Errorf("恢复库 %s 表结构失败: %w", info.target, err)
				}
				o.Report(progress.Event{Kind: progress.KindLog, When: time.Now(),
					Message: fmt.Sprintf("库 %s: 表结构完成", info.target)})
			case strings.HasPrefix(rest, "data/"):
				if hdr.Size > maxEntryBytes {
					return fmt.Errorf("条目 %s 过大（%s）", hdr.Name, progress.Bytes(hdr.Size))
				}
				data := make([]byte, hdr.Size)
				if _, err := io.ReadFull(tr, data); err != nil {
					return fmt.Errorf("读取 %s 失败: %w", hdr.Name, err)
				}
				// data/<表>/<分片>-<序号>.sql → "目标库/表 [分片-序号]"
				table, file, _ := strings.Cut(strings.TrimPrefix(rest, "data/"), "/")
				label := fmt.Sprintf("%s/%s [%s]", info.target, table, strings.TrimSuffix(file, ".sql"))
				select {
				case jobs <- chunkJob{target: info.target, label: label, data: data}:
				case <-ctx.Done():
					return nil
				}
			}
		}
	}()
	close(jobs)
	wg.Wait()
	stopProgress()

	if readErr != nil {
		fail(readErr)
	}
	if firstErr != nil {
		return firstErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	o.Report(progress.Event{Kind: progress.KindOverall, When: time.Now(), Done: 1, Total: 1,
		Message: fmt.Sprintf("已执行 %s 条语句", progress.Count(stmts.Load()))})
	o.Report(progress.Event{Kind: progress.KindSummary, When: time.Now(),
		Message: fmt.Sprintf("恢复完成: %d 个库, %s 条语句", len(sel), progress.Count(stmts.Load()))})
	return nil
}

// reportReadProgress 每 500ms 按已读取的压缩字节上报总体进度，返回停止函数。
func reportReadProgress(o Options, cr *countingReader, size int64, stmts *atomic.Int64) func() {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tk := time.NewTicker(500 * time.Millisecond)
		defer tk.Stop()
		start := time.Now()
		for {
			select {
			case <-done:
				return
			case now := <-tk.C:
				n := cr.n.Load()
				rate := float64(n) / max(now.Sub(start).Seconds(), 0.001)
				o.Report(progress.Event{Kind: progress.KindOverall, When: now, Done: n, Total: size,
					Message: fmt.Sprintf("已读取 %s / %s · %s/s · 已执行 %s 条语句",
						progress.Bytes(n), progress.Bytes(size), progress.Bytes(int64(rate)),
						progress.Count(stmts.Load()))})
			}
		}
	}()
	return func() { close(done); wg.Wait() }
}

// prepareDatabase 按需 DROP / CREATE 目标库。
func (o Options) prepareDatabase(ctx context.Context, ex execer, target, cs, coll string) error {
	if cs == "" {
		cs = "utf8mb4"
	}
	if coll == "" {
		coll = "utf8mb4_general_ci"
	}
	if o.Recreate {
		o.Report(progress.Event{Kind: progress.KindLog, When: time.Now(),
			Message: fmt.Sprintf("⚠ DROP DATABASE %s（Recreate 模式）", target)})
		if _, err := ex.ExecContext(ctx, "DROP DATABASE IF EXISTS "+sqlfmt.QuoteIdent(target)); err != nil {
			return fmt.Errorf("删除库 %s 失败: %w", target, err)
		}
	}
	if o.Create || o.Recreate {
		q := fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s DEFAULT CHARACTER SET %s COLLATE %s",
			sqlfmt.QuoteIdent(target), cs, coll)
		if _, err := ex.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("创建库 %s 失败: %w", target, err)
		}
	}
	return nil
}

// applySchemaTo 在池中取一条连接切到目标库后执行表结构。
func applySchemaTo(ctx context.Context, pool *sql.DB, target string, r io.Reader) error {
	conn, err := pool.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE "+sqlfmt.QuoteIdent(target)); err != nil {
		return err
	}
	return applySchemaReader(ctx, conn, r)
}

// tuneSession 灌数据连接的会话调优。
func (o Options) tuneSession(ctx context.Context, conn *sql.Conn) {
	for _, q := range []string{
		"SET SESSION foreign_key_checks = 0",
		"SET SESSION unique_checks = 0",
		"SET SESSION sql_log_bin = 0",
		"SET SESSION autocommit = 0",
	} {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			// 普通账号可能没有 sql_log_bin 权限，忽略即可
			o.Report(progress.Event{Kind: progress.KindLog, When: time.Now(),
				Message: "会话调优语句失败（忽略）: " + q + " → " + err.Error()})
		}
	}
}

// execChunk 在单连接上按批量事务执行一个数据条目（每行一条 INSERT）。
func (o Options) execChunk(ctx context.Context, conn *sql.Conn, job chunkJob, stmts *atomic.Int64) error {
	total := int64(bytes.Count(job.data, []byte{'\n'}))
	report := func(k progress.Kind, msg string, done int64) {
		o.Report(progress.Event{Kind: k, When: time.Now(), Task: job.label, Message: msg, Done: done, Total: total})
	}
	report(progress.KindTaskStart, "开始恢复", 0)

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	var done, batch int64
	data := job.data
	for len(data) > 0 {
		var line []byte
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line, data = data[:i], data[i+1:]
		} else {
			line, data = data, nil
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 || bytes.HasPrefix(line, []byte("--")) {
			continue
		}
		if _, err := tx.ExecContext(ctx, string(line)); err != nil {
			tx.Rollback()
			snip := string(line)
			if len(snip) > 200 {
				snip = snip[:200] + "..."
			}
			return fmt.Errorf("%s 执行失败: %w\n语句: %s", job.label, err, snip)
		}
		done++
		batch++
		stmts.Add(1)
		if batch >= int64(o.CommitBatch) {
			if err := tx.Commit(); err != nil {
				return err
			}
			report(progress.KindTaskUpdate, "恢复中", done)
			if tx, err = conn.BeginTx(ctx, nil); err != nil {
				return err
			}
			batch = 0
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	report(progress.KindTaskDone, fmt.Sprintf("完成，%d 条语句", done), done)
	return nil
}

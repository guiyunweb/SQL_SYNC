// Package backup 实现 MySQL 逻辑备份：表结构与数据分离导出，
// 多表并行、大表按数值主键范围分片、单查询流式读取，
// 全部内容并行压缩后写入单个 .tar.gz 备份包。
package backup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/gzip"

	"sqlsync/internal/dbconn"
	"sqlsync/internal/progress"
	"sqlsync/internal/sqlfmt"
)

// ManifestName 备份包内第一个条目，记录包含哪些库，恢复端只读它就能列出库清单。
const ManifestName = "sqlsync.json"

// Options 备份参数。
type Options struct {
	OutDir    string   // 备份包输出目录
	Databases []string // 为空表示全部非系统库
	Tables    []string // 仅备份这些表，为空不限

	Workers       int   // 并行 worker 数
	RowsPerInsert int   // 每条 INSERT 最多行数
	MaxStmtBytes  int   // 每条 INSERT 最大字节
	ChunkBytes    int   // 包内单个数据条目的目标大小（恢复端按条目并行）
	ShardMinRows  int64 // 每个分片的目标行数；表行数达到其 2 倍且含数值单列主键时分片
	ShardMax      int   // 单表最大分片数
	CompressLevel int   // gzip 级别 1-9，默认 1（最快）

	Report progress.Reporter `json:"-"`
}

// warn 上报一条告警日志。
func (o *Options) warn(msg string) {
	o.log("⚠ " + msg)
}

func (o *Options) log(msg string) {
	o.Report(progress.Event{Kind: progress.KindLog, When: time.Now(), Message: msg})
}

func (o *Options) withDefaults() {
	if o.Workers <= 0 {
		o.Workers = 8
	}
	if o.RowsPerInsert <= 0 {
		o.RowsPerInsert = 500
	}
	if o.MaxStmtBytes <= 0 {
		o.MaxStmtBytes = 2 << 20 // 兼容 max_allowed_packet=4MB 的老服务器
	}
	if o.ChunkBytes <= 0 {
		o.ChunkBytes = 8 << 20
	}
	if o.ShardMinRows <= 0 {
		o.ShardMinRows = 500_000
	}
	if o.ShardMax <= 0 {
		o.ShardMax = 16
	}
	if o.CompressLevel < gzip.BestSpeed || o.CompressLevel > gzip.BestCompression {
		o.CompressLevel = gzip.BestSpeed
	}
	if o.Report == nil {
		o.Report = progress.Nop
	}
}

// tableInfo 一张表的元信息。
type tableInfo struct {
	Database  string
	Name      string
	Rows      int64
	Engine    string
	PKCol     string // 数值型单列主键（用于分片），可为空
	OrderCols string // 主键/唯一键列（已带引号、逗号连接），可为空
}

// shardTask 一个导出任务：整表或某个主键区间。
type shardTask struct {
	tbl      tableInfo
	Shard    int // 0 表示整表导出；>0 为区间序号
	Shards   int
	Min, Max int64 // 区间 [min, max)，整表导出时无意义
	HasRange bool
	EstRows  int64 // 本任务预估行数，用于调度与进度
}

// Manifest 写在备份包最前面的清单。
type Manifest struct {
	Format    int          `json:"format"`
	CreatedAt time.Time    `json:"created_at"`
	Server    string       `json:"server"`
	Databases []ManifestDB `json:"databases"`
}

// ManifestDB 清单中的一个库。
type ManifestDB struct {
	Name      string `json:"name"`
	Charset   string `json:"charset"`
	Collation string `json:"collation"`
	Tables    int    `json:"tables"`
	EstRows   int64  `json:"est_rows"`
}

// Meta 写入 <库>/meta.json 的备份元数据（位于包末尾，记录实际行数）。
type Meta struct {
	Database    string           `json:"database"`
	Charset     string           `json:"charset"`
	Collation   string           `json:"collation"`
	StartedAt   time.Time        `json:"started_at"`
	FinishedAt  time.Time        `json:"finished_at"`
	MySQLServer string           `json:"server"`
	Tables      []string         `json:"tables"`
	TableRows   map[string]int64 `json:"table_rows"`
	TotalRows   int64            `json:"total_rows"`
}

// dbPlan 一个库的备份计划。
type dbPlan struct {
	meta   Meta
	tables []tableInfo
}

// counters 跨 worker 共享的进度计数。
type counters struct {
	rows      atomic.Int64
	totalRows int64
	mu        sync.Mutex
	tableRows map[string]int64 // "库/表" -> 实际导出行数
}

func (c *counters) addTableRows(db, table string, n int64) {
	c.mu.Lock()
	c.tableRows[db+"/"+table] += n
	c.mu.Unlock()
}

// Run 执行备份，返回生成的 .tar.gz 路径。
func Run(ctx context.Context, cfg dbconn.Config, o Options) (string, error) {
	o.withDefaults()
	if o.OutDir == "" {
		return "", fmt.Errorf("输出目录不能为空")
	}
	started := time.Now()

	connNoDB := cfg
	connNoDB.Database = ""
	if connNoDB.Workers < o.Workers {
		connNoDB.Workers = o.Workers
	}
	db, err := dbconn.Open(connNoDB)
	if err != nil {
		return "", err
	}
	defer db.Close()

	dbs := o.Databases
	if len(dbs) == 0 {
		dbs, err = listUserDatabases(ctx, db)
		if err != nil {
			return "", err
		}
	}
	if len(dbs) == 0 {
		return "", fmt.Errorf("没有可备份的数据库")
	}
	o.log(fmt.Sprintf("待备份数据库: %s", strings.Join(dbs, ", ")))

	var server string
	_ = db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&server)

	// 1. 先规划全部库：字符集、表清单、预估行数（用于总体进度）
	cnt := &counters{tableRows: map[string]int64{}}
	plans := make([]*dbPlan, 0, len(dbs))
	man := Manifest{Format: 1, CreatedAt: started, Server: server}
	for _, name := range dbs {
		p := &dbPlan{meta: Meta{Database: name, StartedAt: started, MySQLServer: server, TableRows: map[string]int64{}}}
		_ = db.QueryRowContext(ctx,
			"SELECT DEFAULT_CHARACTER_SET_NAME, DEFAULT_COLLATION_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?",
			name).Scan(&p.meta.Charset, &p.meta.Collation)
		p.tables, err = listTables(ctx, db, name, o.Tables)
		if err != nil {
			return "", fmt.Errorf("读取库 %s 的表清单失败: %w", name, err)
		}
		var est int64
		for _, t := range p.tables {
			if !strings.EqualFold(t.Engine, "VIEW") {
				est += t.Rows
			}
		}
		cnt.totalRows += est
		man.Databases = append(man.Databases, ManifestDB{Name: name, Charset: p.meta.Charset,
			Collation: p.meta.Collation, Tables: len(p.tables), EstRows: est})
		plans = append(plans, p)
	}

	abs, err := filepath.Abs(o.OutDir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", err
	}
	label := "multi"
	if len(dbs) == 1 {
		label = safeFileName(dbs[0])
	}
	outPath := filepath.Join(abs, fmt.Sprintf("sqlsync_%s_%s.tar.gz", label, started.Format("20060102_150405")))

	aw, err := newArchiveWriter(outPath, o.CompressLevel)
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		if !ok {
			aw.abort()
		}
	}()
	o.log("备份包: " + outPath)

	raw, _ := json.MarshalIndent(man, "", "  ")
	if err := aw.addFile(ManifestName, raw); err != nil {
		return "", err
	}

	// 2. 各库表结构先入包（恢复端读到某库数据前一定已建好表）
	var tasks []shardTask
	for _, p := range plans {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		name := p.meta.Database
		o.log(fmt.Sprintf("库 %s: 共 %d 张表（含视图），导出表结构…", name, len(p.tables)))
		var buf bytes.Buffer
		if err := dumpSchema(ctx, db, name, &buf, p.tables, o); err != nil {
			return "", fmt.Errorf("库 %s 导出表结构失败: %w", name, err)
		}
		if err := aw.addFile(name+"/schema.sql", buf.Bytes()); err != nil {
			return "", err
		}
		for _, t := range p.tables {
			if strings.EqualFold(t.Engine, "VIEW") {
				continue // 视图只有结构，没有数据
			}
			p.meta.Tables = append(p.meta.Tables, t.Name)
			tasks = append(tasks, planShards(ctx, db, t, o)...)
		}
	}

	// 3. 全部库的数据任务放进同一个 worker 池，大任务优先，减少尾部只剩一个大表在跑的情况
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].EstRows > tasks[j].EstRows })
	o.log(fmt.Sprintf("数据导出任务 %d 个，预估 %s 行，%d 个 worker 并行",
		len(tasks), progress.Count(cnt.totalRows), o.Workers))

	stopOverall := reportOverall(o, cnt, aw, started)
	err = runTasks(ctx, db, aw, tasks, o, cnt)
	stopOverall()
	if err != nil {
		return "", err
	}

	// 4. 各库 meta.json（记录实际行数）放在包末尾
	var total int64
	for _, p := range plans {
		for _, t := range p.meta.Tables {
			n := cnt.tableRows[p.meta.Database+"/"+t]
			p.meta.TableRows[t] = n
			p.meta.TotalRows += n
		}
		total += p.meta.TotalRows
		p.meta.FinishedAt = time.Now()
		raw, _ := json.MarshalIndent(p.meta, "", "  ")
		if err := aw.addFile(p.meta.Database+"/meta.json", raw); err != nil {
			return "", err
		}
	}
	if err := aw.finish(); err != nil {
		return "", err
	}
	ok = true

	elapsed := time.Since(started)
	o.Report(progress.Event{Kind: progress.KindOverall, When: time.Now(), Done: 1, Total: 1,
		Message: fmt.Sprintf("已导出 %s 行", progress.Count(total))})
	o.Report(progress.Event{Kind: progress.KindSummary, When: time.Now(),
		Message: fmt.Sprintf("备份完成: %d 个库, %s 行, 备份包 %s, 平均 %s 行/s",
			len(plans), progress.Count(total), progress.Bytes(aw.size()),
			progress.Count(int64(float64(total)/max(elapsed.Seconds(), 1))))})
	return outPath, nil
}

// reportOverall 每 500ms 上报一次总体进度（行数、速度、包大小），返回停止函数。
func reportOverall(o Options, cnt *counters, aw *archiveWriter, started time.Time) func() {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tk := time.NewTicker(500 * time.Millisecond)
		defer tk.Stop()
		lastRows, lastAt := int64(0), started
		var rate float64
		for {
			select {
			case <-done:
				return
			case now := <-tk.C:
				rows := cnt.rows.Load()
				if dt := now.Sub(lastAt).Seconds(); dt >= 2 {
					inst := float64(rows-lastRows) / dt
					if rate == 0 {
						rate = inst
					} else {
						rate = rate*0.6 + inst*0.4 // 平滑，避免数字乱跳
					}
					lastRows, lastAt = rows, now
				}
				msg := fmt.Sprintf("已导出 %s / 约 %s 行 · 已写入 %s",
					progress.Count(rows), progress.Count(cnt.totalRows), progress.Bytes(aw.size()))
				if rate > 0 {
					msg += fmt.Sprintf(" · %s 行/s", progress.Count(int64(rate)))
				}
				o.Report(progress.Event{Kind: progress.KindOverall, When: now,
					Done: rows, Total: cnt.totalRows, Message: msg})
			}
		}
	}()
	return func() { close(done); wg.Wait() }
}

// safeFileName 把库名中不适合做文件名的字符替换掉。
func safeFileName(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', ' ':
			return '_'
		}
		return r
	}, s)
}

var systemSchemas = map[string]bool{
	"mysql": true, "sys": true, "information_schema": true, "performance_schema": true,
}

func listUserDatabases(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT SCHEMA_NAME FROM information_schema.SCHEMATA")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		if !systemSchemas[strings.ToLower(s)] {
			out = append(out, s)
		}
	}
	return out, rows.Err()
}

func listTables(ctx context.Context, db *sql.DB, schema string, only []string) ([]tableInfo, error) {
	query := `SELECT TABLE_NAME, COALESCE(TABLE_ROWS,0), COALESCE(ENGINE,''), TABLE_TYPE
		FROM information_schema.TABLES WHERE TABLE_SCHEMA = ?`
	args := []interface{}{schema}
	if len(only) > 0 {
		marks := strings.TrimSuffix(strings.Repeat("?,", len(only)), ",")
		query += " AND TABLE_NAME IN (" + marks + ")"
		for _, t := range only {
			args = append(args, t)
		}
	}
	query += " ORDER BY TABLE_NAME"
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []tableInfo
	for rows.Next() {
		var t tableInfo
		var ttype string
		if err := rows.Scan(&t.Name, &t.Rows, &t.Engine, &ttype); err != nil {
			return nil, err
		}
		t.Database = schema
		if ttype == "VIEW" {
			t.Engine = "VIEW"
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// planShards 为大表规划主键区间分片任务。
func planShards(ctx context.Context, db *sql.DB, t tableInfo, o Options) []shardTask {
	pk, orderCols := keyColumns(ctx, db, t)
	t.OrderCols = orderCols

	whole := []shardTask{{tbl: t, EstRows: t.Rows}}
	if pk == "" || t.Rows < 2*o.ShardMinRows {
		return whole
	}

	var min, maxV sql.NullInt64
	q := fmt.Sprintf("SELECT MIN(%s), MAX(%s) FROM %s.%s",
		sqlfmt.QuoteIdent(pk), sqlfmt.QuoteIdent(pk),
		sqlfmt.QuoteIdent(t.Database), sqlfmt.QuoteIdent(t.Name))
	if err := db.QueryRowContext(ctx, q).Scan(&min, &maxV); err != nil || !min.Valid || !maxV.Valid {
		return whole
	}

	shards := int(t.Rows / o.ShardMinRows)
	if shards > o.ShardMax {
		shards = o.ShardMax
	}
	span := maxV.Int64 - min.Int64 + 1
	if shards < 2 || span < int64(shards) {
		return whole
	}

	t.PKCol = pk
	tasks := make([]shardTask, 0, shards)
	for i := 0; i < shards; i++ {
		lo := min.Int64 + span*int64(i)/int64(shards)
		hi := min.Int64 + span*int64(i+1)/int64(shards)
		if i == shards-1 {
			hi = maxV.Int64 + 1 // 最后一段带上右边界
		}
		tasks = append(tasks, shardTask{tbl: t, Shard: i + 1, Shards: shards, Min: lo, Max: hi,
			HasRange: true, EstRows: t.Rows / int64(shards)})
	}
	return tasks
}

// keyColumns 找用于分片与排序的列：
//   - pk: 数值型单列主键（可用于分片），无则返回空
//   - orderCols: 排序列（主键全部列，否则第一个唯一键全部列），用于分块稳定
func keyColumns(ctx context.Context, db *sql.DB, t tableInfo) (pk, orderCols string) {
	rows, err := db.QueryContext(ctx, `
		SELECT k.CONSTRAINT_NAME, k.COLUMN_NAME, c.DATA_TYPE, tc.CONSTRAINT_TYPE
		FROM information_schema.TABLE_CONSTRAINTS tc
		JOIN information_schema.KEY_COLUMN_USAGE k
		  ON k.CONSTRAINT_SCHEMA = tc.CONSTRAINT_SCHEMA
		 AND k.CONSTRAINT_NAME  = tc.CONSTRAINT_NAME
		 AND k.TABLE_SCHEMA     = tc.TABLE_SCHEMA
		 AND k.TABLE_NAME       = tc.TABLE_NAME
		JOIN information_schema.COLUMNS c
		  ON c.TABLE_SCHEMA = k.TABLE_SCHEMA AND c.TABLE_NAME = k.TABLE_NAME
		 AND c.COLUMN_NAME = k.COLUMN_NAME
		WHERE tc.TABLE_SCHEMA = ? AND tc.TABLE_NAME = ?
		  AND tc.CONSTRAINT_TYPE IN ('PRIMARY KEY','UNIQUE')
		ORDER BY (tc.CONSTRAINT_TYPE = 'PRIMARY KEY') DESC, k.CONSTRAINT_NAME, k.ORDINAL_POSITION`,
		t.Database, t.Name)
	if err != nil {
		return "", ""
	}
	defer rows.Close()

	type keyDef struct {
		cols []string
		dts  []string
	}
	keys := []keyDef{}
	byName := map[string]*keyDef{}
	for rows.Next() {
		var cname, col, dt, ctype string
		if err := rows.Scan(&cname, &col, &dt, &ctype); err != nil {
			return "", ""
		}
		kd, ok := byName[cname]
		if !ok {
			keys = append(keys, keyDef{})
			kd = &keys[len(keys)-1]
			byName[cname] = kd
		}
		kd.cols = append(kd.cols, col)
		kd.dts = append(kd.dts, dt)
	}
	pkDef, hasPK := byName["PRIMARY"]

	numeric := func(dt string) bool {
		switch strings.ToUpper(dt) {
		case "TINYINT", "SMALLINT", "MEDIUMINT", "INT", "BIGINT":
			return true
		}
		return false
	}

	var order []string
	switch {
	case hasPK:
		if len(pkDef.cols) == 1 && numeric(pkDef.dts[0]) {
			pk = pkDef.cols[0]
		}
		for _, c := range pkDef.cols {
			order = append(order, sqlfmt.QuoteIdent(c))
		}
	case len(keys) > 0:
		for _, c := range keys[0].cols {
			order = append(order, sqlfmt.QuoteIdent(c))
		}
	}
	return pk, strings.Join(order, ",")
}

// runTasks 并发执行导出任务。
func runTasks(ctx context.Context, db *sql.DB, aw *archiveWriter, tasks []shardTask, o Options, cnt *counters) error {
	if len(tasks) == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	taskCh := make(chan shardTask)
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

	workers := min(o.Workers, len(tasks))
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := db.Conn(ctx) // 每 worker 独占一条连接
			if err != nil {
				fail(err)
				return
			}
			defer conn.Close()
			// 流式读取期间客户端还要压缩，放宽服务端写超时，避免大表中途断开
			for _, q := range []string{
				"SET SESSION net_write_timeout = 3600",
				"SET SESSION net_read_timeout = 3600",
			} {
				_, _ = conn.ExecContext(ctx, q)
			}
			var buf []byte // worker 内复用的条目缓冲
			for task := range taskCh {
				if buf, err = dumpShard(ctx, conn, aw, task, o, cnt, buf); err != nil {
					if ctx.Err() == nil {
						fail(fmt.Errorf("表 %s.%s 分片 %d 导出失败: %w", task.tbl.Database, task.tbl.Name, task.Shard, err))
					}
					return
				}
			}
		}()
	}

dispatch:
	for _, t := range tasks {
		select {
		case taskCh <- t:
		case <-ctx.Done():
			break dispatch
		}
	}
	close(taskCh)
	wg.Wait()
	if firstErr == nil {
		firstErr = ctx.Err()
	}
	return firstErr
}

// dumpShard 用一条流式 SELECT 读出一个表（或其主键区间），
// 渲染成多值 INSERT，按 ChunkBytes 切成若干条目写入备份包：
//
//	<库>/data/<表>/<分片>-<序号>.sql
//
// 返回（可能扩容后的）缓冲区供下个任务复用。
func dumpShard(ctx context.Context, conn *sql.Conn, aw *archiveWriter, task shardTask,
	o Options, cnt *counters, buf []byte) ([]byte, error) {

	t := task.tbl
	label := t.Name
	if task.HasRange {
		label = fmt.Sprintf("%s#%d/%d", t.Name, task.Shard, task.Shards)
	}
	report := func(k progress.Kind, msg string, done int64) {
		o.Report(progress.Event{Kind: k, When: time.Now(), Task: t.Database + "/" + label,
			Message: msg, Done: done, Total: task.EstRows})
	}
	report(progress.KindTaskStart, "开始导出", 0)

	q := fmt.Sprintf("SELECT * FROM %s.%s", sqlfmt.QuoteIdent(t.Database), sqlfmt.QuoteIdent(t.Name))
	var args []any
	if task.HasRange {
		q += fmt.Sprintf(" WHERE %s >= ? AND %s < ?", sqlfmt.QuoteIdent(t.PKCol), sqlfmt.QuoteIdent(t.PKCol))
		args = []any{task.Min, task.Max}
	}
	rows, err := conn.QueryContext(ctx, q, args...)
	if err != nil {
		return buf, err
	}
	defer rows.Close()
	cts, err := rows.ColumnTypes()
	if err != nil {
		return buf, err
	}
	scanner := sqlfmt.NewRowScanner(cts)
	insertPrefix := "INSERT INTO " + sqlfmt.QuoteIdent(t.Name) + " " + scanner.ColumnsClause() + " VALUES "

	if need := o.ChunkBytes + o.MaxStmtBytes + 1<<16; cap(buf) < need {
		buf = make([]byte, 0, need)
	}
	buf = buf[:0]
	seq := 0
	flushChunk := func() error {
		if len(buf) == 0 {
			return nil
		}
		seq++
		name := fmt.Sprintf("%s/data/%s/%03d-%05d.sql", t.Database, t.Name, task.Shard, seq)
		err := aw.addFile(name, buf)
		buf = buf[:0]
		return err
	}

	var rowsWritten, reported int64
	stmtRows, stmtStart := 0, 0
	lastReport := time.Now()
	for rows.Next() {
		raws, err := scanner.Scan(rows)
		if err != nil {
			return buf, err
		}
		if stmtRows == 0 {
			stmtStart = len(buf)
			buf = append(buf, insertPrefix...)
		} else {
			buf = append(buf, ',')
		}
		buf = scanner.AppendRow(buf, raws)
		stmtRows++
		rowsWritten++

		if stmtRows >= o.RowsPerInsert || len(buf)-stmtStart >= o.MaxStmtBytes {
			buf = append(buf, ";\n"...)
			stmtRows = 0
			if len(buf) >= o.ChunkBytes {
				if err := flushChunk(); err != nil {
					return buf, err
				}
			}
		}
		if rowsWritten&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return buf, err
			}
			if time.Since(lastReport) >= 300*time.Millisecond {
				cnt.rows.Add(rowsWritten - reported)
				reported = rowsWritten
				lastReport = time.Now()
				report(progress.KindTaskUpdate, "导出中", rowsWritten)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return buf, err
	}
	if stmtRows > 0 {
		buf = append(buf, ";\n"...)
	}
	if err := flushChunk(); err != nil {
		return buf, err
	}
	cnt.rows.Add(rowsWritten - reported)
	cnt.addTableRows(t.Database, t.Name, rowsWritten)

	if rowsWritten == 0 {
		report(progress.KindTaskDone, "空表/空分片", 0)
	} else {
		report(progress.KindTaskDone, fmt.Sprintf("完成，%s 行", progress.Count(rowsWritten)), rowsWritten)
	}
	return buf, nil
}

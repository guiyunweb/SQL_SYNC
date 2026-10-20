// Package backup 实现 MySQL 逻辑备份：表结构与数据分离导出，
// 多表并行、大表按数值主键范围分片、键集分页读取、可选 gzip 压缩。
package backup

import (
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"sqlsync/internal/dbconn"
	"sqlsync/internal/progress"
	"sqlsync/internal/sqlfmt"
)

// Options 备份参数。
type Options struct {
	OutDir    string   // 输出根目录，库名作为一级子目录
	Databases []string // 为空表示全部非系统库
	Tables    []string // 仅备份这些表，为空不限

	Workers       int   // 并行 worker 数
	RowsPerInsert int   // 每条 INSERT 最多行数
	MaxStmtBytes  int   // 每条 INSERT 最大字节
	ChunkRows     int   // 每次 SELECT 拉取的行数
	ShardMinRows  int64 // 表行数超过该值且含数值单列主键时才分片
	ShardMax      int   // 单表最大分片数
	Gzip          bool  // 输出是否 gzip 压缩

	Report progress.Reporter
}

// warn 上报一条告警日志。
func (o *Options) warn(msg string) {
	if o.Report != nil {
		o.Report(progress.Event{Kind: progress.KindLog, When: time.Now(), Message: "⚠ " + msg})
	}
}

func (o *Options) withDefaults() {
	if o.Workers <= 0 {
		o.Workers = 8
	}
	if o.RowsPerInsert <= 0 {
		o.RowsPerInsert = 500
	}
	if o.MaxStmtBytes <= 0 {
		o.MaxStmtBytes = 8 << 20
	}
	if o.ChunkRows <= 0 {
		o.ChunkRows = 2000
	}
	if o.ShardMinRows <= 0 {
		o.ShardMinRows = 2_000_000
	}
	if o.ShardMax <= 0 {
		o.ShardMax = 8
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
	OrderCols string // 用于分块稳定排序的列（已带引号、逗号连接），可为空
}

// shardTask 一个导出任务：整表或某个主键区间。
type shardTask struct {
	tbl      tableInfo
	Shard    int // 0 表示整表导出；>0 为区间序号
	Shards   int
	Min, Max int64 // 区间 [min, max)，整表导出时无意义
	HasRange bool
}

// Meta 写入 meta.json 的备份元数据。
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
	DataBytes   int64            `json:"data_bytes"`
}

// Run 执行备份，返回输出目录。
func Run(ctx context.Context, cfg dbconn.Config, o Options) (string, error) {
	o.withDefaults()
	if o.OutDir == "" {
		return "", fmt.Errorf("输出目录不能为空")
	}

	connNoDB := cfg
	connNoDB.Database = ""
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
	o.Report(progress.Event{Kind: progress.KindLog, When: time.Now(),
		Message: fmt.Sprintf("待备份数据库: %s", strings.Join(dbs, ", "))})

	abs, err := filepath.Abs(o.OutDir)
	if err != nil {
		return "", err
	}

	for _, name := range dbs {
		if err := ctx.Err(); err != nil {
			return abs, err
		}
		if err := backupDatabase(ctx, db, name, abs, o); err != nil {
			return abs, fmt.Errorf("备份库 %s 失败: %w", name, err)
		}
	}
	return abs, nil
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

func backupDatabase(ctx context.Context, db *sql.DB, name, outRoot string, o Options) error {
	dir := filepath.Join(outRoot, name)
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		return err
	}

	var charset, collation string
	_ = db.QueryRowContext(ctx,
		"SELECT DEFAULT_CHARACTER_SET_NAME, DEFAULT_COLLATION_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?",
		name).Scan(&charset, &collation)

	meta := Meta{
		Database: name, Charset: charset, Collation: collation,
		StartedAt: time.Now(), TableRows: map[string]int64{},
	}
	_ = db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&meta.MySQLServer)

	tables, err := listTables(ctx, db, name, o.Tables)
	if err != nil {
		return err
	}
	o.Report(progress.Event{Kind: progress.KindLog, When: time.Now(),
		Message: fmt.Sprintf("库 %s: 共 %d 张表（含视图）", name, len(tables))})

	// 1. 表结构（含视图/触发器/存储过程/事件）先落盘
	if err := dumpSchema(ctx, db, name, dir, tables, o); err != nil {
		return fmt.Errorf("导出表结构失败: %w", err)
	}
	o.Report(progress.Event{Kind: progress.KindLog, When: time.Now(),
		Message: fmt.Sprintf("库 %s: 表结构已写入 schema.sql", name)})

	// 2. 规划数据导出任务
	var tasks []shardTask
	for _, t := range tables {
		if strings.EqualFold(t.Engine, "VIEW") {
			continue // 视图只有结构，没有数据
		}
		meta.Tables = append(meta.Tables, t.Name)
		meta.TableRows[t.Name] = t.Rows
		meta.TotalRows += t.Rows
		tasks = append(tasks, planShards(ctx, db, t, o)...)
	}
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].tbl.Name == tasks[j].tbl.Name {
			return tasks[i].Shard < tasks[j].Shard
		}
		return tasks[i].tbl.Name < tasks[j].tbl.Name
	})
	o.Report(progress.Event{Kind: progress.KindLog, When: time.Now(),
		Message: fmt.Sprintf("库 %s: 数据导出任务 %d 个，%d 个 worker 并行",
			name, len(tasks), o.Workers)})

	// 3. 并发导出数据
	var bytesWritten int64
	if len(tasks) > 0 {
		bytesWritten, err = runTasks(ctx, db, dir, tasks, o)
		if err != nil {
			return err
		}
	}
	meta.DataBytes = bytesWritten
	meta.FinishedAt = time.Now()

	raw, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), raw, 0o644); err != nil {
		return err
	}
	o.Report(progress.Event{Kind: progress.KindSummary, When: time.Now(), Task: name,
		Message: fmt.Sprintf("库 %s 完成: %d 张表, 约 %d 行, 数据文件 %.1f MB",
			name, len(meta.Tables), meta.TotalRows, float64(bytesWritten)/float64(1<<20))})
	return nil
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

	if pk == "" || t.Rows < o.ShardMinRows {
		return []shardTask{{tbl: t}}
	}

	var min, maxV sql.NullInt64
	q := fmt.Sprintf("SELECT MIN(%s), MAX(%s) FROM %s.%s",
		sqlfmt.QuoteIdent(pk), sqlfmt.QuoteIdent(pk),
		sqlfmt.QuoteIdent(t.Database), sqlfmt.QuoteIdent(t.Name))
	if err := db.QueryRowContext(ctx, q).Scan(&min, &maxV); err != nil || !min.Valid || !maxV.Valid {
		return []shardTask{{tbl: t}}
	}

	shards := int(t.Rows / o.ShardMinRows)
	if shards < 2 {
		return []shardTask{{tbl: t}}
	}
	if shards > o.ShardMax {
		shards = o.ShardMax
	}
	span := maxV.Int64 - min.Int64 + 1
	if span < int64(shards) {
		return []shardTask{{tbl: t}}
	}

	t.PKCol = pk
	tasks := make([]shardTask, 0, shards)
	for i := 0; i < shards; i++ {
		lo := min.Int64 + span*int64(i)/int64(shards)
		hi := min.Int64 + span*int64(i+1)/int64(shards)
		if i == shards-1 {
			hi = maxV.Int64 + 1 // 最后一段带上右边界
		}
		tasks = append(tasks, shardTask{tbl: t, Shard: i + 1, Shards: shards, Min: lo, Max: hi, HasRange: true})
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

// runTasks 并发执行导出任务，返回数据文件总字节数。
func runTasks(ctx context.Context, db *sql.DB, dbDir string, tasks []shardTask, o Options) (int64, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	taskCh := make(chan shardTask)
	var mu sync.Mutex
	var totalBytes int64
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
			conn, err := db.Conn(ctx) // 每 worker 独占一条连接
			if err != nil {
				fail(err)
				return
			}
			defer conn.Close()
			for task := range taskCh {
				n, err := dumpShard(ctx, conn, dbDir, task, o)
				mu.Lock()
				totalBytes += n
				mu.Unlock()
				if err != nil {
					fail(fmt.Errorf("表 %s 分片 %d 导出失败: %w", task.tbl.Name, task.Shard, err))
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
	return totalBytes, firstErr
}

// dumpShard 将一个表（或其主键区间）的数据写入 data/<table>[#n].sql[.gz]。
func dumpShard(ctx context.Context, conn *sql.Conn, dbDir string, task shardTask, o Options) (int64, error) {
	t := task.tbl
	label := t.Name
	fname := t.Name + ".sql"
	if task.HasRange {
		fname = fmt.Sprintf("%s#%03d.sql", t.Name, task.Shard)
		label = fmt.Sprintf("%s#%d/%d", t.Name, task.Shard, task.Shards)
	}
	if o.Gzip {
		fname += ".gz"
	}
	path := filepath.Join(dbDir, "data", fname)

	report := func(k progress.Kind, msg string, done int64) {
		o.Report(progress.Event{Kind: k, When: time.Now(), Task: t.Database + "/" + label,
			Message: msg, Done: done, Total: t.Rows})
	}
	report(progress.KindTaskStart, "开始导出", 0)

	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	var w io.Writer = f
	var gz *gzip.Writer
	if o.Gzip {
		gz = gzip.NewWriter(f)
		w = gz
	}

	header := fmt.Sprintf("-- 数据文件  库: %s  表: %s  分片: %d/%d  时间: %s\n",
		t.Database, t.Name, task.Shard, task.Shards, time.Now().Format("2006-01-02 15:04:05"))

	var rowsWritten int64
	var stmt strings.Builder
	stmt.Grow(o.MaxStmtBytes + 1<<16)
	stmtRows := 0
	var scanner *sqlfmt.RowScanner
	pkIdx := -1

	flush := func() error {
		if stmtRows == 0 {
			return nil
		}
		stmt.WriteString(";\n")
		if _, err := w.Write([]byte(stmt.String())); err != nil {
			return err
		}
		stmt.Reset()
		stmt.Grow(o.MaxStmtBytes + 1<<16)
		stmtRows = 0
		return nil
	}

	if _, err := w.Write([]byte(header)); err != nil {
		return 0, err
	}

	if !task.HasRange && t.PKCol != "" {
		// 整表导出：有数值主键的表也走键集分页（比 OFFSET 快得多）
		task.HasRange = true
		task.Min = math.MinInt64
		task.Max = math.MaxInt64
		task.Shard = 0
	}

	offset := int64(0)
	lastPK := task.Min
	firstPage := true
	useKeyset := task.HasRange && t.PKCol != ""

	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		var q string
		var args []interface{}
		switch {
		case useKeyset && firstPage:
			// 键集分页第一页：pk >= Min；后续页改用 pk > lastPK 推进
			q = fmt.Sprintf("SELECT * FROM %s.%s WHERE %s >= ? AND %s < ? ORDER BY %s LIMIT ?",
				sqlfmt.QuoteIdent(t.Database), sqlfmt.QuoteIdent(t.Name),
				sqlfmt.QuoteIdent(t.PKCol), sqlfmt.QuoteIdent(t.PKCol), sqlfmt.QuoteIdent(t.PKCol))
			args = []interface{}{lastPK, task.Max, o.ChunkRows}
		case useKeyset:
			q = fmt.Sprintf("SELECT * FROM %s.%s WHERE %s > ? AND %s < ? ORDER BY %s LIMIT ?",
				sqlfmt.QuoteIdent(t.Database), sqlfmt.QuoteIdent(t.Name),
				sqlfmt.QuoteIdent(t.PKCol), sqlfmt.QuoteIdent(t.PKCol), sqlfmt.QuoteIdent(t.PKCol))
			args = []interface{}{lastPK, task.Max, o.ChunkRows}
		case t.OrderCols != "":
			q = fmt.Sprintf("SELECT * FROM %s.%s ORDER BY %s LIMIT ? OFFSET ?",
				sqlfmt.QuoteIdent(t.Database), sqlfmt.QuoteIdent(t.Name), t.OrderCols)
			args = []interface{}{o.ChunkRows, offset}
		default:
			q = fmt.Sprintf("SELECT * FROM %s.%s LIMIT ? OFFSET ?",
				sqlfmt.QuoteIdent(t.Database), sqlfmt.QuoteIdent(t.Name))
			args = []interface{}{o.ChunkRows, offset}
		}

		rows, err := conn.QueryContext(ctx, q, args...)
		if err != nil {
			return 0, err
		}
		if scanner == nil {
			cts, err := rows.ColumnTypes()
			if err != nil {
				rows.Close()
				return 0, err
			}
			scanner = sqlfmt.NewRowScanner(cts)
			if useKeyset {
				for i, c := range scanner.Cols {
					if strings.EqualFold(c, t.PKCol) {
						pkIdx = i
						break
					}
				}
				if pkIdx < 0 {
					useKeyset = false // 主键不在结果列里（理论上不会），退回 OFFSET
				}
			}
		}

		n := 0
		for rows.Next() {
			raws, err := scanner.Scan(rows)
			if err != nil {
				rows.Close()
				return 0, err
			}
			if stmtRows == 0 {
				stmt.WriteString("INSERT INTO ")
				stmt.WriteString(sqlfmt.QuoteIdent(t.Name))
				stmt.WriteByte(' ')
				stmt.WriteString(scanner.ColumnsClause())
				stmt.WriteString(" VALUES ")
			} else {
				stmt.WriteByte(',')
			}
			scanner.AppendRow(&stmt, raws)
			stmtRows++
			rowsWritten++
			n++

			// 数值可能以字符串形式返回，解析失败则退回 OFFSET
			if useKeyset {
				if v, err := strconv.ParseInt(string(raws[pkIdx]), 10, 64); err == nil {
					lastPK = v
				} else {
					useKeyset = false
				}
			}

			if stmtRows >= o.RowsPerInsert || stmt.Len() >= o.MaxStmtBytes {
				if err := flush(); err != nil {
					rows.Close()
					return 0, err
				}
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, err
		}
		offset += int64(n)
		firstPage = false
		report(progress.KindTaskUpdate, "导出中", rowsWritten)
		if n < o.ChunkRows {
			break
		}
	}

	if err := flush(); err != nil {
		return 0, err
	}
	if gz != nil {
		if err := gz.Close(); err != nil {
			return 0, err
		}
	}
	if err := f.Sync(); err != nil {
		return 0, err
	}

	// 空分片直接删掉，恢复端少一个文件
	if rowsWritten == 0 {
		os.Remove(path)
		report(progress.KindTaskDone, "空分片", 0)
		return 0, nil
	}

	st, _ := os.Stat(path)
	var size int64
	if st != nil {
		size = st.Size()
	}
	report(progress.KindTaskDone,
		fmt.Sprintf("完成，%d 行，%.1f MB", rowsWritten, float64(size)/float64(1<<20)), rowsWritten)
	return size, nil
}

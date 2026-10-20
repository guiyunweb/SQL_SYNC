package backup

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"sqlsync/internal/sqlfmt"
)

// definerRe 去掉 DDL 里的 DEFINER，避免恢复到其它环境时因账号不存在而失败。
var definerRe = regexp.MustCompile("(?i)\\s+DEFINER\\s*=\\s*(`[^`]*`|'[^']*'|[^\\s]+)@(`[^`]*`|'[^']*'|[^\\s]+)")

// stripDBQualifier 去掉 DDL 中的 "`源库`." 限定符。
// SHOW CREATE VIEW/TRIGGER/... 会把对象名和它引用的表都带上库名，
// 改名恢复时会错误写回源库；剥掉后按恢复时的当前库解析。
func stripDBQualifier(ddl, schema string) string {
	return strings.ReplaceAll(ddl, "`"+schema+"`.", "")
}

// dumpSchema 把表、视图、触发器、存储过程、函数、事件的定义写入 dir/schema.sql。
// 每条语句用标记包裹，触发器/过程体内的分号不会干扰恢复切分。
func dumpSchema(ctx context.Context, db *sql.DB, schema, dir string, tables []tableInfo, o Options) error {
	f, err := os.Create(filepath.Join(dir, "schema.sql"))
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 1<<20)

	write := func(s string) error {
		_, err := w.WriteString(s)
		return err
	}
	writeStmt := func(s string) error {
		return write(sqlfmt.WrapStmt(s) + "\n")
	}

	if err := write(fmt.Sprintf(
		"-- 表结构备份  库: %s  时间: %s\n-- 由 sqlsync 生成，恢复时请使用本工具\n\n",
		schema, time.Now().Format("2006-01-02 15:04:05"))); err != nil {
		return err
	}
	if err := writeStmt("SET NAMES utf8mb4"); err != nil {
		return err
	}
	if err := writeStmt("SET FOREIGN_KEY_CHECKS = 0"); err != nil {
		return err
	}

	// 表与视图
	type viewDDL struct{ name, ddl string }
	var views []viewDDL
	for _, t := range tables {
		// SHOW CREATE TABLE 对表返回 2 列、对视图返回 4 列，动态列数读取，DDL 始终取第 2 列
		q := fmt.Sprintf("SHOW CREATE TABLE %s.%s",
			sqlfmt.QuoteIdent(schema), sqlfmt.QuoteIdent(t.Name))
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			return fmt.Errorf("获取 %s.%s 结构失败: %w", schema, t.Name, err)
		}
		var ddl string
		if rows.Next() {
			cols, _ := rows.Columns()
			raws := make([]sql.RawBytes, len(cols))
			dest := make([]interface{}, len(cols))
			for i := range dest {
				dest[i] = &raws[i]
			}
			if err := rows.Scan(dest...); err != nil {
				rows.Close()
				return fmt.Errorf("获取 %s.%s 结构失败: %w", schema, t.Name, err)
			}
			if len(raws) >= 2 {
				ddl = string(raws[1])
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("获取 %s.%s 结构失败: %w", schema, t.Name, err)
		}
		if ddl == "" {
			o.warn(fmt.Sprintf("%s.%s 未获取到 DDL（已跳过）", schema, t.Name))
			continue
		}
		ddl = stripDBQualifier(definerRe.ReplaceAllString(ddl, ""), schema)

		if strings.EqualFold(t.Engine, "VIEW") {
			// 视图依赖表，放到最后创建
			views = append(views, viewDDL{name: t.Name, ddl: ddl})
			continue
		}
		if err := write("\n-- 表: " + t.Name + "\n"); err != nil {
			return err
		}
		if err := writeStmt(fmt.Sprintf("DROP TABLE IF EXISTS %s", sqlfmt.QuoteIdent(t.Name))); err != nil {
			return err
		}
		if err := writeStmt(ddl); err != nil {
			return err
		}
	}

	// 触发器（失败仅告警，不阻断备份）
	triggers, err := exportTriggers(ctx, db, schema)
	if err != nil {
		o.warn("导出触发器元数据失败（已跳过）: " + err.Error())
	} else {
		for _, tr := range triggers {
			if err := writeStmt(fmt.Sprintf("DROP TRIGGER IF EXISTS %s", sqlfmt.QuoteIdent(tr.name))); err != nil {
				return err
			}
			if err := writeStmt(tr.ddl); err != nil {
				return err
			}
		}
	}

	// 存储过程 / 函数
	for _, typ := range []string{"PROCEDURE", "FUNCTION"} {
		items, err := routines(ctx, db, schema, typ)
		if err != nil {
			o.warn("导出" + typ + " 元数据失败（已跳过）: " + err.Error())
			continue
		}
		for _, r := range items {
			if err := writeStmt(fmt.Sprintf("DROP %s IF EXISTS %s", typ, sqlfmt.QuoteIdent(r.name))); err != nil {
				return err
			}
			if err := writeStmt(r.ddl); err != nil {
				return err
			}
		}
	}

	// 事件
	eventList, err := events(ctx, db, schema)
	if err != nil {
		o.warn("导出事件元数据失败（已跳过）: " + err.Error())
	} else {
		for _, e := range eventList {
			if err := writeStmt(fmt.Sprintf("DROP EVENT IF EXISTS %s", sqlfmt.QuoteIdent(e.name))); err != nil {
				return err
			}
			if err := writeStmt(e.ddl); err != nil {
				return err
			}
		}
	}

	// 视图最后创建（依赖表）
	for _, v := range views {
		if err := write("\n-- 视图: " + v.name + "\n"); err != nil {
			return err
		}
		if err := writeStmt(fmt.Sprintf("DROP VIEW IF EXISTS %s", sqlfmt.QuoteIdent(v.name))); err != nil {
			return err
		}
		if err := writeStmt(v.ddl); err != nil {
			return err
		}
	}

	if err := writeStmt("SET FOREIGN_KEY_CHECKS = 1"); err != nil {
		return err
	}
	return w.Flush()
}

type ddlItem struct {
	name string
	ddl  string
}

// exportTriggers 取全部触发器定义。
func exportTriggers(ctx context.Context, db *sql.DB, schema string) ([]ddlItem, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT TRIGGER_NAME FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = ? ORDER BY TRIGGER_NAME`, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}

	var out []ddlItem
	for _, n := range names {
		q := fmt.Sprintf("SHOW CREATE TRIGGER %s.%s", sqlfmt.QuoteIdent(schema), sqlfmt.QuoteIdent(n))
		r, err := db.QueryContext(ctx, q)
		if err != nil {
			return nil, err
		}
		if r.Next() {
			out = append(out, ddlItem{name: n, ddl: stripDBQualifier(definerRe.ReplaceAllString(scanCreateDDL(r), ""), schema)})
		}
		r.Close()
	}
	return out, nil
}

// scanCreateDDL 执行一条 SHOW CREATE ... 并取出 DDL 列（即第 2 个名字以 "Create" 或
// "SQL Original Statement" 出现的列；SHOW CREATE 结果的列数随版本与对象类型变化）。
func scanCreateDDL(r *sql.Rows) string {
	cols, _ := r.Columns()
	raws := make([]sql.RawBytes, len(cols))
	dest := make([]interface{}, len(cols))
	for i := range dest {
		dest[i] = &raws[i]
	}
	if err := r.Scan(dest...); err != nil {
		return ""
	}
	// 优先找列名以 "Create " 开头或 "SQL Original Statement" 的列
	for i, c := range cols {
		if strings.HasPrefix(c, "Create ") || c == "SQL Original Statement" {
			return string(raws[i])
		}
	}
	if len(raws) >= 3 {
		return string(raws[2])
	}
	if len(raws) >= 2 {
		return string(raws[1])
	}
	return ""
}

// routines 取存储过程/函数定义。
func routines(ctx context.Context, db *sql.DB, schema, typ string) ([]ddlItem, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT ROUTINE_NAME FROM information_schema.ROUTINES
		 WHERE ROUTINE_SCHEMA = ? AND ROUTINE_TYPE = ? ORDER BY ROUTINE_NAME`, schema, typ)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}

	var out []ddlItem
	for _, n := range names {
		q := fmt.Sprintf("SHOW CREATE %s %s.%s", typ, sqlfmt.QuoteIdent(schema), sqlfmt.QuoteIdent(n))
		r, err := db.QueryContext(ctx, q)
		if err != nil {
			return nil, err
		}
		if r.Next() {
			out = append(out, ddlItem{name: n, ddl: stripDBQualifier(definerRe.ReplaceAllString(scanCreateDDL(r), ""), schema)})
		}
		r.Close()
	}
	return out, nil
}

// events 取事件定义。
func events(ctx context.Context, db *sql.DB, schema string) ([]ddlItem, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT EVENT_NAME FROM information_schema.EVENTS WHERE EVENT_SCHEMA = ? ORDER BY EVENT_NAME`, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}

	var out []ddlItem
	for _, n := range names {
		q := fmt.Sprintf("SHOW CREATE EVENT %s.%s", sqlfmt.QuoteIdent(schema), sqlfmt.QuoteIdent(n))
		r, err := db.QueryContext(ctx, q)
		if err != nil {
			return nil, err
		}
		if r.Next() {
			out = append(out, ddlItem{name: n, ddl: stripDBQualifier(definerRe.ReplaceAllString(scanCreateDDL(r), ""), schema)})
		}
		r.Close()
	}
	return out, nil
}

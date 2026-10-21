// Package sqlfmt 提供把查询结果行渲染成 INSERT 语句所需值的工具，
// 转义规则与 MySQL 字符串字面量规则一致。
package sqlfmt

import (
	"database/sql"
	"fmt"
	"strings"
)

// numericTypes 中的类型以原始字节写出（不加引号）。
var numericTypes = map[string]bool{
	"INT": true, "TINYINT": true, "SMALLINT": true, "MEDIUMINT": true,
	"BIGINT": true, "DECIMAL": true, "FLOAT": true, "DOUBLE": true,
	"REAL": true, "NUMERIC": true, "YEAR": true,
}

// hexTypes 中的类型以 0x 十六进制写出，避免二进制安全问题。
var hexTypes = map[string]bool{
	"BIT": true, "BINARY": true, "VARBINARY": true,
	"TINYBLOB": true, "BLOB": true, "MEDIUMBLOB": true, "LONGBLOB": true,
	"GEOMETRY": true, "POINT": true, "LINESTRING": true, "POLYGON": true,
	"MULTIPOINT": true, "MULTILINESTRING": true, "MULTIPOLYGON": true,
	"GEOMETRYCOLLECTION": true,
}

// QuoteIdent 给标识符加反引号。
func QuoteIdent(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

// ColumnKind 对列类型归类：数字 / 十六进制 / 其余按字符串转义。
func ColumnKind(dbTypeName string) string {
	t := strings.ToUpper(dbTypeName)
	// 按空格分词处理 "INT UNSIGNED"、"BIGINT UNSIGNED" 等修饰组合
	for _, tok := range strings.Fields(t) {
		if numericTypes[tok] {
			return "num"
		}
	}
	if i := strings.IndexByte(t, ' '); i >= 0 {
		t = t[:i]
	}
	if hexTypes[t] {
		return "hex"
	}
	return "str"
}

// escapeTable 标记需要反斜杠转义的字节及其转义字符（0 表示原样输出）。
var escapeTable = func() (t [256]byte) {
	t[0] = '0'
	t['\''] = '\''
	t['"'] = '"'
	t['\b'] = 'b'
	t['\n'] = 'n'
	t['\r'] = 'r'
	t['\t'] = 't'
	t[0x1a] = 'Z'
	t['\\'] = '\\'
	return
}()

const hexDigits = "0123456789abcdef"

// AppendValue 把单个 RawBytes 值按列类型追加到 dst 并返回新切片。
// raw 为 nil 表示 SQL NULL。逐字节转义、零分配，是备份的热点路径。
func AppendValue(dst, raw []byte, kind string) []byte {
	if raw == nil {
		return append(dst, "NULL"...)
	}
	switch kind {
	case "num":
		if len(raw) == 0 {
			return append(dst, "NULL"...)
		}
		return append(dst, raw...)
	case "hex":
		if len(raw) == 0 {
			return append(dst, "''"...)
		}
		dst = append(dst, '0', 'x')
		for _, b := range raw {
			dst = append(dst, hexDigits[b>>4], hexDigits[b&0x0f])
		}
		return dst
	default:
		dst = append(dst, '\'')
		start := 0
		for i, b := range raw {
			if e := escapeTable[b]; e != 0 {
				dst = append(dst, raw[start:i]...)
				dst = append(dst, '\\', e)
				start = i + 1
			}
		}
		dst = append(dst, raw[start:]...)
		return append(dst, '\'')
	}
}

// RowScanner 封装把 rows 扫描成 []sql.RawBytes 的样板代码。
type RowScanner struct {
	Cols  []string
	Kinds []string
	dest  []sql.RawBytes
	ptrs  []interface{}
}

// NewRowScanner 依据列元数据构造扫描器。
func NewRowScanner(colTypes []*sql.ColumnType) *RowScanner {
	n := len(colTypes)
	s := &RowScanner{
		Cols:  make([]string, n),
		Kinds: make([]string, n),
		dest:  make([]sql.RawBytes, n),
		ptrs:  make([]interface{}, n),
	}
	for i, ct := range colTypes {
		s.Cols[i] = ct.Name()
		s.Kinds[i] = ColumnKind(ct.DatabaseTypeName())
		s.ptrs[i] = &s.dest[i]
	}
	return s
}

// Scan 绑定到当前行。
func (s *RowScanner) Scan(rows *sql.Rows) ([]sql.RawBytes, error) {
	if err := rows.Scan(s.ptrs...); err != nil {
		return nil, err
	}
	// dest 会被下一次 Scan 复用，调用方必须在下一次 Scan 前消费掉
	return s.dest, nil
}

// ColumnsClause 返回 "(`a`,`b`)" 形式的列清单。
func (s *RowScanner) ColumnsClause() string {
	var b strings.Builder
	b.WriteByte('(')
	for i, c := range s.Cols {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(QuoteIdent(c))
	}
	b.WriteByte(')')
	return b.String()
}

// AppendRow 把一行渲染成 "(v1,v2,...)" 追加到 dst 并返回新切片。
func (s *RowScanner) AppendRow(dst []byte, raws []sql.RawBytes) []byte {
	dst = append(dst, '(')
	for i, raw := range raws {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = AppendValue(dst, raw, s.Kinds[i])
	}
	return append(dst, ')')
}

// SplitStatements 按自定义标记切分 schema.sql：
// 备份时为每条语句包裹标记行，恢复时按标记还原，
// 触发器/存储过程体内可安全包含分号。
const (
	StmtBegin = "-- >>>STMT>>>"
	StmtEnd   = "-- <<<STMT<<<"
)

// WrapStmt 把一条 DDL 语句包上标记。
func WrapStmt(sql string) string {
	return fmt.Sprintf("%s\n%s\n%s\n", StmtBegin, strings.TrimRight(sql, ";"), StmtEnd)
}

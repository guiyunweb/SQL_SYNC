package restore_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"

	"sqlsync/internal/backup"
	"sqlsync/internal/dbconn"
	"sqlsync/internal/progress"
	"sqlsync/internal/restore"
)

// TestRoundTripLive 真实连库验证「备份成 .tar.gz → 改名恢复」全链路数据一致。
// 默认跳过；例如：
//
//	SQLSYNC_LIVE_DSN='root:pass@tcp(127.0.0.1:3306)/' go test -run RoundTripLive ./internal/restore
func TestRoundTripLive(t *testing.T) {
	dsn := os.Getenv("SQLSYNC_LIVE_DSN")
	if dsn == "" {
		t.Skip("未设置 SQLSYNC_LIVE_DSN，跳过真实连库测试")
	}
	mc, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	host, port, _ := strings.Cut(mc.Addr, ":")
	var p int
	fmt.Sscanf(port, "%d", &p)
	cfg := dbconn.Config{Host: host, Port: p, User: mc.User, Password: mc.Passwd, SSLMode: "preferred", Workers: 4}

	db, err := dbconn.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const src, dst = "sqlsync_rt_src", "sqlsync_rt_dst"
	mustExec := func(qs ...string) {
		t.Helper()
		for _, q := range qs {
			if _, err := db.Exec(q); err != nil {
				t.Fatalf("%v\n%s", err, q)
			}
		}
	}
	// 递归 CTE 造数需要放宽深度限制，且同一会话执行
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	connExec := func(qs ...string) {
		t.Helper()
		for _, q := range qs {
			if _, err := conn.ExecContext(context.Background(), q); err != nil {
				t.Fatalf("%v\n%s", err, q)
			}
		}
	}
	mustExec("DROP DATABASE IF EXISTS "+src, "DROP DATABASE IF EXISTS "+dst,
		"CREATE DATABASE "+src+" DEFAULT CHARACTER SET utf8mb4")
	connExec("USE "+src, "SET SESSION cte_max_recursion_depth = 1000000",
		`CREATE TABLE big (id BIGINT PRIMARY KEY, v VARCHAR(64), n DECIMAL(12,2), d DATETIME)`,
		`INSERT INTO big WITH RECURSIVE s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i < 300000)
		 SELECT i, CONCAT('row-', i, '-\'q\'-\\b'), i/7, '2024-01-01' + INTERVAL i SECOND FROM s`,
		`CREATE TABLE nokey (a INT, b TEXT)`,
		`INSERT INTO nokey WITH RECURSIVE s(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM s WHERE i < 50000)
		 SELECT i % 100, REPEAT('x', i % 50) FROM s`,
		`CREATE TABLE tricky (id INT AUTO_INCREMENT PRIMARY KEY, s VARCHAR(100), bin VARBINARY(16), j JSON, nul INT NULL, cn VARCHAR(20))`,
		`INSERT INTO tricky (s, bin, j, nul, cn) VALUES
		 ('a''b"c\\d', 0x00FF10, '{"k":"v\\n"}', NULL, '中文测试'),
		 (CONCAT('line1', CHAR(10), 'line2', CHAR(13), CHAR(9), CHAR(0), CHAR(26)), '', NULL, 5, '😀'),
		 ('', NULL, '[]', 0, NULL)`,
		`CREATE VIEW v_big AS SELECT id, v FROM big WHERE id < 10`,
		`CREATE TRIGGER trg BEFORE INSERT ON nokey FOR EACH ROW SET NEW.a = NEW.a`,
		`CREATE PROCEDURE p1() BEGIN SELECT 1; SELECT 2; END`,
		`ANALYZE TABLE big, nokey, tricky`,
	)

	out := t.TempDir()
	var overall int
	pkg, err := backup.Run(context.Background(), cfg, backup.Options{
		OutDir: out, Databases: []string{src}, Workers: 4,
		ShardMinRows: 50_000, ChunkBytes: 256 << 10, // 强制分片 + 多条目
		Report: func(e progress.Event) {
			if e.Kind == progress.KindOverall {
				overall++
			}
			if e.Kind == progress.KindLog && testing.Verbose() {
				t.Log(e.Message)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("备份包: %s，总体进度事件 %d 次", pkg, overall)

	// 标准 tar 能直接列出（验证多 member gzip 拼接的兼容性）
	if _, err := exec.LookPath("tar"); err == nil {
		outb, err := exec.Command("tar", "tzf", pkg).CombinedOutput()
		if err != nil {
			t.Fatalf("tar tzf 失败: %v\n%s", err, outb)
		}
		if !strings.HasPrefix(string(outb), "sqlsync.json\n") || !strings.Contains(string(outb), src+"/data/big/") {
			t.Fatalf("包内容不符合预期:\n%.500s", outb)
		}
	}

	names, err := restore.ListBackedUpDatabases(pkg)
	if err != nil || len(names) != 1 || names[0] != src {
		t.Fatalf("ListBackedUpDatabases = %v, %v", names, err)
	}

	if err := restore.Run(context.Background(), cfg, restore.Options{
		Dir: pkg, Rename: map[string]string{src: dst}, Create: true, Workers: 4,
	}); err != nil {
		t.Fatal(err)
	}

	for _, tbl := range []string{"big", "nokey", "tricky"} {
		a, b := checksum(t, db, src, tbl), checksum(t, db, dst, tbl)
		if a != b {
			t.Errorf("表 %s 校验和不一致: %d vs %d", tbl, a, b)
		}
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + dst + ".v_big").Scan(&n); err != nil || n != 9 {
		t.Errorf("视图恢复异常: %d %v", n, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = ?`, dst).Scan(&n); err != nil || n != 1 {
		t.Errorf("触发器恢复异常: %d %v", n, err)
	}
	mustExec("DROP DATABASE "+src, "DROP DATABASE "+dst)
}

func checksum(t *testing.T, db *sql.DB, schema, table string) int64 {
	t.Helper()
	var name string
	var sum sql.NullInt64
	if err := db.QueryRow(fmt.Sprintf("CHECKSUM TABLE `%s`.`%s`", schema, table)).Scan(&name, &sum); err != nil {
		t.Fatal(err)
	}
	return sum.Int64
}

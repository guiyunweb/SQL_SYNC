package dbconn

import (
	"net"
	"os"
	"strconv"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// TestListDatabasesLive 真实连库验证 ListDatabases。
// 默认跳过；设置环境变量后运行，例如：
//
//	SQLSYNC_LIVE_DSN='oms:oms@2026@tcp(127.0.0.1:6033)/' go test -run Live ./internal/dbconn
//
// 注意密码含 @ 时不会被正确解析，改用 SSL 参数省略的场景请直接改写 Config。
func TestListDatabasesLive(t *testing.T) {
	dsn := os.Getenv("SQLSYNC_LIVE_DSN")
	if dsn == "" {
		t.Skip("未设置 SQLSYNC_LIVE_DSN，跳过真实连库测试")
	}
	mc, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("DSN 解析失败: %v", err)
	}
	host, port := splitAddr(mc.Addr)
	cfg := Config{
		Host:     host,
		Port:     port,
		User:     mc.User,
		Password: mc.Passwd,
		SSLMode:  "skip-verify",
	}
	dbs, err := ListDatabases(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(dbs) == 0 {
		t.Fatal("库列表为空")
	}
	t.Logf("拉取到 %d 个数据库: %v", len(dbs), dbs)
	for _, s := range dbs {
		if systemSchemas[s] {
			t.Errorf("系统库不应出现在列表中: %s", s)
		}
	}
}

func splitAddr(addr string) (string, int) {
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return addr, 3306
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		return h, 3306
	}
	return h, n
}

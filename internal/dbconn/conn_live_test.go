package dbconn

import (
	"net"
	"os"
	"strconv"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// liveConn 从 DSN 解析出的连接要素。
type liveConn struct {
	host, user, pass string
	port             int
}

// envLiveDSN 读取真实连库测试的 DSN，未设置时跳过。
func envLiveDSN(t *testing.T) string {
	dsn := os.Getenv("SQLSYNC_LIVE_DSN")
	if dsn == "" {
		t.Skip("未设置 SQLSYNC_LIVE_DSN，跳过真实连库测试")
	}
	return dsn
}

// parseLiveDSN 把 'user:pass@tcp(host:port)/' 形式的 DSN 解析为 liveConn。
func parseLiveDSN(t *testing.T, dsn string) liveConn {
	mc, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("DSN 解析失败: %v", err)
	}
	h, p := splitAddr(mc.Addr)
	return liveConn{host: h, port: p, user: mc.User, pass: mc.Passwd}
}

// TestListDatabasesLive 真实连库验证 ListDatabases。
// 默认跳过；设置环境变量后运行，例如：
//
//	SQLSYNC_LIVE_DSN='user:pass@tcp(127.0.0.1:6033)/' go test -run Live ./internal/dbconn
func TestListDatabasesLive(t *testing.T) {
	c := parseLiveDSN(t, envLiveDSN(t))
	cfg := Config{
		Host:     c.host,
		Port:     c.port,
		User:     c.user,
		Password: c.pass,
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

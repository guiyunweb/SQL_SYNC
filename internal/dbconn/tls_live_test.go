package dbconn

import (
	"context"
	"database/sql"
	"testing"
)

// TestTLSActuallyEncryptedLive 验证 skip-verify 模式下连接确实完成了 TLS 协商。
// 通过查询会话状态 Ssl_cipher/Ssl_version 判断，而非仅连接成功。
// 需设置 SQLSYNC_LIVE_DSN（如 'user:pass@tcp(host:port)/'），默认跳过。
func TestTLSActuallyEncryptedLive(t *testing.T) {
	c := parseLiveDSN(t, envLiveDSN(t))
	cfg := Config{
		Host:     c.host,
		Port:     c.port,
		User:     c.user,
		Password: c.pass,
		SSLMode:  "skip-verify",
	}
	db, err := Open(cfg)
	if err != nil {
		t.Skipf("连接失败（服务器可能不支持 TLS）: %v", err)
	}
	defer db.Close()

	var cipher, version sql.NullString
	if err := db.QueryRowContext(context.Background(),
		"SHOW SESSION STATUS LIKE 'Ssl_cipher'").Scan(&cipher, &cipher); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(context.Background(),
		"SHOW SESSION STATUS LIKE 'Ssl_version'").Scan(&version, &version); err != nil {
		t.Fatal(err)
	}
	if !cipher.Valid || cipher.String == "" {
		t.Fatal("Ssl_cipher 为空：连接未加密！skip-verify 未生效或服务器退回明文")
	}
	t.Logf("TLS 协商成功: %s / %s", version.String, cipher.String)
}

// Package dbconn 负责构建到 MySQL 的连接，支持 SSL/TLS 配置。
package dbconn

import (
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Config 描述一条 MySQL 连接所需的全部参数。
type Config struct {
	Host     string
	Port     int
	User     string
	Password string
	Database string // 允许为空，表示不预选数据库

	// SSLMode: disable | preferred | skip-verify | verify-ca | verify-full
	SSLMode string
	SSLCA   string // verify-ca / verify-full 模式下使用的 CA 证书路径

	Workers int // 连接池大小参考值
}

// normalizedSSLMode 把用户输入规整为小写并校验合法性。
func (c *Config) normalizedSSLMode() (string, error) {
	m := c.SSLMode
	switch m {
	case "", "preferred":
		return "preferred", nil
	case "disable", "skip-verify", "verify-ca", "verify-full", "true":
		return m, nil
	default:
		return "", fmt.Errorf("未知 ssl-mode: %q（可选: disable|preferred|skip-verify|verify-ca|verify-full）", c.SSLMode)
	}
}

const customTLSName = "sqlsync-custom"

// Open 建立连接池并做连通性探测。
func Open(c Config) (*sql.DB, error) {
	mode, err := c.normalizedSSLMode()
	if err != nil {
		return nil, err
	}

	tlsParam := "preferred"
	switch mode {
	case "disable":
		tlsParam = "false"
	case "preferred":
		tlsParam = "preferred"
	case "true", "skip-verify":
		tlsParam = "skip-verify"
	case "verify-ca", "verify-full":
		cp := x509.NewCertPool()
		pem, err := os.ReadFile(c.SSLCA)
		if err != nil {
			return nil, fmt.Errorf("读取 CA 证书失败: %w", err)
		}
		if !cp.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("CA 证书 %s 中没有可用的 PEM 证书", c.SSLCA)
		}
		tlsCfg := &tls.Config{
			RootCAs:    cp,
			MinVersion: tls.VersionTLS12,
			// verify-ca 只校验证书链，不校验主机名（MySQL 证书常为 IP/内部域名）
			InsecureSkipVerify: mode == "verify-ca", //nolint:gosec // verify-ca 语义即跳过主机名校验
		}
		if mode == "verify-full" {
			tlsCfg.ServerName = c.Host
		}
		if err := mysql.RegisterTLSConfig(customTLSName, tlsCfg); err != nil {
			return nil, err
		}
		tlsParam = customTLSName
	}

	cfg := mysql.Config{
		User:                 c.User,
		Passwd:               c.Password,
		Net:                  "tcp",
		Addr:                 fmt.Sprintf("%s:%d", c.Host, c.Port),
		DBName:               c.Database,
		Timeout:              30 * time.Second,
		TLSConfig:            tlsParam,
		Collation:            "utf8mb4_general_ci",
		MaxAllowedPacket:     64 << 20,
		AllowNativePasswords: true,
		// 备份/恢复语句由本工具自行生成与转义，关闭参数插值以避免意外改写
		InterpolateParams: false,
		Params: map[string]string{
			"charset": "utf8mb4",
		},
	}

	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, err
	}
	pool := c.Workers + 2
	if pool < 4 {
		pool = 4
	}
	db.SetMaxOpenConns(pool)
	db.SetMaxIdleConns(pool)
	db.SetConnMaxLifetime(30 * time.Minute)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("连接 %s:%d 失败: %w", c.Host, c.Port, err)
	}
	return db, nil
}

// URL 返回便于日志展示的脱敏地址。
func (c *Config) URL() string {
	return fmt.Sprintf("%s@%s:%d/%s", url.QueryEscape(c.User), c.Host, c.Port, c.Database)
}

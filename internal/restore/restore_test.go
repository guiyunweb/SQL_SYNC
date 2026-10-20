package restore

import (
	"os"
	"path/filepath"
	"testing"
)

// TestListBackedUpDatabases 验证备份目录扫描：根目录多库、单库目录、无备份目录三种情况。
func TestListBackedUpDatabases(t *testing.T) {
	root := t.TempDir()

	// 正常库目录：schema.sql + meta.json
	dbA := filepath.Join(root, "db_alpha")
	dbB := filepath.Join(root, "db_beta")
	for _, d := range []string{dbA, dbB} {
		if err := os.MkdirAll(filepath.Join(d, "data"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "schema.sql"), []byte("-- >>>STMT>>>\nSET NAMES utf8mb4\n-- <<<STMT<<<\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// meta.json 里的原库名优先于目录名
	if err := os.WriteFile(filepath.Join(dbA, "meta.json"), []byte(`{"database":"alpha_orig"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// 干扰项：无 schema.sql 的目录应被忽略
	if err := os.MkdirAll(filepath.Join(root, "not_a_backup"), 0o755); err != nil {
		t.Fatal(err)
	}

	names, err := ListBackedUpDatabases(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "alpha_orig" || names[1] != "db_beta" {
		t.Fatalf("根目录扫描结果不符合预期: %v", names)
	}

	// 单库目录
	one, err := ListBackedUpDatabases(dbB)
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0] != "db_beta" {
		t.Fatalf("单库目录扫描结果不符合预期: %v", one)
	}

	// 空目录
	empty := t.TempDir()
	none, err := ListBackedUpDatabases(empty)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("空目录应返回空列表: %v", none)
	}
}

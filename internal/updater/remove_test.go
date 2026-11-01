package updater

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRemoveFileRetrying(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.exe")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeFileRetrying(p, time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("文件应已删除")
	}
	// 幂等：不存在视为成功。
	if err := removeFileRetrying(p, time.Second); err != nil {
		t.Fatalf("不存在应视为成功: %v", err)
	}
	// 删不掉（非空目录）时在预算内收敛返回错误。
	sub := filepath.Join(dir, "d")
	os.MkdirAll(filepath.Join(sub, "child"), 0o700)
	start := time.Now()
	if err := removeFileRetrying(sub, 100*time.Millisecond); err == nil {
		t.Fatal("非空目录应返回错误")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("预算未生效")
	}
}

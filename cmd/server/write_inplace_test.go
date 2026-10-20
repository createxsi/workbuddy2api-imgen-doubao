package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWriteFileInPlaceReadOnlyDir issue #226：目录不可写（无法建 tmp）但文件本身可写时，
// 原地覆写必须成功——这正是单文件 bind mount + PUID 运行的部署形态。
func TestWriteFileInPlaceReadOnlyDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 无视目录权限，无法复现")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"old":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if err := os.WriteFile(path+".tmp", []byte("x"), 0o600); err == nil {
		t.Fatal("前置条件：目录应不可写")
	}
	if err := writeFileInPlace(path, []byte(`{"new":true}`)); err != nil {
		t.Fatalf("in-place write: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != `{"new":true}` {
		t.Fatalf("got %s", got)
	}
}

package auth

import (
	"os"
	"path/filepath"
	"testing"
)

// 掉电把凭证文件留成全 0（长度不变、内容全空）时，账号不能凭空消失：
// 上一次写入留下的 .bak 顶上，该账号照常在池里。
func TestLoadDirRecoversZeroedAuthFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u1.json")
	a := &Auth{AccessToken: "at1", RefreshToken: "rt1", ExpiresAt: 1753600000, UID: "u1", FilePath: fp}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	a.AccessToken = "at2" // 第二次写入 → .bak 里是 at1 那份
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	st, err := os.Stat(fp)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fp, make([]byte, st.Size()), 0o600); err != nil {
		t.Fatal(err)
	}

	list, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(list) != 1 || list[0].UID != "u1" || list[0].AccessToken != "at1" {
		t.Fatalf("want u1 recovered from .bak with at1, got %+v", list)
	}
}

// 正文坏且没有 .bak：不静默跳过也不行，只能放弃这个文件——但不能影响别的账号。
func TestLoadDirSkipsUnrecoverableFile(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "workbuddy-ok.json")
	if err := os.WriteFile(good, []byte(`{"auth":{"accessToken":"at","refreshToken":"r","expiresAt":1},"account":{"uid":"ok"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "workbuddy-bad.json"), []byte(`{"auth":`), 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(list) != 1 || list[0].UID != "ok" {
		t.Fatalf("got %+v", list)
	}
}

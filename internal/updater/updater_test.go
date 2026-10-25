package updater

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/cst"
)

func TestParseVersionAndCompare(t *testing.T) {
	cases := map[string]string{
		"v1.13.4": "1.13.4", "1.13.4-panel": "1.13.4", "v1.13.5-ci": "1.13.5-ci", " v2.0.0 ": "2.0.0",
	}
	for in, want := range cases {
		v, ok := parseVersion(in)
		if !ok || v.String() != want {
			t.Errorf("parseVersion(%q) = %v,%v want %s", in, v, ok, want)
		}
	}
	for _, bad := range []string{"", "dev", "1.13", "1.13.x", "v01.2.3", "2026.10.25.1200"} {
		if _, ok := parseVersion(bad); ok {
			t.Errorf("parseVersion(%q) 应失败", bad)
		}
	}
	order := []string{"1.9.9", "1.13.4-ci", "1.13.4", "1.13.10", "2.0.0"}
	for i := 0; i+1 < len(order); i++ {
		a, _ := parseVersion(order[i])
		b, _ := parseVersion(order[i+1])
		if !a.less(b) || b.less(a) {
			t.Errorf("want %s < %s", order[i], order[i+1])
		}
	}
	cur, _ := parseVersion("1.13.4-panel")
	tag, _ := parseVersion("v1.13.4")
	if cur.less(tag) || tag.less(cur) {
		t.Error("1.13.4-panel 与 v1.13.4 应相等")
	}
}

// 资产名必须与 go-binaries.yml 的打包命名一致。
func TestAssetForMatchesWorkflow(t *testing.T) {
	v, _ := parseVersion("v1.13.4")
	cases := []struct{ goos, goarch, name, bin, kind string }{
		{"linux", "amd64", "wb2api-panel-v1.13.4-linux-amd64.tar.gz", "wb2api", "tar.gz"},
		{"linux", "arm64", "wb2api-panel-v1.13.4-linux-arm64.tar.gz", "wb2api", "tar.gz"},
		{"windows", "amd64", "wb2api-panel-doubao-windows-amd64.zip", "wb2api.exe", "zip"},
		{"darwin", "arm64", "wb2api-panel-v1.13.4-darwin-arm64.tar.gz", "wb2api", "tar.gz"},
	}
	for _, c := range cases {
		n, b, k := assetFor(v, c.goos, c.goarch)
		if n != c.name || b != c.bin || k != c.kind {
			t.Errorf("%s/%s = %s %s %s", c.goos, c.goarch, n, b, k)
		}
	}
	wf, err := os.ReadFile("../../.github/workflows/go-binaries.yml")
	if err != nil {
		t.Skip("workflow not found")
	}
	for _, frag := range []string{`wb2api-panel-doubao-windows-amd64.zip`, `wb2api-panel-v${V}-${{ matrix.goos }}-${{ matrix.goarch }}.tar.gz`, "sha256sum", "checksums.txt"} {
		if !strings.Contains(string(wf), frag) {
			t.Errorf("workflow 打包命名已变化（缺 %q），请同步 assetFor", frag)
		}
	}
}

func TestParseChecksums(t *testing.T) {
	h := strings.Repeat("ab", 32)
	text := "garbage line\n" + strings.Repeat("cd", 32) + "  other.zip\n" + strings.ToUpper(h) + " *wb2api-panel-doubao-windows-amd64.zip\n"
	got, ok := parseChecksums(text, "wb2api-panel-doubao-windows-amd64.zip")
	if !ok || got != h {
		t.Fatalf("got %q %v", got, ok)
	}
	if _, ok := parseChecksums(text, "missing.tar.gz"); ok {
		t.Fatal("缺失条目应返回 false")
	}
}

// fakeGitHub 把对 api.github.com / github.com 的请求转到本地处理器。
type rewriteTransport struct{ h http.Handler }

func (rt rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	rt.h.ServeHTTP(rec, r)
	return rec.Result(), nil
}

func newTestManager(t *testing.T, mux *http.ServeMux, goos, goarch string) *Manager {
	t.Helper()
	m := New("1.13.4-panel", "", Settings{})
	m.client = &http.Client{Transport: rewriteTransport{mux}}
	m.goos, m.goarch = goos, goarch
	m.envBlocked = func() string { return "" }
	m.executable = filepath.Join(t.TempDir(), "wb2api")
	m.mu.Lock()
	m.refreshInstallLocked()
	m.mu.Unlock()
	return m
}

func releasesJSON(rels ...release) []byte {
	b, _ := json.Marshal(rels)
	return b
}

func TestCheckPicksNewestStableAndAsset(t *testing.T) {
	asset := func(n string) releaseAsset {
		return releaseAsset{Name: n, BrowserDownloadURL: "https://github.com/o/r/releases/download/x/" + n, Size: 10}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+DefaultRepository+"/releases", func(w http.ResponseWriter, r *http.Request) {
		w.Write(releasesJSON(
			release{TagName: "v1.13.4", HTMLURL: "https://github.com/o/r/releases/tag/v1.13.4"},
			release{TagName: "v1.14.0", HTMLURL: "https://github.com/o/r/releases/tag/v1.14.0",
				Assets: []releaseAsset{asset("wb2api-panel-v1.14.0-linux-amd64.tar.gz"), asset("checksums.txt")}},
			release{TagName: "v1.15.0-ci", HTMLURL: "https://github.com/o/r/releases/tag/v1.15.0-ci", Prerelease: true},
			release{TagName: "v1.16.0", Draft: true},
		))
	})
	m := newTestManager(t, mux, "linux", "amd64")
	if err := m.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := m.Status()
	if st.LatestVersion != "1.14.0" || !st.UpdateAvailable || !st.CanInstall {
		t.Fatalf("status=%+v", st)
	}
	if st.AssetName != "wb2api-panel-v1.14.0-linux-amd64.tar.gz" || !strings.HasPrefix(st.DownloadURL, "https://github.com/") {
		t.Fatalf("asset=%q url=%q", st.AssetName, st.DownloadURL)
	}

	// windows/arm64 没有安装包：只提示，不开放安装。
	m2 := newTestManager(t, mux, "windows", "arm64")
	_ = m2.Check(context.Background())
	if st := m2.Status(); st.CanInstall || st.InstallBlocked == "" || st.DownloadURL != "" {
		t.Fatalf("缺平台包时不应可安装：%+v", st)
	}

	// 环境不支持（容器/托管）：只提示。
	m3 := newTestManager(t, mux, "linux", "amd64")
	m3.envBlocked = func() string { return "容器部署，请更新镜像" }
	_ = m3.Check(context.Background())
	if st := m3.Status(); st.CanInstall || st.InstallBlocked != "容器部署，请更新镜像" || st.DownloadURL == "" {
		t.Fatalf("环境受限时应只给下载链接：%+v", st)
	}
	m3.SetRestart(func() {})
	if err := m3.Install(context.Background()); err == nil || !strings.Contains(err.Error(), "不支持原地更新") {
		t.Fatalf("环境受限时 Install 应拒绝，got %v", err)
	}
}

func TestCheckUpToDate(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+DefaultRepository+"/releases", func(w http.ResponseWriter, r *http.Request) {
		w.Write(releasesJSON(release{TagName: "v1.13.4"}))
	})
	m := newTestManager(t, mux, "linux", "amd64")
	if err := m.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.Status().UpdateAvailable {
		t.Fatal("同版本不应提示更新")
	}
}

func tarGz(t *testing.T, name string, body []byte) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "README.md", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg})
	tw.Write([]byte("hi"))
	tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
	tw.Write(body)
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func zipOf(t *testing.T, name string, body []byte) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create(name)
	w.Write(body)
	zw.Close()
	return buf.Bytes()
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// 下载 → checksums 校验 → 解包暂存：成功路径与各失败路径都不得改动现有程序。
func TestDownloadVerifyStage(t *testing.T) {
	bin := []byte("new-binary-content")
	tgz := tarGz(t, "wb2api", bin)
	zp := zipOf(t, "wb2api.exe", bin)
	mux := http.NewServeMux()
	serve := func(b []byte) http.HandlerFunc { return func(w http.ResponseWriter, r *http.Request) { w.Write(b) } }
	mux.HandleFunc("/dl/a.tar.gz", serve(tgz))
	mux.HandleFunc("/dl/a.zip", serve(zp))
	mux.HandleFunc("/dl/evil", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example.com/x", http.StatusFound)
	})
	m := newTestManager(t, mux, "linux", "amd64")
	dir := t.TempDir()

	for _, c := range []struct {
		url, bin, kind string
		data           []byte
	}{
		{"https://github.com/dl/a.tar.gz", "wb2api", "tar.gz", tgz},
		{"https://github.com/dl/a.zip", "wb2api.exe", "zip", zp},
	} {
		a := &releaseAsset{BrowserDownloadURL: c.url, Size: int64(len(c.data))}
		stage, err := m.downloadAndStage(context.Background(), a, sha(c.data), dir, c.bin, c.kind)
		if err != nil {
			t.Fatalf("%s: %v", c.kind, err)
		}
		got, _ := os.ReadFile(stage)
		if !bytes.Equal(got, bin) {
			t.Fatalf("%s: 暂存内容不符", c.kind)
		}
		os.Remove(stage)
	}

	a := &releaseAsset{BrowserDownloadURL: "https://github.com/dl/a.tar.gz", Size: int64(len(tgz))}
	if _, err := m.downloadAndStage(context.Background(), a, strings.Repeat("0", 64), dir, "wb2api", "tar.gz"); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("摘要不符应失败，got %v", err)
	}
	if _, err := m.downloadAndStage(context.Background(), &releaseAsset{BrowserDownloadURL: "https://github.com/dl/a.tar.gz", Size: 5}, sha(tgz), dir, "wb2api", "tar.gz"); err == nil {
		t.Fatal("大小不符应失败")
	}
	if _, err := m.downloadAndStage(context.Background(), &releaseAsset{BrowserDownloadURL: "https://github.com/dl/evil", Size: 1}, sha(tgz), dir, "wb2api", "tar.gz"); err == nil {
		t.Fatal("重定向到非 GitHub 地址应失败")
	}
	if _, err := m.downloadAndStage(context.Background(), &releaseAsset{BrowserDownloadURL: "http://github.com/dl/a.tar.gz", Size: 1}, sha(tgz), dir, "wb2api", "tar.gz"); err == nil {
		t.Fatal("非 HTTPS 应失败")
	}
	if _, err := m.downloadAndStage(context.Background(), a, sha(tgz), dir, "wb2api.exe", "tar.gz"); err == nil {
		t.Fatal("包内缺程序应失败")
	}
	// 所有失败都不应留下临时文件。
	left, _ := os.ReadDir(dir)
	if len(left) != 0 {
		t.Fatalf("残留临时文件：%v", left)
	}
}

func TestSwapAndStartRollback(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "wb2api")
	write := func(p, s string) { os.WriteFile(p, []byte(s), 0o755) }
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }

	// 成功：新程序就位，备份清理。
	write(target, "old")
	write(filepath.Join(dir, "stage1"), "new")
	if err := swapAndStart(filepath.Join(dir, "stage1"), target, func(time.Duration) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if read(target) != "new" {
		t.Fatal("应换成新程序")
	}
	if _, err := os.Stat(target + ".old"); !os.IsNotExist(err) {
		t.Fatal("成功后应清理备份")
	}

	// 新版本启动失败：恢复旧程序并以旧版本重新拉起。
	write(target, "old")
	write(filepath.Join(dir, "stage2"), "broken")
	var calls []string
	err := swapAndStart(filepath.Join(dir, "stage2"), target, func(probe time.Duration) error {
		calls = append(calls, read(target))
		if probe > 0 {
			return errors.New("exit 1")
		}
		return nil
	})
	if err == nil || read(target) != "old" {
		t.Fatalf("应回滚到旧程序：err=%v target=%q", err, read(target))
	}
	if len(calls) != 2 || calls[0] != "broken" || calls[1] != "old" {
		t.Fatalf("应先试新版本再拉起旧版本，got %v", calls)
	}

	// 暂存文件缺失（替换失败）：旧程序原样保留并被拉起。
	write(target, "old")
	calls = nil
	err = swapAndStart(filepath.Join(dir, "missing"), target, func(time.Duration) error {
		calls = append(calls, read(target))
		return nil
	})
	if err == nil || read(target) != "old" || len(calls) != 1 || calls[0] != "old" {
		t.Fatalf("替换失败应保留并拉起旧程序：err=%v target=%q calls=%v", err, read(target), calls)
	}
}

func TestScheduleDue(t *testing.T) {
	m := New("1.13.4-panel", "", Settings{AutoEnabled: true, AutoTime: "04:00"})
	at := time.Date(2026, 10, 26, 4, 0, 30, 0, cst.Loc)
	auto, regular := m.scheduleDue(at)
	if !auto || !regular {
		t.Fatalf("首次应同时到点：auto=%v regular=%v", auto, regular)
	}
	if auto, _ := m.scheduleDue(at.Add(20 * time.Second)); auto {
		t.Fatal("同一天只自动安装一次")
	}
	m.lastCheck = at
	if _, regular := m.scheduleDue(at.Add(time.Hour)); regular {
		t.Fatal("6 小时内不重复检查")
	}
	if _, regular := m.scheduleDue(at.Add(6 * time.Hour)); !regular {
		t.Fatal("满 6 小时应检查")
	}
	m.Configure(Settings{AutoEnabled: false, AutoTime: "05:00"})
	if auto, _ := m.scheduleDue(time.Date(2026, 10, 27, 5, 0, 0, 0, cst.Loc)); auto {
		t.Fatal("关闭后不应自动安装")
	}
	if st := m.Status(); st.AutoEnabled || st.AutoTime != "05:00" {
		t.Fatalf("热配置未生效：%+v", st)
	}
}

func TestHandleSelfUpdateIgnoresNormalArgs(t *testing.T) {
	if handled, _ := HandleSelfUpdate([]string{"wb2api", "-config", "c.json"}); handled {
		t.Fatal("普通启动不应被拦截")
	}
}

func TestTrustedURL(t *testing.T) {
	ok := []string{"https://github.com/a", "https://objects.githubusercontent.com/x", "https://release-assets.githubusercontent.com/x"}
	for _, s := range ok {
		u, _ := url.Parse(s)
		if !isTrustedGitHubURL(u, false) {
			t.Errorf("%s 应可信", s)
		}
	}
	for _, s := range []string{"http://github.com/a", "https://github.com.evil.io/a", "https://user@github.com/a", "https://github.com:8443/a"} {
		u, _ := url.Parse(s)
		if isTrustedGitHubURL(u, false) {
			t.Errorf("%s 不应可信", s)
		}
	}
	u, _ := url.Parse("https://objects.githubusercontent.com/x")
	if isTrustedGitHubURL(u, true) {
		t.Error("初始下载地址只接受 github.com")
	}
}

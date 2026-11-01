package updater

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/cst"
)

// 进程晚于目标时刻启动、或 ticker 跨过目标分钟，当天仍应补跑一次。
func TestScheduleDueWindow(t *testing.T) {
	m := New("1.13.4-panel", "", Settings{AutoEnabled: true, AutoTime: "4:00"})
	if auto, _ := m.scheduleDue(time.Date(2026, 10, 26, 3, 59, 0, 0, cst.Loc)); auto {
		t.Fatal("未到时刻不应触发")
	}
	if auto, _ := m.scheduleDue(time.Date(2026, 10, 26, 9, 30, 0, 0, cst.Loc)); !auto {
		t.Fatal("已过目标时刻且当天未跑应触发")
	}
	if auto, _ := m.scheduleDue(time.Date(2026, 10, 26, 10, 0, 0, 0, cst.Loc)); auto {
		t.Fatal("同一天只触发一次")
	}
}

// 下载卡住不动时按停滞窗口中止，而不是等整体超时或永久挂起。
func TestDownloadStallGuard(t *testing.T) {
	old := downloadStallWindow
	downloadStallWindow = 150 * time.Millisecond
	t.Cleanup(func() { downloadStallWindow = old })
	block := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.Write([]byte("abc"))
		w.(http.Flusher).Flush()
		<-block
	}))
	defer srv.Close()
	defer close(block)
	m := New("1.13.4-panel", "", Settings{})
	m.client = srv.Client()
	var sb strings.Builder
	start := time.Now()
	// download 有 GitHub 域名白名单，这里直接验证 stallWriter 与取消联动。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stall := time.AfterFunc(downloadStallWindow, cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := m.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, err = io.Copy(&stallWriter{w: &sb, t: stall, d: downloadStallWindow}, resp.Body)
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("应在停滞窗口后中止: err=%v elapsed=%v", err, time.Since(start))
	}
}

func TestValidateHelperFile(t *testing.T) {
	dir := t.TempDir()
	if err := validateHelperFile(filepath.Join(dir, ".wb2api-stage-1"), dir); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{filepath.Join(dir, "config.json"), filepath.Join(os.TempDir(), ".wb2api-x")} {
		if validateHelperFile(bad, dir) == nil {
			t.Errorf("应拒绝 %s", bad)
		}
	}
}

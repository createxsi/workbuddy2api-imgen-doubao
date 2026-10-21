package scheduler

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestClaimBenefitsNoCreditIsNotLogged 上游对「已领过 / 本次无发放」回 code=0 且不带
// credit 字段：不得记成「🎊 +0c 到账」；真带 credit 时照常记。
func TestClaimBenefitsNoCreditIsNotLogged(t *testing.T) {
	body := `{"code":0,"data":{}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer srv.Close()

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: pool.New(""), Upstream: up})
	a := &auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999}

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	s.claimBenefits(a)
	if strings.Contains(buf.String(), "🎊") {
		t.Fatalf("无 credit 的回执不应记到账: %s", buf.String())
	}

	body = `{"code":0,"data":{"credit":500}}`
	buf.Reset()
	s.claimBenefits(a)
	if !strings.Contains(buf.String(), "补偿领取 +500c") {
		t.Fatalf("真到账应记日志: %s", buf.String())
	}
}

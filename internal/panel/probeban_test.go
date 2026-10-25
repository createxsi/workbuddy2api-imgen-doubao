package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func probeVerdict(t *testing.T, balanceStatus int, balanceBody string, refreshStatus int) (string, bool) {
	t.Helper()
	up := &upstream.Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			st, body := balanceStatus, balanceBody
			if strings.Contains(r.URL.Path, "refresh") {
				st, body = refreshStatus, `{"code":0,"data":{"accessToken":"at2","refreshToken":"rt2","expiresIn":3600}}`
				if refreshStatus != 200 {
					body = `{"code":11001,"msg":"invalid refresh token"}`
				}
			}
			return &http.Response{StatusCode: st, Header: http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(strings.NewReader(body))}, nil
		})},
		ChatBaseCN: "https://fake.example", BillingBaseCN: "https://fake.example",
	}
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	pl.Disable("u1", "account restricted by upstream (11140 request illegal)")
	p := New(Config{Version: "test", APIKey: "k", Pool: pl, Upstream: up})
	req := httptest.NewRequest("POST", "/panel/api/accounts/u1/probe_ban", nil)
	req.Header.Set("Authorization", "Bearer k")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body)
	}
	var out struct{ Verdict string }
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	st, _ := pl.Status("u1")
	return out.Verdict, st.Disabled
}

// 探活只读：任何结论都不改变禁用状态。
func TestProbeBanVerdicts(t *testing.T) {
	ok := `{"code":0,"data":{"Response":{"Data":{"Packages":[]}}}}`
	if v, dis := probeVerdict(t, 200, ok, 200); v != "lifted" || !dis {
		t.Fatalf("200 → %s disabled=%v", v, dis)
	}
	banned := `{"code":11140,"msg":"request illegal"}`
	if v, dis := probeVerdict(t, 403, banned, 200); v != "still_banned" || !dis {
		t.Fatalf("11140 → %s disabled=%v", v, dis)
	}
	if v, dis := probeVerdict(t, 418, `{"code":1,"msg":"x"}`, 200); v != "inconclusive" || !dis {
		t.Fatalf("418 → %s disabled=%v", v, dis)
	}
}

package server

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

// TestBridgeNoSuperfluousWriteHeaderOverHTTP 真实 net/http 下各种上游形态都不应出现
// "superfluous response.WriteHeader call"（流式请求上游回 JSON 时曾触发）。
func TestBridgeNoSuperfluousWriteHeaderOverHTTP(t *testing.T) {
	roleOnly := "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n"
	ups := []struct {
		st     int
		body   string
		stream bool
	}{
		{200, sseToolTurn, true}, {200, "data: [DONE]\n\n", true}, {200, roleOnly + "data: {\"error\":{\"message\":\"x\",\"code\":6004}}\n\n", true},
		{200, "", true}, {429, `{"code":6004,"msg":"rate"}`, false}, {400, `{"code":11101,"msg":"Unmarshal chat params failed"}`, false},
		{500, `oops`, false}, {200, `{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`, false},
	}
	bodies := map[string]string{
		"/v1/chat/completions": `{"model":"hy3","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		"/v1/messages":         `{"model":"hy3","stream":true,"max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`,
		"/v1/responses":        `{"model":"hy3","stream":true,"input":"hi"}`,
		"/v1/messages#s":       `{"model":"hy3","stream":true,"max_tokens":10,"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"hi"}]}`,
		"/v1/responses#s":      `{"model":"hy3","stream":true,"tools":[{"type":"web_search"}],"input":"hi"}`,
	}
	for _, u := range ups {
		for p, b := range bodies {
			u := u
			h := NewHandler(Config{
				Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
				Upstream:   newFakeUpstream(t, func(string) (int, string, bool) { return u.st, u.body, u.stream }),
				RequestLog: reqlog.New(reqlog.Config{}),
			})
			var buf bytes.Buffer
			srv := httptest.NewUnstartedServer(h)
			srv.Config.ErrorLog = log.New(&buf, "", 0)
			srv.Start()
			path := strings.Split(p, "#")[0]
			resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(b))
			if err == nil {
				io.ReadAll(resp.Body)
				resp.Body.Close()
			}
			srv.Close()
			if strings.Contains(buf.String(), "superfluous") {
				t.Errorf("superfluous WriteHeader: %s up=%d %.30q", p, u.st, u.body)
			}
		}
	}
}

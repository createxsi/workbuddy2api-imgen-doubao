package prompt

import (
	"encoding/json"
	"testing"
)

func msgsOf(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var obj struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatal(err)
	}
	return obj.Messages
}

// 只拆最长的 system，文本按句界切开、一字不改，仍是 system。
func TestSplitSystemMessagesOnlyLongest(t *testing.T) {
	in := `{"messages":[{"role":"system","content":"Be a pirate."},{"role":"system","content":"Use @ paths. Paths are relative to root. Version 3.5 is fine!"},{"role":"user","content":"hi"}]}`
	out, ok := SplitSystemMessages([]byte(in))
	if !ok {
		t.Fatal("应能拆")
	}
	m := msgsOf(t, out)
	want := []string{"Be a pirate.", "Use @ paths.", "Paths are relative to root.", "Version 3.5 is fine!", "hi"}
	if len(m) != len(want) {
		t.Fatalf("got %d msgs: %v", len(m), m)
	}
	for i, w := range want {
		if m[i]["content"] != w {
			t.Errorf("msg %d = %v want %q", i, m[i]["content"], w)
		}
	}
	if m[3]["role"] != "system" {
		t.Error("拆出的段应仍为 system")
	}
}

func TestSplitSystemSingleSentenceFallsToMove(t *testing.T) {
	in := []byte(`{"messages":[{"role":"system","content":"You are a helpful pirate"},{"role":"user","content":"hi"}]}`)
	if _, ok := SplitSystemMessages(in); ok {
		t.Fatal("单句不应拆")
	}
	out, ok := MoveSystemToUser(in, "neutral")
	if !ok {
		t.Fatal("应能搬")
	}
	m := msgsOf(t, out)
	if m[0]["content"] != "neutral" || m[1]["content"] != "You are a helpful pirate\n\nhi" {
		t.Fatalf("搬家结果不符: %v", m)
	}
	// 多模态 user 不搬。
	if _, ok := MoveSystemToUser([]byte(`{"messages":[{"role":"system","content":"x"},{"role":"user","content":[{"type":"text","text":"hi"}]}]}`), "n"); ok {
		t.Fatal("多模态 user 不应搬")
	}
}

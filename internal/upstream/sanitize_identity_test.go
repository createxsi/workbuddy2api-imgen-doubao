package upstream

import "testing"

func TestSanitizeIdentitySentences(t *testing.T) {
	cases := []struct{ in, want string }{
		{"You are an AI agent powered by DeepSeek Harness.", "You are a helpful AI assistant."},
		{"You are a coding agent powered by the deepseek-v4.1-flash model. Be brief.", "You are a helpful AI assistant. Be brief."},
		{"You are an AI assistant built on DeepSeek.", "You are a helpful AI assistant."},
		{"you are an ai agent POWERED BY deepseek harness.", "You are a helpful AI assistant."},
		{"# Role\nYou are an AI agent powered by DeepSeek Harness.\nFollow rules.", "# Role\nYou are a helpful AI assistant.\nFollow rules."},
		{"You are Foo Agent, built by Bar\nnext", "You are a helpful AI assistant.\nnext"},
		{"You are a helpful AI assistant.", "You are a helpful AI assistant."},
		{"DeepSeek Harness is great.", "DeepSeek Harness is great."},
		{"You are free to use any tool you like; plugins are powered by X.", "You are free to use any tool you like; plugins are powered by X."},
	}
	for _, c := range cases {
		if got := sanitizeIdentity(c.in); got != c.want {
			t.Errorf("sanitizeIdentity(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSanitizeIdentityOnlySystemRoles(t *testing.T) {
	const s = "You are an AI agent powered by DeepSeek Harness."
	msgs := []any{
		map[string]any{"role": "system", "content": s},
		map[string]any{"role": "developer", "content": []any{map[string]any{"type": "text", "text": s}}},
		map[string]any{"role": "user", "content": s},
		map[string]any{"role": "tool", "content": s},
	}
	if !sanitizeMessages(msgs) {
		t.Fatal("expected change")
	}
	if got := msgs[0].(map[string]any)["content"]; got != sanitizeIdentityReplacement {
		t.Errorf("system = %q", got)
	}
	part := msgs[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if part["text"] != sanitizeIdentityReplacement {
		t.Errorf("developer = %q", part["text"])
	}
	for _, i := range []int{2, 3} {
		if got := msgs[i].(map[string]any)["content"]; got != s {
			t.Errorf("msg %d changed: %q", i, got)
		}
	}
}

func TestSanitizeOmoJunior(t *testing.T) {
	in := "Sisyphus-Junior - Focused executor from OhMyOpenCode. Do it."
	if got := sanitizeText(in); got != "Sisyphus-Junior - Focused executor. Do it." {
		t.Errorf("got %q", got)
	}
	if got := sanitizeText("Sisyphus-Junior alone"); got != "Sisyphus-Junior alone" {
		t.Errorf("got %q", got)
	}
}

package upstream

import "testing"

func TestNoHealthyAccountHintForModel(t *testing.T) {
	want := `model "glm-5.3" is only available in the global realm; retry with "global:glm-5.3"`
	if got := NoHealthyAccountHintForModel("glm-5.3", "global"); got != want {
		t.Errorf("got %q want %q", got, want)
	}
	for _, c := range []struct{ bare, other string }{{"glm-5.3", ""}, {"", "global"}} {
		if got := NoHealthyAccountHintForModel(c.bare, c.other); got != noHealthyHint {
			t.Errorf("(%q,%q) = %q, want fallback", c.bare, c.other, got)
		}
	}
}

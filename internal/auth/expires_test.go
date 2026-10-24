package auth

import (
	"fmt"
	"testing"
	"time"
)

// 官方 CLI 凭据的 expiresAt 是毫秒：解析时须归一为秒，否则 token 永不刷新。
func TestParseNormalizesMillisExpiresAt(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	for _, raw := range []string{
		fmt.Sprintf(`{"auth":{"accessToken":"t","expiresAt":%d},"account":{"uid":"u"}}`, exp.UnixMilli()),
		fmt.Sprintf(`{"accessToken":"t","uid":"u","expiresAt":%d}`, exp.UnixMilli()),
	} {
		a, err := Parse([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if a.ExpiresAt != exp.Unix() {
			t.Errorf("ExpiresAt=%d want %d", a.ExpiresAt, exp.Unix())
		}
		if !a.NeedsRefresh(2 * time.Hour) {
			t.Error("NeedsRefresh(2h) 应为真：到期时间仍被当成毫秒")
		}
	}
}

func TestNormalizeExpiresAt(t *testing.T) {
	cases := []struct{ in, want int64 }{
		{0, 0},
		{1795254516, 1795254516},
		{expiresAtMillisThreshold, expiresAtMillisThreshold},
		{1795254516410, 1795254516},
	}
	for _, c := range cases {
		if got := normalizeExpiresAt(c.in); got != c.want {
			t.Errorf("normalizeExpiresAt(%d)=%d want %d", c.in, got, c.want)
		}
	}
}

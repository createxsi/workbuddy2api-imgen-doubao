package scheduler

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/cst"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func TestJitterFracStableAndBounded(t *testing.T) {
	setJitterSeed(12345)
	defer setJitterSeed(0)
	a := jitterFrac("bal|1")
	if a != jitterFrac("bal|1") {
		t.Fatal("同种子同 key 必须稳定")
	}
	seen := map[float64]bool{}
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		f := jitterFrac(k)
		if f < 0 || f >= 1 {
			t.Fatalf("frac %v 越界", f)
		}
		seen[f] = true
	}
	if len(seen) < 4 {
		t.Fatalf("不同 key 应散开，got %d", len(seen))
	}
	setJitterSeed(999)
	if jitterFrac("bal|1") == a {
		t.Fatal("换种子应换相位")
	}
}

func TestPeriodicJitterBoundsAndVaries(t *testing.T) {
	iv := 5 * time.Minute
	if periodicJitter(iv, 0, "k") != 0 || periodicJitter(0, 0.2, "k") != 0 {
		t.Fatal("关闭时必须为 0")
	}
	setJitterSeed(42)
	defer setJitterSeed(0)
	distinct := map[time.Duration]bool{}
	for r := 1; r <= 50; r++ {
		d := periodicJitter(iv, 0.2, "bal|"+strconv.Itoa(r))
		if d < -time.Minute || d > time.Minute {
			t.Fatalf("偏移 %v 超出 ±1m", d)
		}
		distinct[d] = true
	}
	if len(distinct) < 10 {
		t.Fatalf("逐轮偏移应变化，got %d 种", len(distinct))
	}
}

func TestStaggerDelay(t *testing.T) {
	if staggerWindow(5*time.Minute, 0) != 0 || staggerDelay(0, 1, "a") != 0 {
		t.Fatal("关闭时不得错峰")
	}
	setJitterSeed(7)
	defer setJitterSeed(0)
	w := staggerWindow(5*time.Minute, 0.2)
	if w != time.Minute {
		t.Fatalf("window=%v want 1m", w)
	}
	seen := map[time.Duration]bool{}
	for _, uid := range []string{"a", "b", "c", "d", "e"} {
		d := staggerDelay(w, 1, uid)
		if d < 0 || d >= w {
			t.Fatalf("%s 偏移 %v 超出 [0,%v)", uid, d, w)
		}
		seen[d] = true
	}
	if len(seen) < 4 {
		t.Fatalf("账号间应散开，got %d", len(seen))
	}
	if staggerDelay(w, 1, "a") == staggerDelay(w, 2, "a") {
		t.Fatal("同账号不同轮次应换位置")
	}
}

func TestSetBalanceJitterClamps(t *testing.T) {
	s := New(Config{})
	s.SetBalanceJitter(-3)
	if s.BalanceJitterPermill() != 0 {
		t.Fatal("负值应钳为 0")
	}
	s.SetBalanceJitter(900)
	if s.BalanceJitterPermill() != MaxJitterPermill {
		t.Fatal("超上限应钳为 500")
	}
	s.SetNightAccountCap(-1)
	if s.NightAccountCap() != 0 {
		t.Fatal("负上限应视为不限")
	}
}

func TestCapByCredits(t *testing.T) {
	mk := func(uid string, c int64) pool.Status { return pool.Status{UID: uid, Credits: c} }
	list := []pool.Status{mk("a", 3), mk("b", 9)}
	if got := capByCredits(list, 0); len(got) != 2 {
		t.Fatal("0 = 不限")
	}
	if got := capByCredits(list, 5); len(got) != 2 {
		t.Fatal("不超上限原样返回")
	}
	list = []pool.Status{mk("a", 100), mk("b", 500), mk("c", 300), mk("d", 400), mk("e", 200)}
	got := capByCredits(list, 2)
	if len(got) != 2 || got[0].UID != "b" || got[1].UID != "d" {
		t.Fatalf("want [b d], got %v", got)
	}
	if list[0].UID != "a" {
		t.Fatal("不得改动入参顺序")
	}
}

// TestBlackcatNightCap 夜猫子上限：只有积分最高的前 N 个合格账号发起上游调用；
// 不合格账号（禁用）不占名额。
func TestBlackcatNightCap(t *testing.T) {
	restoreClock := nowForNightWindow
	nowForNightWindow = func() time.Time { return time.Date(2026, 10, 7, 23, 30, 0, 0, cst.Loc) }
	t.Cleanup(func() { nowForNightWindow = restoreClock })

	var mu sync.Mutex
	hit := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v2/activity/growth/tasks") {
			mu.Lock()
			hit[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")] = true
			mu.Unlock()
		}
		w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
	}))
	defer srv.Close()

	p := pool.New("")
	for _, uid := range []string{"u1", "u2", "u3", "u4"} {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at-" + uid, RefreshToken: "rt", ExpiresAt: 9999999999})
	}
	p.SetCredits("u1", 100, 1000)
	p.SetCredits("u2", 900, 1000) // 积分最高但禁用：不占名额
	p.SetCredits("u3", 500, 1000)
	p.SetCredits("u4", 300, 1000)
	p.Disable("u2", "manual")

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	s.SetNightAccountCap(2)
	s.RunBlackcatNow()

	snapshot := func() map[string]bool {
		mu.Lock()
		defer mu.Unlock()
		out := hit
		hit = map[string]bool{}
		return out
	}
	if got := snapshot(); !got["at-u3"] || !got["at-u4"] || got["at-u1"] || got["at-u2"] {
		t.Fatalf("want 只有 u3/u4 参加，got %v", got)
	}

	// 上限 0 = 全部合格账号参加（旧行为）。
	s.SetNightAccountCap(0)
	s.RunBlackcatNow()
	if got := snapshot(); !got["at-u1"] || !got["at-u3"] || !got["at-u4"] || got["at-u2"] {
		t.Fatalf("不限时应为全部合格账号，got %v", got)
	}
}

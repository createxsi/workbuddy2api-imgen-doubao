package scheduler

import (
	"strconv"
	"testing"
	"time"
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
}

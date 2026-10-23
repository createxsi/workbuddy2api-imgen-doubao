package scheduler

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

func TestSpreadOffsetStableAndInWindow(t *testing.T) {
	w := 30 * time.Minute
	a := spreadOffset("checkin", "u1", "2026-10-24", w)
	if a != spreadOffset("checkin", "u1", "2026-10-24", w) {
		t.Fatal("same inputs must give same offset")
	}
	if a < 0 || a >= w {
		t.Fatalf("offset %v out of window", a)
	}
	if spreadOffset("checkin", "u1", "2026-10-24", 0) != 0 {
		t.Fatal("zero window must give zero offset")
	}
}

func TestPlanSpreadSortedWithGap(t *testing.T) {
	var list []pool.Status
	for _, u := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		list = append(list, pool.Status{UID: u})
	}
	gap := 90 * time.Second
	out, offs := planSpread(list, "keepalive", "2026-10-24", 10*time.Minute, gap)
	if len(out) != len(list) {
		t.Fatalf("len %d", len(out))
	}
	for i := 1; i < len(out); i++ {
		d := offs[out[i].UID] - offs[out[i-1].UID]
		if d < gap {
			t.Fatalf("gap %v < %v between %s and %s", d, gap, out[i-1].UID, out[i].UID)
		}
	}
}

func TestClampSpread(t *testing.T) {
	w, g := clampSpread(3*time.Hour, -time.Second)
	if w != 45*time.Minute || g != 0 {
		t.Fatalf("got %v %v", w, g)
	}
}

package cst

import (
	"testing"
	"time"
)

// 日界必须按 CST：UTC 主机上 2026-10-21T17:00Z 已经是北京 10-22 01:00。
func TestDayIsUTCPlusEight(t *testing.T) {
	utc := time.Date(2026, 10, 21, 17, 0, 0, 0, time.UTC)
	if got := Day(utc); got != "2026-10-22" {
		t.Fatalf("Day(UTC 17:00) = %s，应为 CST 次日 2026-10-22", got)
	}
	if got := Day(time.Date(2026, 10, 21, 15, 59, 0, 0, time.UTC)); got != "2026-10-21" {
		t.Fatalf("Day(UTC 15:59) = %s，应为 2026-10-21", got)
	}
}

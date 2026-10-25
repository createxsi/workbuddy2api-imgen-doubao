package server

import (
	"strings"
	"testing"
	"time"
)

// 上游每帧都带 "tool_calls":[]，空数组不算见过调用。
func TestStatsReaderSeesNoToolCallsOnStop(t *testing.T) {
	s := newChatStatsReaderSince(strings.NewReader(""), time.Now())
	feedStatsLines(s,
		`data: {"choices":[{"delta":{"content":"。","tool_calls":[]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":"","tool_calls":[]},"finish_reason":"stop"}]}`,
	)
	if s.SawToolCalls() {
		t.Fatal("空数组不应算作见过 tool_calls")
	}
	if got := s.FinishReason(); got != "stop" {
		t.Fatalf("finish=%q want stop", got)
	}
	if !silentToolStop(2, s.SawToolCalls(), 0, s.FinishReason()) {
		t.Fatal("应判为静默收尾")
	}
	if silentToolStop(2, s.SawToolCalls(), 1, s.FinishReason()) {
		t.Fatal("正文标记已还原出调用时不应判为静默收尾")
	}
	if silentToolStop(0, s.SawToolCalls(), 0, s.FinishReason()) {
		t.Fatal("未声明 tools 时不应判为静默收尾")
	}
}

func TestStatsReaderSeesToolCalls(t *testing.T) {
	s := newChatStatsReaderSince(strings.NewReader(""), time.Now())
	feedStatsLines(s,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"read_file","arguments":"{}"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":""},"finish_reason":"tool_calls"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2}}`,
	)
	if !s.SawToolCalls() {
		t.Fatal("非空 tool_calls 应被记录")
	}
	if got := s.FinishReason(); got != "tool_calls" {
		t.Fatalf("finish=%q want tool_calls", got)
	}
	if n, ok := s.Tokens(); !ok || n != 2 {
		t.Fatalf("usage 解析回归：%d %v", n, ok)
	}
}

package upstream

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// TestInjectConversationFields_LastUserMessage 钉住 body 侧会话字段的放置规则：
// 只给「当前轮」的最后一条 user 消息写 conversationRequestId / startsNewUserRequest /
// agent，历史消息不编造（官方历史消息的轮级 ID 网关无从得知）。
func TestInjectConversationFields_LastUserMessage(t *testing.T) {
	in := `{"model":"glm-5.2","messages":[
		{"role":"system","content":"s"},
		{"role":"user","content":"第一轮"},
		{"role":"assistant","content":"答"},
		{"role":"user","content":"第二轮"}
	]}`
	out := injectConversationFields([]byte(in), "01a160ff00007fff8000000000000001")

	var obj struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := obj.Messages[3]["conversationRequestId"]; got != "01a160ff00007fff8000000000000001" {
		t.Fatalf("最后一条 user 必须带本轮 ID，got=%v", got)
	}
	if got := obj.Messages[3]["startsNewUserRequest"]; got != true {
		t.Fatalf("startsNewUserRequest 必须为 true，got=%v", got)
	}
	if got := obj.Messages[3]["agent"]; got != "cli" {
		t.Fatalf("agent 必须为 cli，got=%v", got)
	}
	// 历史 user 消息不补（不编造对不上的 ID）
	if _, ok := obj.Messages[1]["conversationRequestId"]; ok {
		t.Fatal("历史 user 消息不应被写入 conversationRequestId")
	}
	// system / assistant 不动
	if _, ok := obj.Messages[0]["agent"]; ok {
		t.Fatal("system 消息不应被写入 agent")
	}
}

// TestInjectConversationFields_PreservesClientValue 客户端自带轮级 ID 时原值优先。
func TestInjectConversationFields_PreservesClientValue(t *testing.T) {
	in := `{"messages":[{"role":"user","content":"x","conversationRequestId":"client-turn-id"}]}`
	out := injectConversationFields([]byte(in), "gateway-turn-id")
	if !strings.Contains(string(out), "client-turn-id") {
		t.Fatalf("客户端原值被覆盖: %s", out)
	}
}

// TestInjectConversationFields_NoConversationRequestID 空 ID 时原样返回（不写空值）。
func TestInjectConversationFields_NoConversationRequestID(t *testing.T) {
	in := `{"messages":[{"role":"user","content":"x"}]}`
	out := injectConversationFields([]byte(in), "")
	if string(out) != in {
		t.Fatalf("空 ID 不应改 body:\n in=%s\nout=%s", in, out)
	}
}

// TestInjectConversationFields_BadJSON 坏 body 原样返回（不二次错误化）。
func TestInjectConversationFields_BadJSON(t *testing.T) {
	in := `not json`
	if out := injectConversationFields([]byte(in), "id"); string(out) != in {
		t.Fatalf("坏 body 被改动: %s", out)
	}
}

// TestACPConnectionIDShape acp-connection-id 必须与会话 ID 同形但不同值：
// 官方在 applyGatewayHeaders 里把它与 X-Conversation-ID 并列设置，值来自会话注册表
// （每 session 一个，同会话稳定、跨会话不同）。
func TestACPConnectionIDShape(t *testing.T) {
	uid, conv := "85329a86-a3e7-448a-9666-ec34e0bdfb67", "conv-1"
	sess := buildCacheKey(uid, conv)
	acp := buildStableUUID(uid, conv, "acp")
	uuid4 := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !uuid4.MatchString(acp) {
		t.Fatalf("acp-connection-id 必须 UUID v4 形态: %q", acp)
	}
	if acp == sess {
		t.Fatal("acp-connection-id 不能与会话 ID 同值（官方是两个独立值）")
	}
	// 同 (账号, 会话) 稳定
	if again := buildStableUUID(uid, conv, "acp"); again != acp {
		t.Fatalf("acp-connection-id 不稳定: %q vs %q", acp, again)
	}
	// 跨会话不同
	if other := buildStableUUID(uid, "conv-2", "acp"); other == acp {
		t.Fatal("不同会话不该共用 acp-connection-id")
	}
	// purpose 为空时与历史 buildCacheKey 逐字节一致（缓存槽零漂移）
	if buildStableUUID(uid, conv, "") != sess {
		t.Fatal("buildStableUUID(purpose=\"\") 必须等于 buildCacheKey")
	}
}

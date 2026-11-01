// Package headers 构造三类上游请求头（common / chat / billing / refresh）。
// 规则来自 docs/api-reference.md §0/§4/§6。
package upstream

import (
	"net"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
)

const (
	// defaultClientVersionCN / defaultClientVersionGlobal 出站 WorkBuddy 客户端版本段
	// （UA 的 `<brand>/<ver>` 与 X-IDE-Version）。按 realm 分开：国内版分发包 5.9.3、
	// 国际版分发包 5.9.4，两版发版节奏不同。对话与生图共用同一取值。
	// config upstream.client_version 可覆盖（空 = 按 realm 取内置默认）。
	defaultClientVersionCN     = "5.9.3"
	defaultClientVersionGlobal = "5.9.4"
	// defaultCliVersion 出站 UA 中 `CLI/<ver>` 段版本，两版分发包内置同一 CLI（2.165.0）。
	// config upstream.cli_version 可覆盖（空 = 内置默认）。
	defaultCliVersion = "2.165.0"
)

// DefaultClientVersion / DefaultCliVersion 供登录链路等包外调用方取同一版本口径。
func DefaultClientVersion(realm string) string {
	if realmKey(realm) == "global" {
		return defaultClientVersionGlobal
	}
	return defaultClientVersionCN
}

// DesktopRPCUserAgent 桌面端 RPC 族 UA（node fetch 发出的 REST 请求）：
//
//	CN     WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0
//	global workbuddy-ai/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0
//
// chat 走 SDK 的另一族（首段恒驼峰 WorkBuddy），见 defaultWorkBuddyUAFor。
func DesktopRPCUserAgent(realm, clientVer, cliVer string) string {
	brand := "WorkBuddy"
	if realmKey(realm) == "global" {
		brand = "workbuddy-ai"
	}
	if clientVer == "" {
		clientVer = DefaultClientVersion(realm)
	}
	if cliVer == "" {
		cliVer = defaultCliVersion
	}
	return brand + "/" + clientVer + " " + brand + "/" + clientVer + " CLI/" + cliVer
}

// rpcUserAgent 非 chat 的 REST 出站 UA：显式 user_agent 优先，否则桌面端 RPC 族。
func (c *Client) rpcUserAgent(a *auth.Auth) string {
	if c != nil && c.UserAgent != "" {
		return c.UserAgent
	}
	realm := "cn"
	if a != nil && a.IsGlobal() {
		realm = "global"
	}
	return DesktopRPCUserAgent(realm, c.clientVersion(a), c.cliVersion())
}

// clientVersion 生效的 WorkBuddy 客户端版本：Client.ClientVersion 非空则取之，
// 否则按 realm 取内置默认（CN 5.9.3 / global 5.9.4）。
func (c *Client) clientVersion(a *auth.Auth) string {
	if c != nil && c.ClientVersion != "" {
		return c.ClientVersion
	}
	if a != nil && a.IsGlobal() {
		return defaultClientVersionGlobal
	}
	return defaultClientVersionCN
}

// cliVersion 生效的 CLI 版本：Client.CliVersion 非空则取之，否则内置默认 defaultCliVersion。
func (c *Client) cliVersion() string {
	if c != nil && c.CliVersion != "" {
		return c.CliVersion
	}
	return defaultCliVersion
}

// defaultWorkBuddyUAFor 组装默认客户端出站 UA（官方 chat 走 OpenAI SDK 的 buildUserAgent）：
// `WorkBuddy/<ver> <brand>/<ver> CLI/<cliVer>`，首段恒为驼峰 `WorkBuddy`，第二段按 realm：
//
//	CN 5.9.3     → WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0
//	global 5.9.4 → WorkBuddy/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0
//
// 国际版第二段是打包期由 productName `WorkBuddy AI` 改写成的 slug `workbuddy-ai`
// （双版本真机 chat 抓包）。官方无 UA 随机化，故默认确定性。
func (c *Client) defaultWorkBuddyUAFor(a *auth.Auth) string {
	platform := "WorkBuddy"
	if a != nil && a.IsGlobal() {
		platform = "workbuddy-ai"
	}
	ver := c.clientVersion(a)
	return "WorkBuddy/" + ver + " " + platform + "/" + ver + " CLI/" + c.cliVersion()
}

// defaultWorkBuddyUA 返回 CN 形态的默认 UA（默认账号形态即 CN，零回归兼容既有调用/测试）。
func (c *Client) defaultWorkBuddyUA() string {
	return c.defaultWorkBuddyUAFor(nil)
}

// userAgent 返回当前出站 UA（客户端出站路径：chat/refresh/FetchModels）。
// 优先级：Client.UserAgent（config user_agent）显式覆盖 > 按账号 realm 的默认 WorkBuddy 三段式。
// 显式覆盖兼容既有覆盖逻辑：用户配了即以用户值为准（自定义品牌/版本），
// 未配则走官方桌面端默认形态（global 换 `workbuddy-ai` 平台段）。
func (c *Client) userAgent(a *auth.Auth) string {
	if c != nil && c.UserAgent != "" {
		return c.UserAgent
	}
	return c.defaultWorkBuddyUAFor(a)
}

// billingUA 白名单类（billing/checkin/banner）出站 UA：单段 `WorkBuddy/<clientVersion>`
// （官方 banner 显式覆写形态，不带 CLI 段）。默认生效（伪造官方桌面端指纹）；
// 显式 client_name="SaaS" 才不设 UA（还原旧行为，Go 默认 UA）。
func (c *Client) billingUA(a *auth.Auth) string {
	if c == nil || c.attributionClientName() == "SaaS" {
		return ""
	}
	return "WorkBuddy/" + c.clientVersion(a)
}

// resolveDeviceToken 解析本次请求的 X-Device-Token 取值。
// 优先级：auth.Auth.DeviceToken（每号）> Client.DeviceToken（config 全局）> 文件兜底。
// 三者皆空/读失败则返回空串（调用方不注入该头，优雅降级）。
func (c *Client) resolveDeviceToken(a *auth.Auth) string {
	if a != nil && a.DeviceToken != "" {
		return a.DeviceToken
	}
	if c != nil && c.DeviceToken != "" {
		return c.DeviceToken
	}
	if c != nil && c.DeviceTokenFile != "" {
		return readDeviceTokenFile(c.DeviceTokenFile)
	}
	return ""
}

// injectDeviceToken 在 req 注入 X-Device-Token 头（仅当取到非空 token）。
func (c *Client) injectDeviceToken(req *http.Request, a *auth.Auth) {
	if tok := c.resolveDeviceToken(a); tok != "" {
		req.Header.Set("X-Device-Token", tok)
	}
}

// CommonHeaders 设置非 chat REST 出站的公共头（桌面端 RPC 族，node fetch 形态）。
//
// 真机（CN 5.9.3 / global 5.9.4）这一族没有浏览器语义：不发 Origin / Referer /
// X-Requested-With / Accept-Language，X-CodeBuddy-Request 只出现在 chat；设备标识
// 不走头（X-Machine-ID / X-Session-ID 客户端从不发）。chat 与生图在各自的头函数里补回
// 自己那一族的头。
func (c *Client) CommonHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", c.rpcUserAgent(a))
	req.Header.Set("X-Product", "SaaS")
	c.setAcceptEncoding(req)
}

// chatFamilyHeaders chat / 生图（SDK 族）在 RPC 公共头之上的差异：SDK 族 UA、
// 收窄的 Accept、X-Requested-With 与风控闸门头 X-CodeBuddy-Request。
func (c *Client) chatFamilyHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("User-Agent", c.userAgent(a))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-CodeBuddy-Request", "1")
}

// acceptLanguageFor 返回 Accept-Language。
//
// 官方客户端这个头不按账号域切，而是跟随操作系统区域；billing 层固定发 `zh`，
// 国际版账号同样如此。旧实现「global → en-US」与真机不符，统一取 `zh`。
func acceptLanguageFor(a *auth.Auth) string {
	return "zh"
}

// injectCodeBuddyRequest 在 req 注入 X-CodeBuddy-Request: 1（真机仅 chat / 生图带）。
func (c *Client) injectCodeBuddyRequest(req *http.Request) {
	req.Header.Set("X-CodeBuddy-Request", "1")
}

// ChatMeta 一次 chat 出站的会话头族元数据（issue #35：后台按 X-Conversation-Request-ID
// 聚合请求，官方客户端一次 user send 内所有 tool call/重试/换号复用同一个 ID）。
// handler 在轮转循环外生成 conversationID / conversationRequestID，循环内每次出站
// 复用同值；TraceID 透传入站值（空 = 回落 conversationRequestID）。
// messageID（消息级，每条独立）由 ChatHeaders 内部生成，无需外部可见。
type ChatMeta struct {
	ConversationID string // X-Conversation-ID：客户端声明的会话 ID，原值透传
	// CacheKeyID 客户端未声明会话时网关认定的会话键（粘性键 / 派生键）：用于缓存槽，
	// 出站头只放它的 (uid, 键) 派生 UUIDv7，原值不上头。
	CacheKeyID            string
	ConversationRequestID string // X-Conversation-Request-ID / X-Root-Request-ID：聚合主键，必发
	TraceID               string // X-Trace-ID：入站透传值，空则回落 conversationRequestID
}

// ChatHeaders 在 common 之上加 chat 专属的账号头。
// 缺省字段用 X-No-* 约定（与 CodeBuddy 官方 CLI 一致）。
// clientIP 为本次请求的客户端 IP（按参数传递，不读共享字段——避免并发串扰）；
// PassthroughIP=false 或 clientIP 为空时不注入 IP 头。
// meta 为会话头族元数据（纯新增，不改既有头），见 injectConversationHeaders。
func (c *Client) ChatHeaders(req *http.Request, a *auth.Auth, clientIP string, meta ChatMeta) {
	c.CommonHeaders(req, a)
	// SDK 族差异头（UA / Accept / XRW / X-CodeBuddy-Request）。Accept 与官方 chat
	// 一致只发 application/json，照样收 SSE 流。Accept-Encoding 由 CommonHeaders 按
	// WireCompression 声明，SSE 由 decodeTransport 流式解压。
	c.chatFamilyHeaders(req, a)
	// AccessToken 加锁快照：keepalive 定时刷新会在 a.mu 内改写它，锁外直读构成数据竞争
	// （见 auth.AccessTokenValue 注释）。
	if at := a.AccessTokenValue(); at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	} else {
		req.Header.Set("X-No-Authorization", "true")
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	} else {
		req.Header.Set("X-No-User-Id", "true")
	}
	// 安全红线：绝不在 chat 请求里携带 X-Refresh-Token。
	// 企业/域头：**只发有值的**。桌面端个人号实测两个方向都不发 X-No-* 哨兵头
	// （capture/raw3 国际版、capture/raw8 国内版，逐头对照均无 X-No-Enterprise-Id /
	// X-No-Department-Info）。旧实现用 X-No-* 声明「无企业」，那是 CodeBuddy CLI 的
	// 约定，不是 WorkBuddy 桌面端的形态——多发一个客户端从不发的头同样是指纹。
	if a != nil && !a.IsGlobal() {
		if a.EnterpriseID != "" {
			req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		}
		// X-Domain 必发且为对话域裸主机名（真机恒 www.workbuddy.cn），不取 auth 落盘的
		// 旧登录域。
		req.Header.Set("X-Domain", c.chatHost(a))
	} else {
		c.injectGlobalChatHeaders(req, a)
	}
	// 用量归属头：默认伪造 WorkBuddy 桌面端指纹（client_name="SaaS" 还原旧行为）。
	c.injectAttribution(req, a)
	// 客户端 IP 透传（仅 PassthroughIP=true 且本次请求带 IP）。
	c.injectClientIP(req, clientIP)
	// 设备风控头：auth 每号 > config 全局 > 文件兜底；空则不注入。
	c.injectDeviceToken(req, a)
	// 会话头族（对话/请求/消息/B3 链路），见 injectConversationHeaders。
	c.injectConversationHeaders(req, a.UID, meta)
	// SDK / agent 段指纹头（x-stainless-* / x-agent-type / x-agent-intent）。
	c.injectClientSDKHeaders(req)
	c.injectAgentContextHeaders(req, a)
}

// injectAgentContextHeaders chat 出站的运行形态头（5.9.3 / 5.9.4 真机）：
//   - X-Agent-Runtime: local-agent（常量）；
//   - X-Private-Data：客户端逻辑为「模型优化开 → false，关 → true」。国内发行版默认开
//     （企业号默认关），国际发行版默认关——判定维度是发行版，不是账号形态；
//   - baggage：OTel 传播串，恒 4 键，两个 codebuddy.* 与同请求会话头同值
//     （头间不一致本身就是可判别形态）。
func (c *Client) injectAgentContextHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("X-Agent-Runtime", "local-agent")
	req.Header.Set("X-Private-Data", privateDataFor(a))
	req.Header.Set("baggage", "buddyId=,buddyName=,codebuddy.session_id="+req.Header.Get("X-Conversation-ID")+
		",codebuddy.conversation_request_id="+req.Header.Get("X-Conversation-Request-ID"))
}

// privateDataFor X-Private-Data 取值：global → true；CN 企业号 → true；CN 个人号 → false。
func privateDataFor(a *auth.Auth) string {
	if a != nil && (a.IsGlobal() || a.EnterpriseID != "") {
		return "true"
	}
	return "false"
}

// injectGlobalChatHeaders global 账号的 chat 专属域声明头。
//
// 只发 X-Domain：桌面端个人号实测并不发 X-No-Enterprise-Id（旧实现按 CodeBuddy CLI
// 约定补的哨兵头），国际版 5.8.4 与国内版 5.8.3 的真机报文里都不存在该头
// （capture/raw3/001-chat.json、capture/raw8/002-chat.json）。
func (c *Client) injectGlobalChatHeaders(req *http.Request, a *auth.Auth) {
	if a == nil || !a.IsGlobal() {
		return
	}
	req.Header.Set("X-Domain", "www.workbuddy.ai")
}

// injectConversationHeaders 注入官方客户端会话头族（issue #35 后台聚合）。
// 头族分四层，各司其职：
//   - X-Conversation-ID：会话级，多轮稳定（body 的 conversationId）。空则不发——
//     透传客户端原值优先，客户端没给就不伪造，避免误导后台建错会话。
//   - X-Conversation-Request-ID：**对话轮级聚合主键**，必发。一次 user send 内的
//     所有 tool call/重试/换号/降级复用同一个 → 后台按它聚合成一条（不再碎片化）。
//   - X-Conversation-Message-ID = X-Request-ID：消息级，每条独立（32 位 hex）。
//   - X-Root-Request-ID：= conversationRequestID（根请求追踪）。
//   - X-Trace-ID：入站透传，否则独立新值（**不**回落 conversationRequestID——官方实测
//     两族恒不相等，见下方实现注释）。
//   - X-B3-TraceId / X-B3-SpanId / X-B3-Sampled：链路族，TraceId 跟随 X-Trace-ID。
//     B3 规范只认 16/32 hex TraceId 与 16 hex SpanId；入站透传值非法时 TraceId 回落
//     messageID（恒 32 hex），SpanId 取 messageID[:16]（每消息新）。
func (c *Client) injectConversationHeaders(req *http.Request, uid string, meta ChatMeta) {
	convReqID := meta.ConversationRequestID
	if convReqID == "" {
		// 零值 meta（直接调 ChatHeaders 的调用方/测试）也要保证聚合主键必发：
		// 本级补一个 32 hex，调用方（handler）已生成稳定的不走到这里。
		convReqID = session.NewMessageID()
	}
	messageID := session.NewMessageID()
	// 会话头三元组必须齐全：国际端点收紧通道校验后，缺 X-Conversation-ID 的
	// 请求会直接 400 / 11128（conversation id missing）。入站没带会话 ID 时
	// 本级补一个 32 hex（与 X-Conversation-Request-ID 的零值兜底同口径），
	// 不再因为「透传优先」而漏发该头（issue #190）。
	// 形态对齐真机 UUIDv7（01a1815d-dacf-75f1-…）：客户端声明的原值透传；否则按
	// (uid, 会话键) 稳定派生 v7；都没有现造 v7。
	switch {
	case meta.ConversationID != "":
		req.Header.Set("X-Conversation-ID", meta.ConversationID)
	case meta.CacheKeyID != "":
		req.Header.Set("X-Conversation-ID", buildTimeOrderedUUID(uid, meta.CacheKeyID, ""))
	default:
		req.Header.Set("X-Conversation-ID", session.NewTimeOrderedID())
	}
	req.Header.Set("X-Conversation-Request-ID", convReqID)
	req.Header.Set("X-Conversation-Message-ID", messageID)
	req.Header.Set("X-Request-ID", messageID)
	req.Header.Set("X-Root-Request-ID", convReqID)
	// X-Trace-ID 与 request id **互不回落**：国际版 5.8.4 真机实测，X-Trace-ID 恒为
	// 一条独立的 32 hex（样本 0ece3294f88c4ec5a02e78547617f874），而
	// X-Conversation-Request-ID 是另一条时间有序 ID（01a160b2…），二者从无相等。
	// 旧实现空则回落 convReqID，导致两族头恒等——是「网关自造」的可见痕迹（issue #51）。
	// 入站透传值仍优先（客户端自己给了就尊重）。
	traceID := meta.TraceID
	if traceID == "" {
		traceID = session.NewTraceID()
	}
	req.Header.Set("X-Trace-ID", traceID)
	// B3 跟着 trace 走（官方 X-B3-TraceId == X-Trace-ID），而不是 request id。
	b3Trace := traceID
	if !validTraceID(b3Trace) {
		b3Trace = messageID // 入站透传值非法（含横线/超长/非 hex）→ 回落恒 32 hex
	}
	req.Header.Set("X-B3-TraceId", b3Trace)
	// SpanId 必须独立随机：官方 span（9795d0f16a68bd32）与 trace/request 均无关系。
	// 曾用 messageID[:16]，那会把这个头的 12 hex 时间前缀一起带出来。
	spanID := session.NewSpanID()
	req.Header.Set("X-B3-SpanId", spanID)
	// ParentSpanId：官方每条 chat 都带（两版实测），值是上一级 span 的 16 hex。
	// 网关没有真实 span 树，取独立随机值——上游无法校验父子关系，但**缺这个头**
	// 与「有 span 无 parent」在形态上是可区分的。
	parentSpanID := session.NewSpanID()
	req.Header.Set("X-B3-ParentSpanId", parentSpanID)
	req.Header.Set("X-B3-Sampled", "1")
	// B3 单头与 W3C traceparent：官方两版都双发（b3 = trace-span-flags-parent，
	// traceparent = 00-trace-span-flags）。缺这两个头时只有 X-B3-* 三件套，
	// 而客户端从不只发三件套。
	req.Header.Set("b3", b3Trace+"-"+spanID+"-1-"+parentSpanID)
	req.Header.Set("traceparent", "00-"+b3Trace+"-"+spanID+"-01")
	// acp-connection-id：官方把它列进**标准出站头族**（asar applyGatewayHeaders 与
	// X-Conversation-ID / X-Conversation-Request-ID 同一批设置），值来自会话注册表的
	// acpConnectionId —— 每个 session 生成一次、同会话多轮复用、跨会话不同
	// （`acquire(): metadata={acpConnectionId: uuid(), conversationRequestId: uuid()}`）。
	// 网关没有 ACP 连接层，按「同 (账号, 会话) 稳定、与会话 ID 不同值」派生，
	// 语义与官方一致；无会话源时现造一个（该路径无跨请求稳定语义）。
	if src := cacheKeySource(meta); src != "" {
		req.Header.Set("acp-connection-id", buildTimeOrderedUUID(uid, src, "acp"))
	} else {
		req.Header.Set("acp-connection-id", session.NewTimeOrderedID())
	}
}

// 官方 chat 出站的自报 SDK 指纹（国际版 5.8.4 与国内版 5.8.3 实测**完全一致**，
// 见 capture/raw3/001-chat.json 与 capture/raw8/002-chat.json）。
//
// 这些是 openai-node SDK 自动加的 x-stainless-* 头 + 业务语义头 x-agent-type/intent。
// 网关是 Go 实现，没有对应的 SDK 运行时；这里按官方实测值固定，理由是「客户端形态
// 一致性」优先——不发这几个头，上游看到的是一份缺了 SDK 段的残缺头集合。
// 一旦客户端版本变化（package-version / node 版本），这几个常量需要跟着更新。
const (
	clientArch     = "x64"
	clientOS       = "Windows"
	clientLang     = "js"
	clientRuntime  = "node"
	clientNodeVer  = "v22.21.1"
	clientSDKVer   = "6.25.0"
	agentTypeMain  = "main"
	agentIntentCrf = "craft"
)

// injectClientSDKHeaders 注入 x-stainless-* 与 x-agent-* 两族出站头。
// 仅在 chat 路径调用（ChatHeaders）——billing / checkin 等 REST 域没有真机对照证据，
// 不臆造。
func (c *Client) injectClientSDKHeaders(req *http.Request) {
	req.Header.Set("X-Agent-Type", agentTypeMain)
	req.Header.Set("X-Agent-Intent", agentIntentCrf)
	req.Header.Set("X-Stainless-Lang", clientLang)
	req.Header.Set("X-Stainless-Package-Version", clientSDKVer)
	req.Header.Set("X-Stainless-OS", clientOS)
	req.Header.Set("X-Stainless-Arch", clientArch)
	req.Header.Set("X-Stainless-Runtime", clientRuntime)
	req.Header.Set("X-Stainless-Runtime-Version", clientNodeVer)
	req.Header.Set("X-Stainless-Retry-Count", "0")
}

// validTraceID 判断 B3 TraceId 是否合法：16 或 32 位 hex（大小写均可）。
// 官方客户端生成的 conversationRequestId 是 32 位 hex（UUID 去横线），入站透传值
// 可能是任意形状（含横线/超长/非 hex），直接塞进 B3 头会破坏链路关联（issue #35）。
func validTraceID(s string) bool {
	if len(s) != 16 && len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')) {
			return false
		}
	}
	return true
}

// attributionClientName 生效的用量归属名：ClientName 非空取之；
// 空默认 "WorkBuddy"（伪造官方桌面端指纹；显式配 "SaaS" 可还原旧行为）。
func (c *Client) attributionClientName() string {
	if c != nil && c.ClientName != "" {
		return c.ClientName
	}
	return "WorkBuddy"
}

// injectAttribution 注入用量归属头（X-Agent-Purpose / X-IDE-* / X-Product）。
// 仅在 chat/completions 路径生效（ChatHeaders 调用）。
//
// X-Product 恒为 "SaaS"，与 X-IDE-* 分开取值——这是真机抓包纠正过的形态（issue #51）：
//
//	国际版 5.8.4 出站：X-Product: SaaS  X-IDE-Name: WorkBuddy  X-IDE-Type: WorkBuddy  X-IDE-Version: 5.8.4
//	（capture/raw3/001-chat.json 官方全量头；CN 侧 desktop.go 文件头的逆向结论同为 SaaS）
//
// 旧实现让 X-Product 也取客户端名（=WorkBuddy），于是同一个头官方发 SaaS、网关发 WorkBuddy
// ——「同名不同值」比多一个头更容易被规则命中。X-IDE-* 才是承载产品名的地方。
func (c *Client) injectAttribution(req *http.Request, a *auth.Auth) {
	name := c.attributionClientName()
	// X-Product 是 SKU 段，恒 SaaS：官方两版实测都不是产品名。
	req.Header.Set("X-Product", "SaaS")
	if name == "SaaS" {
		// 显式 ClientName=SaaS：仅声明 SKU，不补 X-IDE-*（旧行为，保留兼容）。
		return
	}
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-IDE-Name", name)
	req.Header.Set("X-IDE-Type", name)
	req.Header.Set("X-IDE-Version", c.clientVersion(a))
}

// injectClientIP 在 PassthroughIP 开启时把 clientIP 参数透传给上游（三等价头）。
func (c *Client) injectClientIP(req *http.Request, clientIP string) {
	if c == nil || !c.PassthroughIP || clientIP == "" {
		return
	}
	req.Header.Set("X-Forwarded-For", clientIP)
	req.Header.Set("X-Real-IP", clientIP)
	req.Header.Set("X-Client-IP", clientIP)
}

// ExtractClientIP 从入站请求提取客户端 IP 首段（X-Forwarded-For 首段，回落 X-Real-IP）。
func ExtractClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' {
				return strings.TrimSpace(xff[:i])
			}
		}
		return strings.TrimSpace(xff)
	}
	if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
		return real
	}
	return ""
}

// chatHost 当前 chat base 的裸主机名（X-Domain 用，不带 scheme / 端口 / 路径）。
func (c *Client) chatHost(a *auth.Auth) string {
	return hostOf(c.chatBase(a), "www.workbuddy.cn")
}

// billingHost 当前 billing base 的裸主机名（X-Domain 兜底用）。
func (c *Client) billingHost(a *auth.Auth) string {
	return hostOf(c.billingBase(a), "www.workbuddy.cn")
}

// hostOf 从 base URL 剥出裸主机名；空则回落 fallback。
func hostOf(base, fallback string) string {
	if i := strings.Index(base, "://"); i >= 0 {
		base = base[i+3:]
	}
	if i := strings.IndexAny(base, "/?#"); i >= 0 {
		base = base[:i]
	}
	if h, _, err := net.SplitHostPort(base); err == nil {
		base = h
	}
	if base == "" {
		return fallback
	}
	return base
}

// BillingHeaders billing 接口请求头（签到 / 余额 / growth）。
// 真机 billing 族带纯随机 32 hex X-Request-ID，不带 X-CodeBuddy-Request /
// Accept-Language；X-Domain 恒发裸主机名。UA：显式 user_agent 优先，否则单段
// WorkBuddy/<ver>（client_name="SaaS" 时不设）。
func (c *Client) BillingHeaders(req *http.Request, a *auth.Auth) {
	// AccessToken 加锁快照（同 ChatHeaders：keepalive 可在 a.mu 内改写）。
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", session.NewTraceID())
	c.setAcceptEncoding(req)
	if c != nil && c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	} else if ua := c.billingUA(a); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	req.Header.Set("X-Domain", domainOrHost(a, req))
	// 设备风控头：billing 域（travel/balance/checkin）同样注入。
	c.injectDeviceToken(req, a)
}

// RefreshHeaders refresh 端点专属头（X-Refresh-Token 只允许出现在这里）。
// 真机（5.9.3 / 5.9.4）refresh 为 RPC 族：不带 X-Requested-With；Authorization 与
// X-Refresh-Token 双发，另带 X-Request-ID / X-User-Id / X-Domain，body 为 `{}`。
func (c *Client) RefreshHeaders(req *http.Request, a *auth.Auth) {
	c.CommonHeaders(req, a)
	req.Header.Set("X-Domain", domainOrHost(a, req))
	req.Header.Set("X-Refresh-Token", a.RefreshToken)
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	}
	// X-Auth-Refresh-Source 对齐官方客户端 refresh 渠道标识 "plugin"（D3）。
	req.Header.Set("X-Auth-Refresh-Source", "plugin")
	req.Header.Set("X-Request-ID", session.NewTraceID())
	if at := a.AccessTokenValue(); at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
}

// domainOrHost X-Domain 取值：auth 落盘 domain（剥 scheme）优先，否则请求目标主机名。
func domainOrHost(a *auth.Auth, req *http.Request) string {
	if a != nil {
		if d := hostOf(strings.TrimSpace(a.DomainValue()), ""); d != "" {
			return d
		}
	}
	if req != nil && req.URL != nil {
		return req.URL.Hostname()
	}
	return "www.workbuddy.cn"
}

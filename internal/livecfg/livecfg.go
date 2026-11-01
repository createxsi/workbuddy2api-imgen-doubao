// Package livecfg 运行期可变配置的并发安全持有者。
//
// 背景：进程启动时读入的配置是普通字段（读多写零），但管理面板允许在线改配置，
// 于是少量"可热生效"的字段需要有并发安全的读写点。此处用不可变快照 + atomic 指针：
// 读方 Load 拿到一致视图，写方 Store 整体替换，无锁无数据竞争。
//
// 只承载**读路径深、热改需求强**的少数字段；池参数/排程参数等各有既有 setter
// （pool.SetBreaker、scheduler.Reconfigure 等），不重复收编到这里。
package livecfg

import (
	"strings"
	"sync/atomic"
	"time"
)

// Snapshot 一次读取的不可变配置视图。
type Snapshot struct {
	APIKey               string              // 网关/面板共同鉴权密钥；空 = 不鉴权
	SoftCooldown         time.Duration       // 429 软冷却基数（<=0 时调用方回退内置默认）
	SanitizeFingerprints bool                // 出站请求体指纹脱敏
	RecordClientInfo     bool                // 请求日志是否记录调用来源（客户端 IP / UA）
	HiddenModels         map[string]struct{} // 精确隐藏的模型 id（已归一化为 "cn:x"/"global:x" 全量形式）；nil = 不隐藏
	ModelAliases         map[string]string   // 客户端名 → 真实模型名（键 "cn:x"/"global:x" 小写）；nil = 不映射
	ModelFallback        map[string]string   // 模型级限流改道表（键 "cn:x"/"global:x" 小写，值为同域裸名）；nil = 不改道
}

// NormalizeModelFallback 把 models.fallback 条目（"原模型=等价模型"）归一化为查找表。
//   - 键带 cn:/global: 前缀只在该域生效，裸名键两域都登记；
//   - 值可带前缀，但必须与键同域（不跨域改道），落表时存裸名；
//   - 两端须同一模型族（ModelFamily 相同），防止误把请求改道到别家模型；
//   - 无 "="、空键/空值、自我映射、跨域、跨族条目丢弃；重复键以后写为准。
func NormalizeModelFallback(in []string) map[string]string {
	out := make(map[string]string, len(in)*2)
	for _, item := range in {
		k, v, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		if k == "" || v == "" {
			continue
		}
		realms := []string{"cn", "global"}
		if prefix, bare, cut := strings.Cut(k, ":"); cut && (prefix == "cn" || prefix == "global") {
			realms, k = []string{prefix}, bare
		}
		vRealm := ""
		if prefix, bare, cut := strings.Cut(v, ":"); cut && (prefix == "cn" || prefix == "global") {
			vRealm, v = prefix, bare
		}
		if k == "" || v == "" || strings.EqualFold(k, v) || ModelFamily(k) == "" || ModelFamily(k) != ModelFamily(v) {
			continue
		}
		for _, r := range realms {
			if vRealm != "" && vRealm != r {
				continue
			}
			out[r+":"+k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ModelFamily 模型族：小写模型名开头的连续字母段（deepseek-v4.1-flash → deepseek，
// glm-5.2 → glm，hy3 → hy）。取不到字母段时返回空串。
func ModelFamily(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	end := 0
	for end < len(model) && model[end] >= 'a' && model[end] <= 'z' {
		end++
	}
	return model[:end]
}

// NormalizeModelAliases 把 aliases 条目（"客户端名=真实模型名"）归一化为查找表。
//   - 按第一个 "=" 拆分，键值 Trim，键小写；
//   - 键带 cn:/global: 前缀只在该域生效，裸名键两域都登记（与 NormalizeHiddenModels 同口径）；
//   - 值原样保留，可带前缀显式指定域；
//   - 无 "="、空键/空值、自我映射丢弃；重复键以后写为准。
func NormalizeModelAliases(in []string) map[string]string {
	out := make(map[string]string, len(in)*2)
	for _, item := range in {
		k, v, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		if k == "" || v == "" || strings.EqualFold(k, v) {
			continue
		}
		if prefix, bare, cut := strings.Cut(k, ":"); cut && (prefix == "cn" || prefix == "global") {
			if bare != "" {
				out[k] = v
			}
			continue
		}
		out["cn:"+k] = v
		out["global:"+k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Holder 原子持有当前快照。
type Holder struct {
	p atomic.Pointer[Snapshot]
}

// New 以初始快照构建。
func New(s Snapshot) *Holder {
	h := &Holder{}
	h.Store(s)
	return h
}

// Load 返回当前快照（Holder 为 nil 或从未 Store 时返回零值快照，调用方无需判空）。
func (h *Holder) Load() Snapshot {
	if h == nil {
		return Snapshot{}
	}
	if s := h.p.Load(); s != nil {
		return *s
	}
	return Snapshot{}
}

// Store 整体替换快照。
func (h *Holder) Store(s Snapshot) { h.p.Store(&s) }

// NormalizeHiddenModels 把用户配置的 hidden 条目归一化为精确 id 集合。
//
// 条目带合法 "cn:"/"global:" 前缀 → 只隐藏该域；裸名 → 两域都隐藏。
// 大小写敏感（与 internal/server.resolveModel 的前缀规则对称：取第一个 ":",
// 前段恰为 "cn"/"global" 才剥离）。返回的 map 只读共享，调用方不得修改。
func NormalizeHiddenModels(hidden []string) map[string]struct{} {
	if len(hidden) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(hidden)*2)
	for _, m := range hidden {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if prefix, bare, ok := strings.Cut(m, ":"); ok && (prefix == "cn" || prefix == "global") {
			out[prefix+":"+bare] = struct{}{}
		} else {
			out["cn:"+m] = struct{}{}
			out["global:"+m] = struct{}{}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

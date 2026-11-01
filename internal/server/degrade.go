package server

import (
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/prompt"
)

// 11128 system 指纹误报的抢救阶梯。
const (
	degradeSplit   = 1 // 最长 system 按句界拆成多条 system
	degradeMove    = 2 // system 原文搬进首条 user，system 换中性提示词
	degradeRewrite = 3 // 中性提示词整体替换
)

// rescueSystemPrompt 从 after 的下一层起找第一层可用的改写，返回新 body 与所用层级。
// 拆不动（单句 system）、搬不动（无纯文本 user）时自动落到下一层，替换层恒可用。
func rescueSystemPrompt(body []byte, after int) ([]byte, int) {
	if after < degradeSplit {
		if out, ok := prompt.SplitSystemMessages(body); ok {
			return out, degradeSplit
		}
	}
	if after < degradeMove {
		if out, ok := prompt.MoveSystemToUser(body, prompt.Degraded); ok {
			return out, degradeMove
		}
	}
	return prompt.Rewrite(body, prompt.Degraded), degradeRewrite
}

// degradeGate 降级状态机：passthrough 模式下请求被上游内容策略拦截时，
// 切换到 Degraded 中性提示词直到次日 00:00 CST 重置。
//
// 定位：误报处理（与 internal/upstream/sanitize.go 同哲学）——内容拦截多为
// system 来源的指纹误报，换最小中性提示词即可绕开；不是对抗框架。
//
// 状态机：进程内存、重启清零（可接受：重启极少触发，且 custom 模式根本
// 不进降级路径）。降级期内的请求直达 Degraded，不再先撞 400。
type degradeGate struct {
	mu    sync.Mutex
	until time.Time
}

// Active 当前是否处于降级期（now < until）。
func (g *degradeGate) Active() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return time.Now().Before(g.until)
}

// Trigger 触发降级，直到次日 00:00 CST（Asia/Shanghai）。
// 已在降级期内则不续期（保持最早触发点的 00:00 重置语义）。
func (g *degradeGate) Trigger() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !time.Now().Before(g.until) {
		// 已过期/未触发 → 设新的 until；未过期则保持原 until（不续期）。
		g.until = nextMidnightCST(time.Now())
	}
}

// nextMidnightCST 返回 now 之后最近的 Asia/Shanghai 00:00 时刻（纯函数，方便单测）。
//
// 边界语义：
//   - 23:59 → 次日 00:00（几秒后）
//   - 00:00 → 次日 00:00（刚过 00:00，下个零点是次日）
//
// 用固定 +08:00 偏移计算，避免依赖系统时区配置（容器/宿主机时区不确定）。
func nextMidnightCST(now time.Time) time.Time {
	cst := time.FixedZone("CST", 8*60*60)
	// 把 now 转到 CST 视角取当天 00:00，再加一天；若该 00:00 <= now 则再加一天。
	y, m, d := now.In(cst).Date()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, cst)
	for !midnight.After(now) {
		midnight = midnight.Add(24 * time.Hour)
	}
	return midnight
}

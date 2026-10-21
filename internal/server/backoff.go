// backoff.go 轮转退避与抖动的单一事实来源（WAF 403 修复 P0-2/P0-1 共用）：
// 指数基数/封顶/抖动比例一处定义，chatCompletions 轮转退避与 WAF 软冷却基数
// （handler.wafCooldownBase）共用同一 jitterDur，不在 handler 与 upstream 各写一份。
package server

import (
	"context"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// rotateBackoffBase 轮转退避基数（对齐官方 intl CLI computeRequestRetryDelayMs
// 的 500ms 形态，WAF 403 并发研究报告 §2.1/§6 P0-2）。测试可置 0 跳过等待
// （TestMain 已置 0 加速轮转测试；退避界断言测试临时恢复）。
var rotateBackoffBase = 500 * time.Millisecond

const (
	// rotateBackoffCap 轮转退避封顶（报告 P0-2 建议 8s：轮转上限默认 3 次，
	// 实际等待序列 500ms/1s，封顶只约束极端配置下的 MaxRotate）。
	rotateBackoffCap = 8 * time.Second
	// jitterFraction 抖动比例（±25%，对齐 intl CLI delay×(1±0.25) 形态）。
	jitterFraction = 0.25
)

// jitterDur 给时长施加 ±jitterFraction 的均匀抖动，返回 [d·(1-f), d·(1+f)] 区间值。
// d<=0 原样返回（零等待不抖动）。抖动目的是打散多请求同相位重试（WAF 频控按
// 密度判罚，齐步走的退避会以固定周期再次聚团）。
func jitterDur(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	f := 1 + (rand.Float64()*2-1)*jitterFraction
	out := time.Duration(float64(d) * f)
	if out < 0 {
		return 0
	}
	return out
}

// backoffAfter 返回第 n 次轮转（0 基：首次失败换号前 n=0）前应等待的退避时长：
// base·2^n 封顶 rotateBackoffCap，再施加 ±25% 抖动。base 置 0（测试）时恒 0。
// 用逐次翻倍而非位移：base 调整后无需同步维护移位上限，溢出由封顶比较兜底。
func backoffAfter(n int) time.Duration {
	d := rotateBackoffBase
	if d <= 0 {
		return 0
	}
	for k := 0; k < n && d < rotateBackoffCap; k++ {
		d *= 2
		if d <= 0 { // 翻倍溢出成非正数：直接按封顶处理
			return jitterDur(rotateBackoffCap)
		}
	}
	if d > rotateBackoffCap {
		d = rotateBackoffCap
	}
	return jitterDur(d)
}

// busyWaitCap 「模型繁忙」（14003）全池被挡时，单个请求愿意等待的上限。
// 超过它的冷却说明上游连续繁忙，等待只会让客户端干耗，直接 503 + Retry-After。
// 测试置 0 关闭该路径（与 rotateBackoffBase 同口径）。
var busyWaitCap = 10 * time.Second

// busyWaitFor 报告本请求该为「模型繁忙」等多久再试（0 = 不等，直接按 503 报）。
// 只有纯 14003 阻塞、且剩余冷却在上限内才值得等：11102/11158 换号重试必然同样
// 失败，冷却已被连续命中拉长到上限外说明上游真的持续繁忙。
func busyWaitFor(blk pool.ModelBlockStatus, now time.Time, cap time.Duration) time.Duration {
	if !blk.Blocked || blk.Until.IsZero() || !isModelBusyReason(blk.Reason) {
		return 0
	}
	d := blk.Until.Sub(now)
	if d <= 0 || d > cap {
		return 0
	}
	return d
}

// isModelBusyReason 判「模型繁忙」：冷却原因可能是上游原文（含 `"code":14003`），
// 也可能是 applyErrorPolicy 写入的固定标记 "14003 model busy"——两者都要认。
func isModelBusyReason(reason string) bool {
	return upstream.IsModelBusy(reason) || strings.Contains(reason, "14003")
}

// waitPast 等待到解封时刻：只在 d 之上加**正向**抖动（0 ~ +25%），绝不提前醒来。
//
// 不能用 jitterDur：那是双向 ±25%，抽到小于 1 的系数时会在冷却还没解除时就醒，
// 于是「只等一次」的额度被白花掉、客户端多等 8 秒后照样拿 503（实测同一场景
// 8 次里 4 次白等，issue #46）。往上多等一点没有代价，早醒一次就是纯损失。
func waitPast(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d + time.Duration(float64(d)*rand.Float64()*jitterFraction)
}

// sleepCtx 可取消的等待：ctx 取消立即返回 false（客户端断连/优雅停机不必等退避
// 睡醒），等满返回 true。d<=0 立即放行。与 scheduler.sleepCtx 同模式（该函数未
// 导出且 scheduler 不宜被 server 反向依赖，按任务书「等价物」口径在消费侧实现）。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

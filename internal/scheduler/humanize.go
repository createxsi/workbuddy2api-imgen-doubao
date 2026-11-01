// humanize.go 分钟级周期任务的人味化：余额刷新不再按精确固定间隔整池齐射。
//
// 小时级排程已有账号级摊开（spread.go）；余额刷新是分钟级 ticker，此前每轮间隔
// 恒定、全账号同一秒并发——同一出口上最好认的时间形状。这里补两类随机性：
//   - 周期抖动：每轮间隔在 ±frac×interval 内偏移；
//   - 账号错峰：本轮各账号在 [0, frac×interval) 内各自延后起跑。
//
// 随机源是「进程种子 + key」的 FNV 散列：同一种子下可复现（测试可固定），重启换相位，
// 多实例共享同一配置也不会抖到一起。key 带轮次序号，因此逐轮、逐号都不同。
package scheduler

import (
	"encoding/binary"
	"hash/fnv"
	"strconv"
	"sync/atomic"
	"time"
)

// MaxJitterPermill 周期抖动上限（千分比，500 = ±50%）。
const MaxJitterPermill = 500

// jitterSeed 抖动随机源的进程种子（默认取启动时刻）。
var jitterSeed atomic.Int64

func init() { jitterSeed.Store(time.Now().UnixNano()) }

// setJitterSeed 固定抖动种子（测试专用；0 = 恢复为当前时刻）。
func setJitterSeed(n int64) {
	if n == 0 {
		n = time.Now().UnixNano()
	}
	jitterSeed.Store(n)
}

// jitterFrac 稳定的 [0,1) 伪随机分数：同种子同 key → 同结果，不同 key 均匀散开。
func jitterFrac(key string) float64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	var b [9]byte
	binary.LittleEndian.PutUint64(b[1:], uint64(jitterSeed.Load()))
	_, _ = h.Write(b[:])
	return float64(h.Sum64()>>11) / float64(1<<53)
}

// periodicJitter 本轮间隔的偏移量：±frac×interval 内。frac<=0 返回 0（旧行为）。
func periodicJitter(interval time.Duration, frac float64, key string) time.Duration {
	if frac <= 0 || interval <= 0 {
		return 0
	}
	return time.Duration((jitterFrac(key) - 0.5) * 2 * float64(interval) * frac)
}

// staggerWindow 余额刷新账号错峰窗口 = frac×interval；<=0 表示全并发（旧行为）。
func staggerWindow(interval time.Duration, frac float64) time.Duration {
	if frac <= 0 || interval <= 0 {
		return 0
	}
	return time.Duration(float64(interval) * frac)
}

// staggerDelay 单账号在本轮错峰窗口内的偏移，[0, window)。round 参与散列，
// 避免「每轮第 i 个号」固定落在同一相对位置。
func staggerDelay(window time.Duration, round uint64, uid string) time.Duration {
	if window <= 0 {
		return 0
	}
	return time.Duration(jitterFrac("bal|"+strconv.FormatUint(round, 10)+"|"+uid) * float64(window))
}

// SetBalanceJitter 热改余额刷新抖动（千分比，钳到 0–500；0 = 关闭）。下一轮生效。
func (s *Scheduler) SetBalanceJitter(permill int) {
	s.balanceJitter.Store(int64(clampPermill(permill)))
}

// BalanceJitterPermill 当前余额刷新抖动（千分比）。
func (s *Scheduler) BalanceJitterPermill() int { return int(s.balanceJitter.Load()) }

func clampPermill(v int) int {
	if v < 0 {
		return 0
	}
	if v > MaxJitterPermill {
		return MaxJitterPermill
	}
	return v
}

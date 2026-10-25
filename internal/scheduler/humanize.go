// humanize.go 分钟级周期任务的人味化：余额刷新不再按精确固定间隔整池齐射，
// 夜猫子可收窄到积分最高的少数账号。
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
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
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

// capByCredits 按积分降序取前 n 个（同分保持原顺序）。n<=0 或不超上限时原样返回；
// 不改动入参切片的顺序。
func capByCredits(list []pool.Status, n int) []pool.Status {
	if n <= 0 || len(list) <= n {
		return list
	}
	sorted := append([]pool.Status(nil), list...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Credits > sorted[j].Credits })
	return sorted[:n]
}

// SetBalanceJitter 热改余额刷新抖动（千分比，钳到 0–500；0 = 关闭）。下一轮生效。
func (s *Scheduler) SetBalanceJitter(permill int) {
	s.balanceJitter.Store(int64(clampPermill(permill)))
}

// BalanceJitterPermill 当前余额刷新抖动（千分比）。
func (s *Scheduler) BalanceJitterPermill() int { return int(s.balanceJitter.Load()) }

// SetNightAccountCap 热改夜猫子参与上限；<=0 = 不限。
func (s *Scheduler) SetNightAccountCap(n int) {
	if n < 0 {
		n = 0
	}
	s.nightAccountCap.Store(int64(n))
}

// NightAccountCap 当前夜猫子参与上限（0 = 不限）。
func (s *Scheduler) NightAccountCap() int { return int(s.nightAccountCap.Load()) }

func clampPermill(v int) int {
	if v < 0 {
		return 0
	}
	if v > MaxJitterPermill {
		return MaxJitterPermill
	}
	return v
}

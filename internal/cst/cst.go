// Package cst 固定 UTC+8（北京时间）：上游「自然日」的唯一定义。
//
// 为什么需要一个包：WorkBuddy 的额度、签到、每日活跃、夜猫子窗口、积分包到期日
// 全部按北京时间零点切日，而网关可能跑在 Docker / NAS / 海外 VPS 上——那些机器的
// 本地时区不是 UTC+8。判定跟着宿主时区走，「今天」的边界就整体漂移 8 小时：
// UTC 主机上配的 activity_hours=[10] 实际在 CST 18:00 跑，硬冷却的「次日 04:00」
// 落在 CST 12:00（签到早已结束），夜猫子 23:00–08:00 的窗口也跟着错位。
// 这些错都不会报错，只会静默少拿积分——所以日界只能由一个常量决定。
//
// 为什么是固定偏移而不是时区库：中国标准时间没有夏令时，1970 年后恒为 +08:00，
// 固定偏移既够用又不依赖宿主 tzdata。（agent2api 的 core::beijing 同一口径。）
//
// 什么时候**不**该用：面向操作者的统计口径（用量报表按小时/按天分桶、请求日志
// 按天切文件）保留宿主本地时区——那看的是「我的昨天」，不是上游的自然日。
package cst

import "time"

// Loc 固定 UTC+8 时区。
var Loc = time.FixedZone("CST", 8*60*60)

// Now 当前时刻（带 CST 时区信息；Go 的 time.Time 本身存绝对时刻，转区不改语义）。
func Now() time.Time { return time.Now().In(Loc) }

// Day 返回 t 所属的上游自然日（CST），格式 2006-01-02，字典序即时间序。
func Day(t time.Time) string { return t.In(Loc).Format("2006-01-02") }

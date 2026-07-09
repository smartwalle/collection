package sortedset

import "cmp"

// Member 表示 Set 支持的成员类型。
//
// Sorted Set 会先按 score 排序；当多个成员的 score 相同时，会继续按 member
// 的自然顺序排序，用来保证遍历和 rank 结果稳定。因此 member 需要满足
// 标准库 cmp.Ordered 约束。
//
// Redis 的 sorted set 通常使用字符串成员。这个实现允许整数、浮点数和字符串等
// cmp.Ordered 类型作为成员，但不支持结构体这类只能比较相等、不能比较大小的类型。
//
// 注意：如果 member 是浮点类型，NaN 不受支持。Go 中 NaN != NaN，作为 map key
// 会破坏 member 唯一性和查询/删除语义。为了不影响热路径性能，Set 不会在运行时检查
// NaN，调用方需要保证传入的 member 不是 NaN。
type Member = cmp.Ordered

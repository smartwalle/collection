package treemap

import "cmp"

// Key 表示 Map 支持的 key 类型。
//
// Map 是有序结构，key 必须满足标准库 cmp.Ordered 约束。
// 这意味着 key 可以使用 <、<=、>、>= 进行自然排序。
//
// 当前实现没有接收自定义 less 函数，是为了让 API 保持简单：
// 用户看到 Map[K,V] 时，可以明确知道它一定使用 K 的自然顺序。
type Key = cmp.Ordered

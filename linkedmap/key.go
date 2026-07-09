package linkedmap

// Key 表示 Map 支持的 key 类型。
//
// Map 底层使用 Go 内置 map 按 key 定位元素，因此 key 必须满足
// 标准库预声明的 comparable 约束。也就是说，key 可以是布尔、数字、字符串、
// 指针、channel、元素可比较的数组，以及所有字段都可比较的结构体等类型。
//
// Map 不会根据 key 做大小比较，元素顺序只由插入和移动操作决定。
// 如果需要按 key 的自然顺序遍历，应该使用 treemap。
type Key = comparable

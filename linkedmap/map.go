package linkedmap

// element 是 Map 中保存的一组 key/value 以及链表指针。
//
// Map 需要同时满足两个目标：
//   - 通过 table 按 key 快速定位元素；
//   - 通过 prev/next 按插入顺序遍历元素。
//
// 因此 element 同时存在于哈希表和双向链表中。这里没有复用 container/list，
// 是为了避免 interface{} 装箱和额外的 list.Element 包装。
type element[K Key, V any] struct {
	key   K
	value V
	prev  *element[K, V]
	next  *element[K, V]
}

// Map 是按插入顺序维护元素的泛型 Map。
//
// 和 Go 内置 map 不同，Map 会记录 key 第一次插入的顺序，因此 Range、Keys、
// Values 会稳定地按插入顺序返回结果。对已经存在的 key 再次 Put 或 Set 只会
// 更新 value，不会改变该 key 原来的位置。
//
// Map 的定位更接近 linked hash map：
//   - table 提供按 key 查询、写入和删除的平均 O(1) 能力；
//   - 双向链表提供按插入顺序的正向和反向遍历能力；
//   - Front、Back 可以直接读取最早和最新插入的元素。
//
// K 必须满足 Key 约束，也就是 Go 内置 map 支持的 comparable 类型。
//
// Map 不是并发安全的。如果多个 goroutine 同时读写同一个 Map，
// 调用方需要自行加锁。
type Map[K Key, V any] struct {
	// table 保存 key 到链表元素的映射。空 Map 的 table 为 nil，首次写入时会初始化。
	table map[K]*element[K, V]
	// head 指向最早插入且尚未删除的元素。
	head *element[K, V]
	// tail 指向最新插入且尚未删除的元素。
	tail *element[K, V]
}

// New 创建一个空 Map。
func New[K Key, V any]() *Map[K, V] {
	return NewWithCapacity[K, V](0)
}

// NewWithCapacity 创建一个带初始容量的空 Map。
//
// capacity 会用于初始化底层 table。调用方如果大致知道元素数量，
// 可以通过这个函数减少 table 扩容次数。
//
// capacity 小于 0 时会按 0 处理。
func NewWithCapacity[K Key, V any](capacity int) *Map[K, V] {
	if capacity < 0 {
		capacity = 0
	}
	return &Map[K, V]{table: make(map[K]*element[K, V], capacity)}
}

// Len 返回当前元素数量。
//
// Len 对 nil Map 返回 0，方便调用方在可选 Map 场景中直接使用。
func (m *Map[K, V]) Len() int {
	if m == nil {
		return 0
	}
	return len(m.table)
}

// Get 根据 key 获取 value。
//
// 如果 key 存在，返回对应 value 和 true。
// 如果 key 不存在，返回 value 的零值和 false。
func (m *Map[K, V]) Get(key K) (V, bool) {
	var zero V
	if m == nil || m.table == nil {
		return zero, false
	}
	item, ok := m.table[key]
	if !ok {
		return zero, false
	}
	return item.value, true
}

// Has 判断 key 是否存在。
//
// Has 只是对 Get 的轻量包装，用于调用方只关心 key 是否存在的场景。
// 它不会区分 value 是否为零值。
func (m *Map[K, V]) Has(key K) bool {
	if m == nil || m.table == nil {
		return false
	}
	_, ok := m.table[key]
	return ok
}

// Put 写入 key 和 value。
//
// 如果 key 不存在，Put 会把新元素追加到链表尾部，并返回 value 的零值和 false。
// 如果 key 已存在，Put 只会覆盖旧 value，并返回旧 value 和 true。
//
// 覆盖已有 key 时不会调整链表位置，这样可以保证“插入顺序”只由第一次插入决定。
func (m *Map[K, V]) Put(key K, value V) (V, bool) {
	var zero V
	if m == nil {
		return zero, false
	}
	m.ensureTable()
	if item, ok := m.table[key]; ok {
		old := item.value
		item.value = value
		return old, true
	}

	item := &element[K, V]{key: key, value: value}
	m.table[key] = item
	m.pushBack(item)
	return zero, false
}

// Set 写入 key 和 value，并忽略旧值。
//
// Set 适合调用方只关心最终写入结果、不关心 key 原来是否存在的场景。
// 如果需要知道本次写入是新增还是覆盖，或者需要拿到旧 value，应使用 Put。
func (m *Map[K, V]) Set(key K, value V) {
	m.Put(key, value)
}

// Delete 删除指定 key。
//
// 如果 key 存在，Delete 会从 table 和链表中同时删除该元素，
// 并返回被删除的 value 和 true。
// 如果 key 不存在，Delete 返回 value 的零值和 false。
func (m *Map[K, V]) Delete(key K) (V, bool) {
	var zero V
	if m == nil || m.table == nil {
		return zero, false
	}
	item, ok := m.table[key]
	if !ok {
		return zero, false
	}
	delete(m.table, key)
	m.unlink(item)
	return item.value, true
}

// Clear 清空所有元素。
//
// Clear 会直接丢弃 table 和链表头尾引用，让旧元素交给 GC 回收。
// 这里不会保留元素池，避免让普通使用场景承担额外复杂度。
func (m *Map[K, V]) Clear() {
	if m == nil {
		return
	}
	m.table = nil
	m.head = nil
	m.tail = nil
}

// Front 返回最早插入且尚未删除的元素。
//
// 如果 Map 为空，返回 key/value 的零值和 false。
func (m *Map[K, V]) Front() (K, V, bool) {
	var key K
	var value V
	if m == nil || m.head == nil {
		return key, value, false
	}
	return m.head.key, m.head.value, true
}

// Back 返回最新插入且尚未删除的元素。
//
// 如果 Map 为空，返回 key/value 的零值和 false。
func (m *Map[K, V]) Back() (K, V, bool) {
	var key K
	var value V
	if m == nil || m.tail == nil {
		return key, value, false
	}
	return m.tail.key, m.tail.value, true
}

// MoveToFront 把 key 对应的元素移动到链表头部。
//
// 移动后，该元素会成为 Range、Keys、Values 返回的第一个元素。
// 如果 key 不存在、Map 为空或 Map 为 nil，返回 false。
// 如果 key 已经在头部，返回 true，但不会调整任何指针。
//
// MoveToFront 只改变元素顺序，不改变 key/value，也不会重新分配元素。
// 由于 table 可以直接定位元素，链表摘除和插入也都是常数成本，
// 所以该操作的平均时间复杂度为 O(1)。
func (m *Map[K, V]) MoveToFront(key K) bool {
	if m == nil || m.table == nil {
		return false
	}
	item, ok := m.table[key]
	if !ok {
		return false
	}
	if item == m.head {
		return true
	}
	m.unlink(item)
	m.pushFront(item)
	return true
}

// MoveToBack 把 key 对应的元素移动到链表尾部。
//
// 移动后，该元素会成为 ReverseRange 访问的第一个元素，
// 也是 Back 返回的元素。
// 如果 key 不存在、Map 为空或 Map 为 nil，返回 false。
// 如果 key 已经在尾部，返回 true，但不会调整任何指针。
//
// MoveToBack 只改变元素顺序，不改变 key/value，也不会重新分配元素。
// 该操作通过 table 定位元素，再在双向链表中摘除并追加，平均时间复杂度为 O(1)。
func (m *Map[K, V]) MoveToBack(key K) bool {
	if m == nil || m.table == nil {
		return false
	}
	item, ok := m.table[key]
	if !ok {
		return false
	}
	if item == m.tail {
		return true
	}
	m.unlink(item)
	m.pushBack(item)
	return true
}

// MoveBefore 把 key 对应的元素移动到 markKey 对应元素的前面。
//
// 如果 key 或 markKey 任意一个不存在，返回 false，Map 不会发生变化。
// 如果 key 和 markKey 相同，返回 true，并保持原顺序不变。
//
// MoveBefore 适合需要精确调整插入顺序的场景，例如把某个配置项移动到
// 另一个配置项之前。该方法不会改变 key/value，只调整链表指针。
// 两个元素都通过 table 定位，因此平均时间复杂度为 O(1)。
func (m *Map[K, V]) MoveBefore(key K, markKey K) bool {
	if m == nil || m.table == nil {
		return false
	}
	item, ok := m.table[key]
	if !ok {
		return false
	}
	mark, ok := m.table[markKey]
	if !ok {
		return false
	}
	if item == mark {
		return true
	}
	m.unlink(item)
	m.insertBefore(item, mark)
	return true
}

// MoveAfter 把 key 对应的元素移动到 markKey 对应元素的后面。
//
// 如果 key 或 markKey 任意一个不存在，返回 false，Map 不会发生变化。
// 如果 key 和 markKey 相同，返回 true，并保持原顺序不变。
//
// MoveAfter 和 MoveBefore 一样，只调整链表顺序，不影响 table 中的映射。
// 因为移动前后元素对象没有变化，所以已保存的 value 不会被复制。
// 该操作的平均时间复杂度为 O(1)。
func (m *Map[K, V]) MoveAfter(key K, markKey K) bool {
	if m == nil || m.table == nil {
		return false
	}
	item, ok := m.table[key]
	if !ok {
		return false
	}
	mark, ok := m.table[markKey]
	if !ok {
		return false
	}
	if item == mark {
		return true
	}
	m.unlink(item)
	m.insertAfter(item, mark)
	return true
}

// Range 按插入顺序遍历所有元素。
//
// fn 返回 false 时会立即停止遍历。
// 如果 Map 为空、Map 为 nil 或 fn 为 nil，Range 不做任何操作。
//
// 遍历时会先保存 next 指针，再调用 fn。这样即使 fn 删除当前元素，
// 遍历仍然可以继续走向原本的下一个元素。
func (m *Map[K, V]) Range(fn func(K, V) bool) {
	if m == nil || fn == nil {
		return
	}
	for item := m.head; item != nil; {
		next := item.next
		if !fn(item.key, item.value) {
			return
		}
		item = next
	}
}

// ReverseRange 按插入顺序的反方向遍历所有元素。
//
// fn 返回 false 时会立即停止遍历。
// 如果 Map 为空、Map 为 nil 或 fn 为 nil，ReverseRange 不做任何操作。
func (m *Map[K, V]) ReverseRange(fn func(K, V) bool) {
	if m == nil || fn == nil {
		return
	}
	for item := m.tail; item != nil; {
		prev := item.prev
		if !fn(item.key, item.value) {
			return
		}
		item = prev
	}
}

// Keys 按插入顺序返回所有 key。
//
// 返回的切片是新分配的，调用方可以安全修改切片本身。
// 修改切片不会影响 Map 内部结构。
func (m *Map[K, V]) Keys() []K {
	keys := make([]K, 0, m.Len())
	m.Range(func(key K, _ V) bool {
		keys = append(keys, key)
		return true
	})
	return keys
}

// Values 按插入顺序返回所有 value。
//
// 返回的 value 顺序与 Keys 返回的 key 顺序一致。
// 返回的切片是新分配的，调用方可以安全修改切片本身。
func (m *Map[K, V]) Values() []V {
	values := make([]V, 0, m.Len())
	m.Range(func(_ K, value V) bool {
		values = append(values, value)
		return true
	})
	return values
}

// ensureTable 确保零值 Map 在首次写入时可以使用。
func (m *Map[K, V]) ensureTable() {
	if m.table == nil {
		m.table = make(map[K]*element[K, V])
	}
}

// pushFront 把 item 插入到链表头部。
//
// 调用方需要保证 item 还没有挂到任何链表上。
// 对空链表来说，item 同时是 head 和 tail。
func (m *Map[K, V]) pushFront(item *element[K, V]) {
	if m.head == nil {
		m.head = item
		m.tail = item
		return
	}
	item.next = m.head
	m.head.prev = item
	m.head = item
}

// pushBack 把 item 追加到链表尾部。
//
// 调用方需要保证 item 还没有挂到任何链表上。
func (m *Map[K, V]) pushBack(item *element[K, V]) {
	if m.tail == nil {
		m.head = item
		m.tail = item
		return
	}
	item.prev = m.tail
	m.tail.next = item
	m.tail = item
}

// insertBefore 把 item 插入到 mark 前面。
//
// 调用方需要保证：
//   - item 没有挂到任何链表上；
//   - mark 当前仍在同一个链表中；
//   - item 和 mark 不是同一个元素。
//
// 如果 mark 是 head，插入后 item 会成为新的 head；否则只需要修正
// mark 前一个元素、item、mark 三者之间的 prev/next 关系。
func (m *Map[K, V]) insertBefore(item *element[K, V], mark *element[K, V]) {
	prev := mark.prev
	item.prev = prev
	item.next = mark
	mark.prev = item
	if prev == nil {
		m.head = item
		return
	}
	prev.next = item
}

// insertAfter 把 item 插入到 mark 后面。
//
// 调用方需要保证：
//   - item 没有挂到任何链表上；
//   - mark 当前仍在同一个链表中；
//   - item 和 mark 不是同一个元素。
//
// 如果 mark 是 tail，插入后 item 会成为新的 tail；否则只需要修正
// mark、item、mark 后一个元素三者之间的 prev/next 关系。
func (m *Map[K, V]) insertAfter(item *element[K, V], mark *element[K, V]) {
	next := mark.next
	item.prev = mark
	item.next = next
	mark.next = item
	if next == nil {
		m.tail = item
		return
	}
	next.prev = item
}

// unlink 从链表中摘除 item。
//
// table 的删除由调用方负责，这里只维护链表前后指针。
func (m *Map[K, V]) unlink(item *element[K, V]) {
	if item.prev == nil {
		m.head = item.next
	} else {
		item.prev.next = item.next
	}
	if item.next == nil {
		m.tail = item.prev
	} else {
		item.next.prev = item.prev
	}
	item.prev = nil
	item.next = nil
}

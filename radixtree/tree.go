package radixtree

import (
	"sort"
	"strings"
)

// node 是 Radix Tree 中的一条压缩边和它落到的节点。
//
// label 保存从父节点走到当前节点需要消费的字符串片段。
// 根节点的 label 永远为空；其他节点的 label 永远非空。
//
// Radix Tree 和普通 trie 的差异就在这里：普通 trie 通常一个字节一层，
// 而这里会把只有单一路径的连续字节压成一个 label。这样能明显减少节点数量，
// 尤其适合 URL、路由、配置项、字典词这类共享前缀较多的字符串集合。
type node[V any] struct {
	label    string
	value    V
	hasValue bool
	children []*node[V]
}

// Tree 是以 string 为 key 的压缩前缀树。
//
// Tree 适合需要按字符串前缀组织数据的场景：
//   - Get、Put、Delete 提供基础 Map 操作；
//   - LongestPrefix 查找输入字符串命中的最长已保存前缀；
//   - RangePrefix 和 DeletePrefix 处理一整段前缀下的数据；
//   - Range、ReverseRange、Keys、Values 提供按 key 字典序的遍历能力。
//
// 这里的字典序和 Go 对 string 使用 <、> 比较时的顺序一致，都是按字节比较。
// 对合法 UTF-8 文本来说，这个顺序稳定可复现，但它不是按自然语言排序。
//
// Tree 不是并发安全的。如果多个 goroutine 同时读写同一个 Tree，
// 调用方需要自行加锁。
type Tree[V any] struct {
	// root 是 Radix Tree 的哨兵根节点。空 Tree 的 root 为 nil。
	root *node[V]
	// length 是当前保存的 key/value 数量。
	length int
}

// New 创建一个空 Tree。
func New[V any]() *Tree[V] {
	return &Tree[V]{root: &node[V]{}}
}

// Len 返回当前元素数量。
//
// Len 对 nil Tree 返回 0，方便调用方在可选 Tree 场景中直接使用。
func (t *Tree[V]) Len() int {
	if t == nil {
		return 0
	}
	return t.length
}

// Get 根据 key 获取 value。
//
// 如果 key 存在，返回对应 value 和 true。
// 如果 key 不存在，返回 value 的零值和 false。
func (t *Tree[V]) Get(key string) (V, bool) {
	var zero V
	if t == nil || t.root == nil {
		return zero, false
	}
	return t.root.get(key)
}

// Has 判断 key 是否存在。
//
// Has 只是对 Get 的轻量包装，用于调用方只关心 key 是否存在的场景。
// 它不会区分 value 是否为零值。
func (t *Tree[V]) Has(key string) bool {
	_, ok := t.Get(key)
	return ok
}

// Put 写入 key 和 value。
//
// 如果 key 不存在，Put 会插入新节点，并返回 value 的零值和 false。
// 如果 key 已存在，Put 会覆盖旧 value，并返回旧 value 和 true。
//
// 插入时只在遇到“部分公共前缀”时分裂节点：
//   - 已有边为 "foobar"，新 key 为 "foo" 时，会分裂出 "foo" -> "bar"；
//   - 已有边为 "foobar"，新 key 为 "fooz" 时，会分裂出 "foo" -> "bar"/"z"；
//   - 已有边完全匹配时继续向下走，不制造额外节点。
//
// 这种分裂策略让树保持压缩形态，同时不会影响精确查询和按前缀扫描。
func (t *Tree[V]) Put(key string, value V) (V, bool) {
	var zero V
	if t == nil {
		return zero, false
	}
	t.ensureRoot()
	old, replaced := t.root.put(key, value)
	if !replaced {
		t.length++
	}
	return old, replaced
}

// Set 写入 key 和 value，并忽略旧值。
//
// Set 适合调用方只关心最终写入结果、不关心 key 原来是否存在的场景。
// 如果需要知道本次写入是新增还是覆盖，或者需要拿到旧 value，应使用 Put。
func (t *Tree[V]) Set(key string, value V) {
	t.Put(key, value)
}

// Delete 删除指定 key。
//
// 如果 key 存在，Delete 会删除该元素，并返回被删除的 value 和 true。
// 如果 key 不存在，Delete 返回 value 的零值和 false。
//
// 删除后如果某个中间节点不再保存 value，并且只剩一个子节点，Tree 会把两条边重新
// 合并成一条边。这个压缩动作能避免多次删除以后树逐渐退化成普通 trie 的形态。
func (t *Tree[V]) Delete(key string) (V, bool) {
	var zero V
	if t == nil || t.root == nil {
		return zero, false
	}
	old, deleted := t.root.delete(key)
	if !deleted {
		return zero, false
	}
	t.length--
	if t.length == 0 {
		t.root = nil
	}
	return old, true
}

// DeletePrefix 删除所有以 prefix 开头的 key，并返回删除数量。
//
// prefix 为空字符串时会清空整棵树。
// 如果不存在匹配 prefix 的 key，DeletePrefix 返回 0。
func (t *Tree[V]) DeletePrefix(prefix string) int {
	if t == nil || t.root == nil {
		return 0
	}
	if prefix == "" {
		deleted := t.length
		t.Clear()
		return deleted
	}
	deleted := t.root.deletePrefix(prefix)
	if deleted == 0 {
		return 0
	}
	t.length -= deleted
	if t.length == 0 {
		t.root = nil
	}
	return deleted
}

// Clear 清空所有元素。
//
// Clear 会直接丢弃根节点，让旧节点交给 GC 回收。
// 这里不会保留节点池，避免让普通使用场景承担额外复杂度。
func (t *Tree[V]) Clear() {
	if t == nil {
		return
	}
	t.root = nil
	t.length = 0
}

// Min 返回 key 最小的元素。
//
// 如果 Tree 为空，返回 key/value 的零值和 false。
func (t *Tree[V]) Min() (string, V, bool) {
	var value V
	if t == nil || t.root == nil {
		return "", value, false
	}
	return t.root.min("")
}

// Max 返回 key 最大的元素。
//
// 如果 Tree 为空，返回 key/value 的零值和 false。
func (t *Tree[V]) Max() (string, V, bool) {
	var value V
	if t == nil || t.root == nil {
		return "", value, false
	}
	return t.root.max("")
}

// LongestPrefix 返回 key 命中的最长已保存前缀。
//
// 例如 Tree 中保存了 "/"、"/api"、"/api/users"，调用
// LongestPrefix("/api/users/42") 会返回 "/api/users" 对应的 value。
//
// 如果不存在任何已保存 key 能作为输入 key 的前缀，返回零值和 false。
func (t *Tree[V]) LongestPrefix(key string) (string, V, bool) {
	var prefix string
	var value V
	if t == nil || t.root == nil {
		return prefix, value, false
	}

	current := t.root
	search := key
	matched := 0
	ok := false
	if current.hasValue {
		value = current.value
		ok = true
	}

	for search != "" {
		index, found := current.findChild(search[0])
		if !found {
			break
		}
		child := current.children[index]
		if !strings.HasPrefix(search, child.label) {
			break
		}
		matched += len(child.label)
		search = search[len(child.label):]
		current = child
		if current.hasValue {
			prefix = key[:matched]
			value = current.value
			ok = true
		}
	}

	if !ok {
		return "", value, false
	}
	return prefix, value, true
}

// Range 按 key 从小到大遍历所有元素。
//
// fn 返回 false 时会立即停止遍历。
// 如果 Tree 为空、Tree 为 nil 或 fn 为 nil，Range 不做任何操作。
//
// 遍历顺序由压缩边的首字节顺序决定。每个节点的 children 都按首字节升序保存，
// 因此深度优先遍历得到的顺序与 Go string 的字典序一致。
func (t *Tree[V]) Range(fn func(key string, value V) bool) {
	if t == nil || t.root == nil || fn == nil {
		return
	}
	t.root.rangeAsc("", fn)
}

// ReverseRange 按 key 从大到小遍历所有元素。
//
// fn 返回 false 时会立即停止遍历。
// 如果 Tree 为空、Tree 为 nil 或 fn 为 nil，ReverseRange 不做任何操作。
func (t *Tree[V]) ReverseRange(fn func(key string, value V) bool) {
	if t == nil || t.root == nil || fn == nil {
		return
	}
	t.root.rangeDesc("", fn)
}

// RangePrefix 按 key 从小到大遍历所有以 prefix 开头的元素。
//
// fn 返回 false 时会立即停止遍历。
// 如果没有 key 匹配 prefix，或者 Tree 为空、Tree 为 nil、fn 为 nil，
// RangePrefix 不做任何操作。
//
// prefix 不需要正好落在节点边界上。比如树里只有 "foobar"，调用
// RangePrefix("foo", fn) 也会访问 "foobar"。
func (t *Tree[V]) RangePrefix(prefix string, fn func(key string, value V) bool) {
	if t == nil || t.root == nil || fn == nil {
		return
	}
	item, path, ok := t.root.locatePrefix(prefix)
	if !ok {
		return
	}
	item.rangeAsc(path, fn)
}

// Keys 按 key 从小到大返回所有 key。
//
// 返回的切片是新分配的，调用方可以安全修改切片本身。
// 修改切片不会影响 Tree 内部结构。
func (t *Tree[V]) Keys() []string {
	keys := make([]string, 0, t.Len())
	t.Range(func(key string, _ V) bool {
		keys = append(keys, key)
		return true
	})
	return keys
}

// Values 按 key 从小到大返回所有 value。
//
// 返回的 value 顺序与 Keys 返回的 key 顺序一致。
// 返回的切片是新分配的，调用方可以安全修改切片本身。
func (t *Tree[V]) Values() []V {
	values := make([]V, 0, t.Len())
	t.Range(func(_ string, value V) bool {
		values = append(values, value)
		return true
	})
	return values
}

// KeysWithPrefix 按 key 从小到大返回所有以 prefix 开头的 key。
//
// 返回的切片是新分配的，调用方可以安全修改切片本身。
func (t *Tree[V]) KeysWithPrefix(prefix string) []string {
	keys := make([]string, 0)
	t.RangePrefix(prefix, func(key string, _ V) bool {
		keys = append(keys, key)
		return true
	})
	return keys
}

// ValuesWithPrefix 按 key 从小到大返回所有以 prefix 开头的 value。
//
// 返回的 value 顺序与 KeysWithPrefix 返回的 key 顺序一致。
func (t *Tree[V]) ValuesWithPrefix(prefix string) []V {
	values := make([]V, 0)
	t.RangePrefix(prefix, func(_ string, value V) bool {
		values = append(values, value)
		return true
	})
	return values
}

// ensureRoot 确保零值 Tree 在首次写入时可以使用。
func (t *Tree[V]) ensureRoot() {
	if t.root == nil {
		t.root = &node[V]{}
	}
}

// get 从当前节点开始查找 key。
func (n *node[V]) get(key string) (V, bool) {
	var zero V
	if key == "" {
		if !n.hasValue {
			return zero, false
		}
		return n.value, true
	}
	index, found := n.findChild(key[0])
	if !found {
		return zero, false
	}
	child := n.children[index]
	if !strings.HasPrefix(key, child.label) {
		return zero, false
	}
	return child.get(key[len(child.label):])
}

// put 从当前节点开始插入 key/value。
//
// key 是从当前节点往下还需要消费的剩余字符串。
func (n *node[V]) put(key string, value V) (V, bool) {
	var zero V
	if key == "" {
		if n.hasValue {
			old := n.value
			n.value = value
			return old, true
		}
		n.value = value
		n.hasValue = true
		return zero, false
	}

	index, found := n.findChild(key[0])
	if !found {
		n.insertChild(&node[V]{label: key, value: value, hasValue: true})
		return zero, false
	}

	child := n.children[index]
	common := commonPrefixLen(key, child.label)
	if common == len(child.label) {
		return child.put(key[common:], value)
	}

	branch := &node[V]{label: child.label[:common]}
	child.label = child.label[common:]
	branch.children = append(branch.children, child)

	if common == len(key) {
		branch.value = value
		branch.hasValue = true
	} else {
		branch.insertChild(&node[V]{label: key[common:], value: value, hasValue: true})
	}

	n.children[index] = branch
	return zero, false
}

// delete 从当前节点开始删除 key。
func (n *node[V]) delete(key string) (V, bool) {
	var zero V
	if key == "" {
		if !n.hasValue {
			return zero, false
		}
		old := n.value
		n.clearValue()
		return old, true
	}

	index, found := n.findChild(key[0])
	if !found {
		return zero, false
	}
	child := n.children[index]
	if !strings.HasPrefix(key, child.label) {
		return zero, false
	}

	old, deleted := child.delete(key[len(child.label):])
	if !deleted {
		return zero, false
	}
	n.fixChild(index)
	return old, true
}

// deletePrefix 删除当前节点下所有以 prefix 开头的 key。
func (n *node[V]) deletePrefix(prefix string) int {
	if prefix == "" {
		deleted := n.countValues()
		n.clearValue()
		n.children = nil
		return deleted
	}

	index, found := n.findChild(prefix[0])
	if !found {
		return 0
	}
	child := n.children[index]
	common := commonPrefixLen(prefix, child.label)
	if common == len(prefix) {
		deleted := child.countValues()
		n.removeChild(index)
		return deleted
	}
	if common == len(child.label) {
		deleted := child.deletePrefix(prefix[common:])
		if deleted > 0 {
			n.fixChild(index)
		}
		return deleted
	}
	return 0
}

// locatePrefix 定位 prefix 对应的子树。
//
// 返回的 path 是 item 对应节点的完整 key 前缀。prefix 可能落在某条压缩边中间，
// 此时 path 会比 prefix 更长，因为真正可遍历的子树根只能是完整节点。
func (n *node[V]) locatePrefix(prefix string) (*node[V], string, bool) {
	current := n
	path := ""
	for prefix != "" {
		index, found := current.findChild(prefix[0])
		if !found {
			return nil, "", false
		}
		child := current.children[index]
		common := commonPrefixLen(prefix, child.label)
		switch {
		case common == len(prefix):
			return child, path + child.label, true
		case common == len(child.label):
			path += child.label
			prefix = prefix[common:]
			current = child
		default:
			return nil, "", false
		}
	}
	return current, path, true
}

// min 返回当前子树中 key 最小的元素。
func (n *node[V]) min(prefix string) (string, V, bool) {
	if n.hasValue {
		return prefix, n.value, true
	}
	for _, child := range n.children {
		key, value, ok := child.min(prefix + child.label)
		if ok {
			return key, value, true
		}
	}
	var zero V
	return "", zero, false
}

// max 返回当前子树中 key 最大的元素。
func (n *node[V]) max(prefix string) (string, V, bool) {
	for i := len(n.children) - 1; i >= 0; i-- {
		child := n.children[i]
		key, value, ok := child.max(prefix + child.label)
		if ok {
			return key, value, true
		}
	}
	if n.hasValue {
		return prefix, n.value, true
	}
	var zero V
	return "", zero, false
}

// rangeAsc 按 key 升序遍历当前子树。
func (n *node[V]) rangeAsc(prefix string, fn func(key string, value V) bool) bool {
	if n.hasValue && !fn(prefix, n.value) {
		return false
	}
	for _, child := range n.children {
		if !child.rangeAsc(prefix+child.label, fn) {
			return false
		}
	}
	return true
}

// rangeDesc 按 key 降序遍历当前子树。
func (n *node[V]) rangeDesc(prefix string, fn func(key string, value V) bool) bool {
	for i := len(n.children) - 1; i >= 0; i-- {
		child := n.children[i]
		if !child.rangeDesc(prefix+child.label, fn) {
			return false
		}
	}
	if n.hasValue && !fn(prefix, n.value) {
		return false
	}
	return true
}

// countValues 统计当前子树中保存的 value 数量。
func (n *node[V]) countValues() int {
	var count int
	if n.hasValue {
		count++
	}
	for _, child := range n.children {
		count += child.countValues()
	}
	return count
}

// findChild 按 label 的首字节查找子节点。
//
// Radix Tree 的同一层不会存在两个首字节相同的子节点；如果出现相同首字节，
// 插入时就应该继续比较公共前缀并分裂节点，而不是并排保存两条边。
func (n *node[V]) findChild(first byte) (int, bool) {
	index := sort.Search(len(n.children), func(i int) bool {
		return n.children[i].label[0] >= first
	})
	if index < len(n.children) && n.children[index].label[0] == first {
		return index, true
	}
	return index, false
}

// insertChild 把 child 插入到 children 中，并保持首字节升序。
func (n *node[V]) insertChild(child *node[V]) {
	index, _ := n.findChild(child.label[0])
	n.children = append(n.children, nil)
	copy(n.children[index+1:], n.children[index:])
	n.children[index] = child
}

// removeChild 删除 children[index]。
func (n *node[V]) removeChild(index int) {
	copy(n.children[index:], n.children[index+1:])
	n.children[len(n.children)-1] = nil
	n.children = n.children[:len(n.children)-1]
}

// fixChild 在子节点删除 value 后恢复压缩形态。
func (n *node[V]) fixChild(index int) {
	child := n.children[index]
	if child.hasValue {
		return
	}
	switch len(child.children) {
	case 0:
		n.removeChild(index)
	case 1:
		next := child.children[0]
		child.label += next.label
		child.value = next.value
		child.hasValue = next.hasValue
		child.children = next.children
	}
}

// clearValue 清理节点上的 value。
//
// value 可能持有指针，清零可以让删除后的对象更早被 GC 回收。
func (n *node[V]) clearValue() {
	var zero V
	n.value = zero
	n.hasValue = false
}

// commonPrefixLen 返回 a 和 b 的公共前缀字节长度。
func commonPrefixLen(a string, b string) int {
	limit := len(a)
	if len(b) < limit {
		limit = len(b)
	}
	for i := 0; i < limit; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return limit
}

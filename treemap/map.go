package treemap

import (
	"cmp"
	"sort"
)

// kDefaultDegree 是默认的 B-Tree 最小阶数。
//
// 对于 degree = d 的节点：
//   - 非根节点最少保存 d-1 个元素；
//   - 每个节点最多保存 2*d-1 个元素；
//   - 子节点数量最多是元素数量 + 1。
//
// 这里默认使用 32，是为了让单个节点能保存较多元素，减少树高。
// 对内存中的有序 Map 来说，较低的树高通常能减少指针跳转次数，
// 同时节点内部用连续切片保存元素，二分查找和顺序遍历都有较好的缓存局部性。
const kDefaultDegree = 32

// element 是 Map 中真正保存的一组 key/value。
//
// B-Tree 节点内部会按 key 从小到大保存 elements。
// 查询、插入、删除都会先在当前节点的 elements 中二分定位，
// 再决定是否命中当前节点，或者继续向某个子节点查找。
//
// 这里不直接复用外部传入的 key/value 结构，是为了让节点内部只关心
// B-Tree 需要维护的最小数据单元。element 没有父指针，也没有链表指针，
// 这样单个元素的内存开销比较低。
type element[K Key, V any] struct {
	key   K
	value V
}

// node 是 B-Tree 的节点。
//
// elements 按 key 升序排列。children 为空时表示叶子节点；
// children 不为空时，len(children) 总是等于 len(elements)+1。
//
// 对任意 elements[i]：
//   - children[i] 中所有 key 都小于 elements[i].key；
//   - children[i+1] 中所有 key 都大于 elements[i].key。
//
// 这个约束是 B-Tree 能通过二分和单一路径查找 key 的基础。
// 插入、删除、借位、合并这些内部操作都必须维护这个约束。
type node[K Key, V any] struct {
	elements []element[K, V]
	children []*node[K, V]
}

// Map 是基于 B-Tree 的有序 Map。
//
// 和 Go 内置 map 不同，Map 会维护 key 的顺序，因此可以稳定地按 key
// 升序或降序遍历。它的定位更接近其他语言里的 TreeMap：
//   - Put、Get、Delete 提供基础 Map 操作；
//   - Min、Max、Less、LessEqual、Greater、GreaterEqual 提供边界查找；
//   - Range、ReverseRange、RangeFrom、RangeBetween 提供有序遍历能力。
//
// Map 按 key 的自然顺序维护元素，而不是按插入顺序维护元素。
// 因此 Range、Keys、Values、Min、Max 等方法都会体现 key 的排序结果。
// 如果需要保持插入顺序，应该使用 linked map 语义的实现，而不是这个类型。
//
// K 必须满足 Key 约束，也就是标准库 cmp.Ordered 支持的可排序类型。
// 如果需要自定义比较函数，应该单独设计另一种类型，避免让当前类型的 API 变复杂。
//
// Map 不是并发安全的。如果多个 goroutine 同时读写同一个 Map，
// 调用方需要自行加锁。
type Map[K Key, V any] struct {
	// root 是 B-Tree 根节点。空 Map 的 root 为 nil。
	root *node[K, V]
	// length 是当前保存的 key/value 数量。
	length int
	// degree 是 B-Tree 的最小阶数。零值 Map 会在首次写入时使用 kDefaultDegree。
	degree int
}

// New 创建一个使用默认阶数的空 Map。
//
// 默认阶数适合大多数内存场景。除非需要做专门的性能调优，
// 一般直接使用 New 即可。
func New[K Key, V any]() *Map[K, V] {
	return NewWithDegree[K, V](kDefaultDegree)
}

// NewWithDegree 创建一个指定阶数的空 Map。
//
// degree 决定每个 B-Tree 节点最多能保存多少元素。
// degree 越大，树越矮，但节点内部的移动成本越高；
// degree 越小，节点内部移动成本越低，但树高会增加。
//
// degree 需要大于 1。degree <= 1 时会自动使用默认阶数。
func NewWithDegree[K Key, V any](degree int) *Map[K, V] {
	if degree <= 1 {
		degree = kDefaultDegree
	}
	return &Map[K, V]{degree: degree}
}

// Len 返回当前元素数量。
//
// Len 对 nil Map 返回 0，方便调用方在可选 Map 场景中直接使用。
func (m *Map[K, V]) Len() int {
	if m == nil {
		return 0
	}
	return m.length
}

// Get 根据 key 获取 value。
//
// 如果 key 存在，返回对应 value 和 true。
// 如果 key 不存在，返回 value 的零值和 false。
//
// 查询过程会从根节点开始，在每个节点的 elements 中二分定位 key。
// 如果当前节点没有命中，就根据二分得到的位置进入对应子节点。
// B-Tree 的高度通常较低，所以查询路径一般很短。
func (m *Map[K, V]) Get(key K) (V, bool) {
	var zero V
	if m == nil || m.root == nil {
		return zero, false
	}
	return m.root.get(key)
}

// Has 判断 key 是否存在。
//
// Has 只是对 Get 的轻量包装，用于调用方只关心 key 是否存在的场景。
// 它不会区分 value 是否为零值。
func (m *Map[K, V]) Has(key K) bool {
	_, ok := m.Get(key)
	return ok
}

// Put 写入 key 和 value。
//
// 如果 key 不存在，Put 会插入新元素，并返回 value 的零值和 false。
// 如果 key 已存在，Put 会覆盖旧 value，并返回旧 value 和 true。
//
// 插入逻辑使用标准 B-Tree 的“先分裂再下沉”策略：
//  1. 如果根节点已满，先把根节点分裂，树高增加 1；
//  2. 从根节点向下查找插入位置；
//  3. 每次准备进入某个满子节点前，先分裂该子节点；
//  4. 这样真正插入叶子节点时，目标节点一定还有空间。
//
// 这种方式可以避免插入后再一路向上回溯调整树结构。
func (m *Map[K, V]) Put(key K, value V) (V, bool) {
	var zero V
	if m == nil {
		return zero, false
	}
	m.ensureDegree()

	var item = element[K, V]{key: key, value: value}
	if m.root == nil {
		// 空树直接创建根节点。根节点允许少于 degree-1 个元素。
		// 这里不提前分配 2*degree-1 的容量，避免小 Map 一开始就占用较多内存。
		m.root = &node[K, V]{elements: []element[K, V]{item}}
		m.length = 1
		return zero, false
	}

	maxElements := m.maxElements()
	if len(m.root.elements) >= maxElements {
		// 根节点满了以后不能直接继续插入，需要先生成新的根节点，
		// 再把旧根分裂成左右两个子节点。分裂后树高增加 1。
		// 只有根节点分裂会增加树高，普通子节点分裂只会把中间元素上移到父节点。
		var oldRoot = m.root
		m.root = &node[K, V]{children: []*node[K, V]{oldRoot}}
		m.root.splitChild(0, m.degree)
	}

	var old, replaced = m.root.insert(item, m.degree)
	if !replaced {
		m.length++
	}
	return old.value, replaced
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
// 如果 key 存在，Delete 会删除该元素，并返回被删除的 value 和 true。
// 如果 key 不存在，Delete 返回 value 的零值和 false。
//
// 删除逻辑会在向下查找 key 的过程中保证即将进入的子节点至少有 degree 个元素。
// 这样当删除发生在子树中时，子节点删除后仍能满足 B-Tree 的最小元素数量约束。
// 如果目标子节点元素不足，会先从相邻兄弟节点借一个元素，或者和兄弟节点合并。
//
// 注意：即使 key 最终不存在，向下查找过程中也可能已经发生了借位或合并。
// 因此 delete 返回后无论是否真的删除元素，都需要修正可能变空的根节点。
func (m *Map[K, V]) Delete(key K) (V, bool) {
	var zero V
	if m == nil || m.root == nil {
		return zero, false
	}

	var old, deleted = m.root.delete(key, m.degree)
	m.shrinkRoot()
	if !deleted {
		return zero, false
	}
	m.length--
	if m.length == 0 {
		m.root = nil
	}
	return old.value, true
}

// Clear 清空所有元素。
//
// Clear 会直接丢弃根节点，让旧节点交给 GC 回收。
// 这里不会保留节点池，也不会复用旧节点，避免让普通使用场景承担额外复杂度。
func (m *Map[K, V]) Clear() {
	if m == nil {
		return
	}
	m.root = nil
	m.length = 0
}

// Min 返回 key 最小的元素。
//
// B-Tree 中最小 key 一定位于从根节点一路向最左子节点走到的叶子节点中。
// 如果 Map 为空，返回 key/value 的零值和 false。
func (m *Map[K, V]) Min() (K, V, bool) {
	var key K
	var value V
	if m == nil || m.root == nil {
		return key, value, false
	}
	var item, ok = m.root.min()
	if !ok {
		return key, value, false
	}
	return item.key, item.value, true
}

// Max 返回 key 最大的元素。
//
// B-Tree 中最大 key 一定位于从根节点一路向最右子节点走到的叶子节点中。
// 如果 Map 为空，返回 key/value 的零值和 false。
func (m *Map[K, V]) Max() (K, V, bool) {
	var key K
	var value V
	if m == nil || m.root == nil {
		return key, value, false
	}
	var item, ok = m.root.max()
	if !ok {
		return key, value, false
	}
	return item.key, item.value, true
}

// Less 返回严格小于 key 的最大元素。
//
// 如果不存在比 key 小的元素，返回 key/value 的零值和 false。
func (m *Map[K, V]) Less(key K) (K, V, bool) {
	return m.neighbor(key, false, false)
}

// LessEqual 返回小于或等于 key 的最大元素。
//
// 如果 key 本身存在，LessEqual 会返回 key 对应的元素。
// 如果不存在小于或等于 key 的元素，返回 key/value 的零值和 false。
func (m *Map[K, V]) LessEqual(key K) (K, V, bool) {
	return m.neighbor(key, false, true)
}

// Greater 返回严格大于 key 的最小元素。
//
// 如果不存在比 key 大的元素，返回 key/value 的零值和 false。
func (m *Map[K, V]) Greater(key K) (K, V, bool) {
	return m.neighbor(key, true, false)
}

// GreaterEqual 返回大于或等于 key 的最小元素。
//
// 如果 key 本身存在，GreaterEqual 会返回 key 对应的元素。
// 如果不存在大于或等于 key 的元素，返回 key/value 的零值和 false。
func (m *Map[K, V]) GreaterEqual(key K) (K, V, bool) {
	return m.neighbor(key, true, true)
}

// Range 按 key 从小到大遍历所有元素。
//
// fn 返回 false 时会立即停止遍历。
// 如果 Map 为空、Map 为 nil 或 fn 为 nil，Range 不做任何操作。
//
// 遍历使用 B-Tree 的中序遍历：先遍历左子树，再访问当前元素，
// 最后遍历右子树，因此访问顺序就是 key 的升序。
func (m *Map[K, V]) Range(fn func(key K, value V) bool) {
	if m == nil || m.root == nil || fn == nil {
		return
	}
	m.root.rangeAsc(fn)
}

// ReverseRange 按 key 从大到小遍历所有元素。
//
// fn 返回 false 时会立即停止遍历。
// 如果 Map 为空、Map 为 nil 或 fn 为 nil，ReverseRange 不做任何操作。
//
// ReverseRange 是 Range 的反向版本，使用反向中序遍历：
// 先遍历右子树，再访问当前元素，最后遍历左子树。
func (m *Map[K, V]) ReverseRange(fn func(key K, value V) bool) {
	if m == nil || m.root == nil || fn == nil {
		return
	}
	m.root.rangeDesc(fn)
}

// RangeFrom 从第一个大于或等于 key 的元素开始遍历。
//
// fn 返回 false 时会立即停止遍历。
// 如果 Map 为空、Map 为 nil 或 fn 为 nil，RangeFrom 不做任何操作。
//
// RangeFrom 会先在树中定位 from key 应该出现的位置，
// 然后从该位置开始继续做升序遍历。
func (m *Map[K, V]) RangeFrom(key K, fn func(key K, value V) bool) {
	if m == nil || m.root == nil || fn == nil {
		return
	}
	m.root.rangeAscFrom(key, fn)
}

// RangeBetween 按 key 从小到大遍历闭区间 [from, to] 内的元素。
//
// from 和 to 都会被包含在遍历范围内。
// 如果 from > to，或者 Map 为空、Map 为 nil、fn 为 nil，RangeBetween 不做任何操作。
//
// RangeBetween 复用 RangeFrom 定位下界，然后在遍历过程中遇到 key > to 时停止。
// 这样可以避免从树的最小元素开始全量扫描。
func (m *Map[K, V]) RangeBetween(from K, to K, fn func(key K, value V) bool) {
	if m == nil || m.root == nil || fn == nil || cmp.Compare(from, to) > 0 {
		return
	}
	m.RangeFrom(from, func(key K, value V) bool {
		if cmp.Compare(key, to) > 0 {
			return false
		}
		return fn(key, value)
	})
}

// Keys 按 key 从小到大返回所有 key。
//
// 返回的切片是新分配的，调用方可以安全修改切片本身。
// 修改切片不会影响 Map 内部结构。
func (m *Map[K, V]) Keys() []K {
	var keys = make([]K, 0, m.Len())
	m.Range(func(key K, _ V) bool {
		keys = append(keys, key)
		return true
	})
	return keys
}

// Values 按 key 从小到大返回所有 value。
//
// 返回的 value 顺序与 Keys 返回的 key 顺序一致。
// 返回的切片是新分配的，调用方可以安全修改切片本身。
func (m *Map[K, V]) Values() []V {
	var values = make([]V, 0, m.Len())
	m.Range(func(_ K, value V) bool {
		values = append(values, value)
		return true
	})
	return values
}

// maxElements 返回单个节点允许保存的最大元素数量。
//
// B-Tree 的最大元素数量由 degree 决定，公式是 2*degree-1。
func (m *Map[K, V]) maxElements() int {
	m.ensureDegree()
	return 2*m.degree - 1
}

// ensureDegree 确保零值 Map 也能使用默认阶数。
//
// 通过 var m Map[K,V] 声明出来的零值 Map 没有经过 New，
// degree 会是 0。首次写入时需要把它修正为 kDefaultDegree。
func (m *Map[K, V]) ensureDegree() {
	if m.degree <= 1 {
		m.degree = kDefaultDegree
	}
}

// shrinkRoot 修正删除过程中可能产生的空根节点。
//
// B-Tree 允许根节点比普通节点保存更少元素，但根节点如果已经没有元素、
// 同时还有子节点，就应该把唯一子节点提升为新的根节点。
//
// 这个修正不能只在成功删除 key 后执行。删除一个不存在的 key 时，
// 查找路径上也可能先发生子节点合并，导致根节点变空。
func (m *Map[K, V]) shrinkRoot() {
	for m.root != nil && len(m.root.elements) == 0 && len(m.root.children) > 0 {
		// 删除过程中根节点可能只剩一个子节点。把这个子节点提升为根节点，
		// 可以保持树高尽量小，并避免空根节点长期留在树顶。
		m.root = m.root.children[0]
	}
	if m.length == 0 {
		m.root = nil
	}
}

// neighbor 查找 key 的相邻元素。
//
// greater 决定查找方向：
//   - true 表示查找大于或大于等于 key 的最小元素；
//   - false 表示查找小于或小于等于 key 的最大元素。
//
// allowEqual 决定是否允许返回 key 本身：
//   - true 用于 LessEqual 和 GreaterEqual；
//   - false 用于 Less 和 Greater。
//
// 查找过程中会维护一个 candidate。
// candidate 表示当前路径上已经见过、并且可能成为答案的最近元素。
// 如果继续向子树查找后没有更精确的结果，就返回 candidate。
func (m *Map[K, V]) neighbor(key K, greater bool, allowEqual bool) (K, V, bool) {
	var emptyKey K
	var emptyValue V
	if m == nil || m.root == nil {
		return emptyKey, emptyValue, false
	}

	var candidate element[K, V]
	var ok bool
	for current := m.root; current != nil; {
		var index, found = current.find(key)
		if found {
			if allowEqual {
				return current.elements[index].key, current.elements[index].value, true
			}
			if greater {
				if len(current.children) > 0 {
					// Greater 命中当前 key 时，严格更大的最小值优先在右子树最左侧。
					// 右子树存在时，右子树中的所有元素都大于当前 key，
					// 因此其中最小元素就是最接近 key 的严格上界。
					if item, found := current.children[index+1].min(); found {
						return item.key, item.value, true
					}
				}
				if index+1 < len(current.elements) {
					item := current.elements[index+1]
					return item.key, item.value, true
				}
				if ok {
					return candidate.key, candidate.value, true
				}
				return emptyKey, emptyValue, false
			}
			if len(current.children) > 0 {
				// Less 命中当前 key 时，严格更小的最大值优先在左子树最右侧。
				// 左子树存在时，左子树中的所有元素都小于当前 key，
				// 因此其中最大元素就是最接近 key 的严格下界。
				if item, found := current.children[index].max(); found {
					return item.key, item.value, true
				}
			}
			if index > 0 {
				item := current.elements[index-1]
				return item.key, item.value, true
			}
			if ok {
				return candidate.key, candidate.value, true
			}
			return emptyKey, emptyValue, false
		}

		if greater {
			if index < len(current.elements) {
				// 往左侧子树继续查找时，当前 elements[index] 是一个可能的上界候选。
				// 后续如果在子树里找不到更小但仍大于 key 的元素，就返回这个候选。
				candidate = current.elements[index]
				ok = true
			}
		} else if index > 0 {
			// 往右侧子树继续查找时，当前 elements[index-1] 是一个可能的下界候选。
			// 后续如果在子树里找不到更大但仍小于 key 的元素，就返回这个候选。
			candidate = current.elements[index-1]
			ok = true
		}

		if len(current.children) == 0 {
			break
		}
		current = current.children[index]
	}

	if !ok {
		return emptyKey, emptyValue, false
	}
	return candidate.key, candidate.value, true
}

// find 在当前节点内二分查找 key。
//
// 返回值 index 表示 key 命中位置，或者 key 应该插入的位置。
// found 表示 elements[index] 是否正好等于 key。
//
// 当前节点内部 elements 始终保持升序，所以可以使用 sort.Search。
// 对内部节点来说，未命中时 index 也正好是下一层 children[index] 的下探位置。
func (n *node[K, V]) find(key K) (int, bool) {
	var index = sort.Search(len(n.elements), func(i int) bool {
		return cmp.Compare(n.elements[i].key, key) >= 0
	})
	if index < len(n.elements) && cmp.Compare(n.elements[index].key, key) == 0 {
		return index, true
	}
	return index, false
}

// get 从当前节点开始查找 key。
//
// 如果当前节点命中，直接返回 value。
// 如果当前节点未命中且是叶子节点，说明 key 不存在。
// 如果当前节点未命中且有子节点，就进入 find 返回的 index 对应的子节点继续查找。
func (n *node[K, V]) get(key K) (V, bool) {
	var index, found = n.find(key)
	if found {
		return n.elements[index].value, true
	}
	if len(n.children) == 0 {
		var zero V
		return zero, false
	}
	return n.children[index].get(key)
}

// insert 向一个保证未满的节点插入 item。
//
// 调用方需要保证当前节点不是满节点。
// 如果插入过程中需要进入某个满子节点，会先分裂子节点，
// 再决定 item 应该进入左子节点、右子节点，还是替换分裂上来的中间元素。
//
// “先分裂再下沉”的关键点是：递归进入子节点前，子节点一定不是满的。
// 因此叶子节点插入时可以直接移动切片，不需要在递归返回后再处理溢出。
//
// 返回旧 element 和 true 表示发生了替换；
// 返回零值 element 和 false 表示插入了新 key。
func (n *node[K, V]) insert(item element[K, V], degree int) (element[K, V], bool) {
	var index, found = n.find(item.key)
	if found {
		// key 已存在时只替换 value，不改变树结构和元素数量。
		var old = n.elements[index]
		n.elements[index] = item
		return old, true
	}

	if len(n.children) == 0 {
		// 叶子节点中没有子树，直接把元素插入到二分得到的位置。
		n.elements = insertElement(n.elements, index, item)
		return element[K, V]{}, false
	}

	if len(n.children[index].elements) >= 2*degree-1 {
		// 子节点已满，先分裂。分裂会把子节点的中间元素提升到当前节点，
		// 所以插入方向需要根据 item.key 和提升后的 elements[index].key 重新判断。
		// 如果 item.key 正好等于提升上来的 key，本次操作就是覆盖父节点中的元素。
		n.splitChild(index, degree)
		switch cmp.Compare(item.key, n.elements[index].key) {
		case 0:
			var old = n.elements[index]
			n.elements[index] = item
			return old, true
		case 1:
			index++
		}
	}
	return n.children[index].insert(item, degree)
}

// splitChild 分裂当前节点的第 index 个子节点。
//
// 被分裂的子节点必须是满节点。
// 分裂过程：
//  1. 取子节点中间位置 degree-1 的元素作为 median；
//  2. median 左侧元素留在原子节点；
//  3. median 右侧元素移动到新建的右子节点；
//  4. median 插入当前节点；
//  5. 右子节点插入到当前节点 children 的 index+1 位置。
//
// 如果被分裂节点不是叶子节点，子节点切片也要按同样边界拆成左右两半。
func (n *node[K, V]) splitChild(index int, degree int) {
	var child = n.children[index]
	var median = child.elements[degree-1]
	var right = &node[K, V]{}

	// 满节点元素数量是 2*degree-1。
	// median 左右两侧各有 degree-1 个元素，分裂后左右节点都满足最小元素数量。
	right.elements = append(right.elements, child.elements[degree:]...)
	child.elements = truncateElements(child.elements, degree-1)

	if len(child.children) > 0 {
		// 内部节点的 children 数量比 elements 多 1。
		// median 左侧 degree-1 个元素对应前 degree 个子节点；
		// median 右侧 degree-1 个元素对应后 degree 个子节点。
		right.children = append(right.children, child.children[degree:]...)
		child.children = truncateChildren(child.children, degree)
	}

	n.elements = insertElement(n.elements, index, median)
	n.children = insertChild(n.children, index+1, right)
}

// delete 从当前节点开始删除 key。
//
// 删除前会保证准备进入的子节点至少有 degree 个元素。
// 这样子节点删除后即使少一个元素，也不会低于 B-Tree 允许的下限。
//
// 这里采用自顶向下删除。相比删除后再回溯修复，代码可以在下探前
// 就保证子节点“有余量”，从而把大部分修复逻辑限制在当前节点和相邻兄弟节点之间。
func (n *node[K, V]) delete(key K, degree int) (element[K, V], bool) {
	var index, found = n.find(key)
	if found {
		return n.deleteFound(index, degree)
	}
	if len(n.children) == 0 {
		// 到达叶子节点仍未命中，说明 key 不存在。
		return element[K, V]{}, false
	}

	if len(n.children[index].elements) < degree {
		// 目标子节点元素不足，先通过借位或合并让它具备删除能力。
		// ensureChildCanDelete 可能会把目标子节点和左兄弟合并，
		// 所以返回值才是后续应该继续下探的真实 index。
		index = n.ensureChildCanDelete(index, degree)
	}
	return n.children[index].delete(key, degree)
}

// deleteFound 删除当前节点 elements[index] 上已经命中的元素。
//
// 如果当前节点是叶子节点，可以直接删除 elements[index]。
// 如果当前节点是内部节点，需要保持 B-Tree 的排序和节点容量约束：
//   - 左子节点元素足够时，用前驱元素替换当前元素，再删除前驱；
//   - 右子节点元素足够时，用后继元素替换当前元素，再删除后继；
//   - 左右子节点元素都不足时，合并左右子节点和当前元素，再到合并后的子节点删除。
func (n *node[K, V]) deleteFound(index int, degree int) (element[K, V], bool) {
	var old = n.elements[index]
	if len(n.children) == 0 {
		n.elements, _ = removeElement(n.elements, index)
		return old, true
	}

	var left = n.children[index]
	var right = n.children[index+1]
	if len(left.elements) >= degree {
		// 左子节点有足够元素时，用前驱替换当前元素。
		// 前驱是左子树最大元素，替换后仍满足左子树 < 当前元素 < 右子树。
		var predecessor, _ = left.max()
		n.elements[index] = predecessor
		left.delete(predecessor.key, degree)
		return old, true
	}
	if len(right.elements) >= degree {
		// 右子节点有足够元素时，用后继替换当前元素。
		// 后继是右子树最小元素，替换后仍满足左子树 < 当前元素 < 右子树。
		var successor, _ = right.min()
		n.elements[index] = successor
		right.delete(successor.key, degree)
		return old, true
	}

	// 左右子节点都只有最少元素时，无法直接借位。
	// 把当前元素下移并合并左右子节点后，目标 key 一定落在合并后的子节点里。
	n.mergeChildren(index)
	n.children[index].delete(old.key, degree)
	return old, true
}

// ensureChildCanDelete 确保 children[index] 可以继续执行删除。
//
// B-Tree 删除要求向下递归前，目标子节点至少有 degree 个元素。
// 如果目标子节点只有 degree-1 个元素，需要先修复：
//   - 左兄弟元素足够时，从左兄弟借一个元素；
//   - 右兄弟元素足够时，从右兄弟借一个元素；
//   - 两侧兄弟都不够时，与一个兄弟合并。
//
// 合并可能会改变目标子节点在 children 中的位置，所以函数返回修正后的 index。
func (n *node[K, V]) ensureChildCanDelete(index int, degree int) int {
	if index > 0 && len(n.children[index-1].elements) >= degree {
		// 优先从左兄弟借位。借位不会改变目标 child 在 children 中的位置。
		n.borrowFromLeft(index)
		return index
	}
	if index < len(n.children)-1 && len(n.children[index+1].elements) >= degree {
		// 左兄弟不能借时再尝试右兄弟。借位同样不改变目标 child 的位置。
		n.borrowFromRight(index)
		return index
	}
	if index > 0 {
		// 两侧都不能借时必须合并。与左兄弟合并后，
		// 原 children[index] 被并入 children[index-1]，所以下探位置需要左移。
		n.mergeChildren(index - 1)
		return index - 1
	}
	// 没有左兄弟时只能与右兄弟合并，目标 child 仍位于当前 index。
	n.mergeChildren(index)
	return index
}

// borrowFromLeft 从左兄弟节点借一个元素给 children[index]。
//
// 调整过程：
//  1. 父节点中位于 index-1 的分隔元素下移到 child 的最前面；
//  2. 左兄弟的最大元素上移到父节点 index-1；
//  3. 如果有子节点，左兄弟的最后一个子节点也移动到 child 的最前面。
//
// 这样可以保持排序关系：
// left 的所有元素 < 父节点分隔元素 < child 的所有元素。
func (n *node[K, V]) borrowFromLeft(index int) {
	var child = n.children[index]
	var left = n.children[index-1]

	// 父节点分隔元素先下移到 child 最左侧，再用左兄弟最大元素填回父节点。
	// 这个旋转操作会让 child 增加一个元素、left 减少一个元素。
	child.elements = insertElement(child.elements, 0, n.elements[index-1])
	n.elements[index-1], left.elements = removeLastElement(left.elements)
	if len(left.children) > 0 {
		// 非叶子节点还需要同步移动子树。
		// 左兄弟的最后一个子树中所有 key 都位于新的 child 最左元素左侧。
		var moved *node[K, V]
		moved, left.children = removeLastChild(left.children)
		child.children = insertChild(child.children, 0, moved)
	}
}

// borrowFromRight 从右兄弟节点借一个元素给 children[index]。
//
// 调整过程：
//  1. 父节点中位于 index 的分隔元素下移到 child 的最后面；
//  2. 右兄弟的最小元素上移到父节点 index；
//  3. 如果有子节点，右兄弟的第一个子节点也移动到 child 的最后面。
//
// 这样可以保持排序关系：
// child 的所有元素 < 父节点分隔元素 < right 的所有元素。
func (n *node[K, V]) borrowFromRight(index int) {
	var child = n.children[index]
	var right = n.children[index+1]

	// 父节点分隔元素先下移到 child 最右侧，再用右兄弟最小元素填回父节点。
	// 这个旋转操作会让 child 增加一个元素、right 减少一个元素。
	child.elements = append(child.elements, n.elements[index])
	n.elements[index], right.elements = removeFirstElement(right.elements)
	if len(right.children) > 0 {
		// 非叶子节点还需要同步移动子树。
		// 右兄弟的第一个子树中所有 key 都位于新的 child 最右元素右侧。
		var moved *node[K, V]
		moved, right.children = removeFirstChild(right.children)
		child.children = append(child.children, moved)
	}
}

// mergeChildren 合并 children[index]、elements[index]、children[index+1]。
//
// 合并后：
//   - 左子节点会追加父节点分隔元素；
//   - 再追加右子节点的所有元素和子节点；
//   - 父节点删除 elements[index] 和 children[index+1]。
//
// 这个操作用于删除过程中两个相邻子节点都只有最少元素时。
func (n *node[K, V]) mergeChildren(index int) {
	var left = n.children[index]
	var right = n.children[index+1]
	var middle element[K, V]
	// 父节点中的 middle 是左右子节点之间的分隔元素。
	// 下移到 left 后，left + middle + right 组成一个有序节点。
	n.elements, middle = removeElement(n.elements, index)
	left.elements = append(left.elements, middle)
	left.elements = append(left.elements, right.elements...)
	left.children = append(left.children, right.children...)
	n.children, _ = removeChild(n.children, index+1)
}

// min 返回当前子树中 key 最小的 element。
//
// 最小元素位于最左路径的最后一个节点。
func (n *node[K, V]) min() (element[K, V], bool) {
	current := n
	for len(current.children) > 0 {
		current = current.children[0]
	}
	if len(current.elements) == 0 {
		return element[K, V]{}, false
	}
	return current.elements[0], true
}

// max 返回当前子树中 key 最大的 element。
//
// 最大元素位于最右路径的最后一个节点。
func (n *node[K, V]) max() (element[K, V], bool) {
	current := n
	for len(current.children) > 0 {
		current = current.children[len(current.children)-1]
	}
	if len(current.elements) == 0 {
		return element[K, V]{}, false
	}
	return current.elements[len(current.elements)-1], true
}

// rangeAsc 从当前节点开始按 key 升序遍历。
//
// 对 B-Tree 节点来说，升序遍历顺序是：
// child[0]、element[0]、child[1]、element[1]、...、最后一个 child。
// fn 返回 false 时立即停止，并把 false 一路向上传递给调用方。
func (n *node[K, V]) rangeAsc(fn func(key K, value V) bool) bool {
	for i, item := range n.elements {
		if len(n.children) > 0 {
			if !n.children[i].rangeAsc(fn) {
				return false
			}
		}
		if !fn(item.key, item.value) {
			return false
		}
	}
	if len(n.children) > 0 {
		return n.children[len(n.children)-1].rangeAsc(fn)
	}
	return true
}

// rangeDesc 从当前节点开始按 key 降序遍历。
//
// 降序遍历与 rangeAsc 相反：
// 先访问最后一个 child，再从后往前访问 element 和它左侧的 child。
func (n *node[K, V]) rangeDesc(fn func(key K, value V) bool) bool {
	if len(n.children) > 0 {
		if !n.children[len(n.children)-1].rangeDesc(fn) {
			return false
		}
	}
	for i := len(n.elements); i > 0; i-- {
		var index = i - 1
		var item = n.elements[index]
		if !fn(item.key, item.value) {
			return false
		}
		if len(n.children) > 0 {
			if !n.children[index].rangeDesc(fn) {
				return false
			}
		}
	}
	return true
}

// rangeAscFrom 从当前节点开始，按 key 升序遍历所有大于或等于 key 的元素。
//
// find 返回的 index 是 key 在当前节点中应该出现的位置。
// 对 index 左侧的 elements 和 children，它们都严格小于 key，可以跳过。
// 对 index 位置之后的 elements 和 children，需要继续按升序访问。
//
// 第一个下探子树仍然使用 rangeAscFrom，因为里面可能同时包含小于 key
// 和大于等于 key 的元素；后续右侧子树已经整体大于当前 element，
// 可以直接使用 rangeAsc 做完整遍历。
func (n *node[K, V]) rangeAscFrom(key K, fn func(key K, value V) bool) bool {
	var index, _ = n.find(key)
	if len(n.children) > 0 {
		if !n.children[index].rangeAscFrom(key, fn) {
			return false
		}
	}
	for i := index; i < len(n.elements); i++ {
		if cmp.Compare(n.elements[i].key, key) >= 0 {
			if !fn(n.elements[i].key, n.elements[i].value) {
				return false
			}
		}
		if len(n.children) > 0 && !n.children[i+1].rangeAsc(fn) {
			return false
		}
	}
	return true
}

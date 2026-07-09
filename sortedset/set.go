package sortedset

const (
	// kMaxLevel 是 skip list 的最大层数。
	//
	// Redis sorted set 也使用 32 层。按 1/4 的晋升概率计算，32 层足够覆盖非常大的
	// 元素数量，同时每个节点只分配自己实际需要的层数，避免为小层级节点浪费过多内存。
	kMaxLevel = 32
	// kDefaultSeed 是零值 Set 第一次写入时使用的伪随机种子。
	//
	// Set 本身不是并发安全结构，这里使用轻量级 xorshift 生成层高，避免在插入热路径上
	// 使用 math/rand 的锁和接口成本。固定种子也让 benchmark 更稳定。
	kDefaultSeed uint64 = 0x9e3779b97f4a7c15
	// kLevelProbability 表示节点晋升到上一层的概率，当前为 1/4。
	//
	// 较低的晋升概率会减少高层指针数量，降低内存占用；较高的晋升概率会让查找路径更短。
	// 1/4 是 Redis skip list 使用的折中值。
	kLevelProbability = ^uint64(0) / 4
)

// level 是 skip list 节点在某一层上的前进指针和跨度。
//
// forward 指向同一层的下一个节点。
// span 表示从当前节点沿 forward 前进到下一个节点时，跨过了底层链表中的多少个节点。
// span 是 rank 查询的关键：查找过程中累计 span，就能在 O(log n) 内得到元素排名。
type level[M Member] struct {
	forward *node[M]
	span    uint64
}

// node 是 sorted set 在 skip list 中保存的节点。
//
// 同一个 node 会同时被 table 引用，用于按 member O(1) 定位；也会被 skip list
// 引用，用于按 score/member 有序遍历。
//
// levels 使用变长切片，而不是固定 [32]level，是为了避免每个节点都承担最大层数的内存成本。
// 这会让每个节点多一次层切片分配，但对大多数业务场景比固定 32 层节点更节省内存。
type node[M Member] struct {
	member   M
	score    float64
	backward *node[M]
	levels   []level[M]
}

// Set 是按 score 排序、member 唯一的有序集合。
//
// Set 的结构和 Redis sorted set 接近：
//   - table 用于按 member 快速定位节点，GetScore、Has、Delete 的定位成本为平均 O(1)；
//   - skip list 用于按 score/member 顺序遍历、按 rank 定位，以及按 score 范围扫描；
//   - skip list 的 span 信息让 Rank 和按 rank 定位保持 O(log n)。
//
// 排序规则为：
//   - score 小的排在前面；
//   - score 相同的时候，member 小的排在前面。
//
// NaN 不支持作为 score 或 member。为了避免影响写入、查询、删除等热路径性能，
// Set 不会在运行时检查 NaN，调用方需要保证传入的数据合法。
//
// Set 不是并发安全的。如果多个 goroutine 同时读写同一个 Set，调用方需要自行加锁。
type Set[M Member] struct {
	// table 保存 member 到 skip list 节点的映射。
	table map[M]*node[M]
	// header 是 skip list 的哨兵头节点，不保存业务 member。
	header *node[M]
	// tail 指向 score/member 最大的节点，便于反向遍历和 Max。
	tail *node[M]
	// level 是当前 skip list 使用到的最高层数。
	level int
	// length 是当前元素数量。
	length int
	// seed 是当前 Set 用于生成随机层高的伪随机状态。
	seed uint64
}

// New 创建一个空 Set。
func New[M Member]() *Set[M] {
	return NewWithCapacity[M](0)
}

// NewWithCapacity 创建一个带初始容量的空 Set。
//
// capacity 会用于初始化 member 索引 table。调用方如果大致知道元素数量，
// 可以通过这个函数减少 table 扩容次数。
//
// capacity 小于 0 时会按 0 处理。
func NewWithCapacity[M Member](capacity int) *Set[M] {
	if capacity < 0 {
		capacity = 0
	}
	s := &Set[M]{table: make(map[M]*node[M], capacity)}
	s.init()
	return s
}

// Len 返回当前元素数量。
//
// Len 对 nil Set 返回 0，方便调用方在可选 Set 场景中直接使用。
func (s *Set[M]) Len() int {
	if s == nil {
		return 0
	}
	return s.length
}

// GetScore 根据 member 获取 score。
//
// 如果 member 存在，返回对应 score 和 true。
// 如果 member 不存在，返回 0 和 false。
func (s *Set[M]) GetScore(member M) (float64, bool) {
	if s == nil || s.table == nil {
		return 0, false
	}
	item, ok := s.table[member]
	if !ok {
		return 0, false
	}
	return item.score, true
}

// Has 判断 member 是否存在。
//
// Has 只检查 member 索引，不关心 score 的具体值。
func (s *Set[M]) Has(member M) bool {
	if s == nil || s.table == nil {
		return false
	}
	_, ok := s.table[member]
	return ok
}

// Put 写入 member 和 score。
//
// 如果 member 不存在，Put 会插入新节点，并返回 0 和 false。
// 如果 member 已存在，Put 会返回旧 score 和 true。
//
// 当旧 score 和新 score 排序上相等时，节点位置不需要变化；否则会先从 skip list
// 删除旧节点，再按新 score 重新插入。table 中始终只保留 member 的最新节点。
//
// NaN 不支持作为 score 或 member。为了避免影响写入热路径，Put 不会做运行时检查；
// 调用方需要保证传入的 score 和 member 都不是 NaN。
func (s *Set[M]) Put(member M, score float64) (float64, bool) {
	if s == nil {
		return 0, false
	}
	s.ensure()
	if item, ok := s.table[member]; ok {
		old := item.score
		if old == score {
			item.score = score
			return old, true
		}
		s.deleteNode(item)
		s.table[member] = s.insertNode(member, score)
		return old, true
	}
	s.table[member] = s.insertNode(member, score)
	return 0, false
}

// Set 写入 member 和 score，并忽略旧 score。
//
// Set 适合调用方只关心最终写入结果、不关心 member 原来是否存在的场景。
// 如果需要知道本次写入是新增还是覆盖，或者需要拿到旧 score，应使用 Put。
func (s *Set[M]) Set(member M, score float64) {
	s.Put(member, score)
}

// Delete 删除指定 member。
//
// 如果 member 存在，Delete 会从 table 和 skip list 中同时删除该节点，
// 并返回被删除的 score 和 true。
// 如果 member 不存在，Delete 返回 0 和 false。
func (s *Set[M]) Delete(member M) (float64, bool) {
	if s == nil || s.table == nil {
		return 0, false
	}
	item, ok := s.table[member]
	if !ok {
		return 0, false
	}
	delete(s.table, member)
	s.deleteNode(item)
	return item.score, true
}

// Clear 清空所有元素。
//
// Clear 会直接丢弃 table 和 skip list 节点引用，让旧节点交给 GC 回收。
// 这里不会保留节点池，避免让普通使用场景承担额外复杂度。
func (s *Set[M]) Clear() {
	if s == nil {
		return
	}
	s.table = nil
	s.header = nil
	s.tail = nil
	s.level = 0
	s.length = 0
}

// Min 返回排序最靠前的元素。
//
// 如果 Set 为空，返回 member 零值、0 和 false。
func (s *Set[M]) Min() (M, float64, bool) {
	var member M
	if s == nil || s.header == nil {
		return member, 0, false
	}
	item := s.header.levels[0].forward
	if item == nil {
		return member, 0, false
	}
	return item.member, item.score, true
}

// Max 返回排序最后面的元素。
//
// 如果 Set 为空，返回 member 零值、0 和 false。
func (s *Set[M]) Max() (M, float64, bool) {
	var member M
	if s == nil || s.tail == nil {
		return member, 0, false
	}
	return s.tail.member, s.tail.score, true
}

// PopMin 删除并返回排序最靠前的元素。
//
// 如果 Set 为空，返回 member 零值、0 和 false。
// PopMin 会同时删除 table 中的 member 映射和 skip list 中的节点。
func (s *Set[M]) PopMin() (M, float64, bool) {
	var member M
	if s == nil || s.header == nil {
		return member, 0, false
	}
	item := s.header.levels[0].forward
	if item == nil {
		return member, 0, false
	}
	member = item.member
	score := item.score
	delete(s.table, member)
	s.deleteNode(item)
	return member, score, true
}

// PopMax 删除并返回排序最后面的元素。
//
// 如果 Set 为空，返回 member 零值、0 和 false。
// PopMax 通过 tail 直接定位最大元素，再从 skip list 中删除该节点。
func (s *Set[M]) PopMax() (M, float64, bool) {
	var member M
	if s == nil || s.tail == nil {
		return member, 0, false
	}
	item := s.tail
	member = item.member
	score := item.score
	delete(s.table, member)
	s.deleteNode(item)
	return member, score, true
}

// Rank 返回 member 的正向排名。
//
// 排名从 0 开始，score/member 最小的元素 rank 为 0。
// 如果 member 不存在，返回 0 和 false。
func (s *Set[M]) Rank(member M) (int, bool) {
	if s == nil || s.table == nil {
		return 0, false
	}
	item, ok := s.table[member]
	if !ok {
		return 0, false
	}
	rank, ok := s.rankOf(item)
	if !ok {
		return 0, false
	}
	return int(rank - 1), true
}

// RevRank 返回 member 的反向排名。
//
// 排名从 0 开始，score/member 最大的元素 rev rank 为 0。
// 如果 member 不存在，返回 0 和 false。
func (s *Set[M]) RevRank(member M) (int, bool) {
	rank, ok := s.Rank(member)
	if !ok {
		return 0, false
	}
	return s.length - 1 - rank, true
}

// Range 按 score/member 从小到大遍历所有元素。
//
// fn 返回 false 时会立即停止遍历。
// 如果 Set 为空、Set 为 nil 或 fn 为 nil，Range 不做任何操作。
//
// Range 适合只读遍历。不要在 fn 中修改当前 Set；插入、删除或更新 score 都可能改变
// skip list 的 forward/backward 指针，使当前遍历过程跳过元素或重复访问元素。
func (s *Set[M]) Range(fn func(M, float64) bool) {
	if s == nil || s.header == nil || fn == nil {
		return
	}
	for item := s.header.levels[0].forward; item != nil; item = item.levels[0].forward {
		if !fn(item.member, item.score) {
			return
		}
	}
}

// ReverseRange 按 score/member 从大到小遍历所有元素。
//
// fn 返回 false 时会立即停止遍历。
// 如果 Set 为空、Set 为 nil 或 fn 为 nil，ReverseRange 不做任何操作。
//
// ReverseRange 适合只读遍历。不要在 fn 中修改当前 Set；反向遍历依赖 bottom level
// 的 backward 指针，遍历期间修改集合会让后续位置不再稳定。
func (s *Set[M]) ReverseRange(fn func(M, float64) bool) {
	if s == nil || fn == nil {
		return
	}
	for item := s.tail; item != nil; item = item.backward {
		if !fn(item.member, item.score) {
			return
		}
	}
}

// RangeByRank 按正向 rank 范围遍历元素。
//
// start 和 stop 都是 0-based 且包含边界。负数表示从末尾倒数：
//   - -1 表示最后一个元素；
//   - -2 表示倒数第二个元素。
//
// 例如 RangeByRank(0, 2, fn) 会遍历前三个元素。
// 如果范围为空、Set 为空或 fn 为 nil，不做任何操作。
//
// RangeByRank 会先用 span 定位 start 对应节点，再沿 bottom level 顺序遍历到 stop。
// 定位成本为 O(log n)，遍历成本为 O(k)，k 为返回的元素数量。
func (s *Set[M]) RangeByRank(start int, stop int, fn func(M, float64) bool) {
	if s == nil || s.header == nil || fn == nil {
		return
	}
	start, stop, ok := normalizeRange(s.length, start, stop)
	if !ok {
		return
	}
	item := s.nodeByRank(uint64(start + 1))
	for index := start; item != nil && index <= stop; index++ {
		next := item.levels[0].forward
		if !fn(item.member, item.score) {
			return
		}
		item = next
	}
}

// RevRangeByRank 按反向 rank 范围遍历元素。
//
// start 和 stop 都是基于反向顺序的 0-based 且包含边界。
// 也就是说，RevRangeByRank(0, 0, fn) 会遍历 Max 返回的元素。
//
// RevRangeByRank 会先把反向 rank 转成正向 1-based rank，再沿 backward 指针向前遍历。
// 定位成本为 O(log n)，遍历成本为 O(k)。
func (s *Set[M]) RevRangeByRank(start int, stop int, fn func(M, float64) bool) {
	if s == nil || fn == nil {
		return
	}
	start, stop, ok := normalizeRange(s.length, start, stop)
	if !ok {
		return
	}
	item := s.nodeByRank(uint64(s.length - start))
	for index := start; item != nil && index <= stop; index++ {
		prev := item.backward
		if !fn(item.member, item.score) {
			return
		}
		item = prev
	}
}

// RangeByScore 按 score 范围从小到大遍历元素。
//
// min 和 max 都是闭区间边界。score 等于 min 或 max 的元素会被包含在结果中。
// 如果 min 大于 max、Set 为空或 fn 为 nil，不做任何操作。
//
// RangeByScore 会先从最高层开始跳过所有 score < min 的节点，定位到第一个
// score >= min 的节点后，再沿 bottom level 顺序扫描到 score > max。
func (s *Set[M]) RangeByScore(min float64, max float64, fn func(M, float64) bool) {
	if s == nil || s.header == nil || fn == nil || min > max {
		return
	}
	item := s.firstInScoreRange(min)
	for item != nil && item.score <= max {
		next := item.levels[0].forward
		if !fn(item.member, item.score) {
			return
		}
		item = next
	}
}

// Count 返回 score 在闭区间 [min, max] 内的元素数量。
//
// Count 不会线性扫描整个区间。它会分别定位区间内第一个和最后一个节点，
// 再通过 span 计算两者的 1-based rank，最后用 rank 差值得到数量。
func (s *Set[M]) Count(min float64, max float64) int {
	if s == nil || s.header == nil || min > max {
		return 0
	}
	first := s.firstInScoreRange(min)
	if first == nil || first.score > max {
		return 0
	}
	last := s.lastInScoreRange(max)
	if last == nil || last.score < min {
		return 0
	}
	firstRank, firstOK := s.rankOf(first)
	lastRank, lastOK := s.rankOf(last)
	if !firstOK || !lastOK || lastRank < firstRank {
		return 0
	}
	return int(lastRank - firstRank + 1)
}

// DeleteByRank 删除正向 rank 范围内的元素，并返回删除数量。
//
// start 和 stop 都是 0-based 且包含边界。负数表示从末尾倒数：
//   - -1 表示最后一个元素；
//   - -2 表示倒数第二个元素。
//
// 如果范围为空或 Set 为空，返回 0。
//
// DeleteByRank 会先定位 start 对应节点，并记录该节点每一层的前驱 update。
// 后续连续删除时，被删除节点都在同一个范围内，update 始终指向删除范围前的节点；
// 每次 unlinkNode 都会基于这组前驱修正 forward 和 span，因此不需要为每个节点重新查找前驱。
func (s *Set[M]) DeleteByRank(start int, stop int) int {
	if s == nil || s.header == nil {
		return 0
	}
	start, stop, ok := normalizeRange(s.length, start, stop)
	if !ok {
		return 0
	}
	var update [kMaxLevel]*node[M]
	traversed, item := s.findByRank(uint64(start+1), &update)
	if item == nil {
		return 0
	}
	var deleted int
	for item != nil && int(traversed) <= stop+1 {
		next := item.levels[0].forward
		delete(s.table, item.member)
		s.unlinkNode(item, update)
		deleted++
		traversed++
		item = next
	}
	return deleted
}

// DeleteByScore 删除 score 在闭区间 [min, max] 内的元素，并返回删除数量。
//
// 如果 min 大于 max 或 Set 为空，返回 0。
//
// DeleteByScore 会先找到所有 score < min 的最后一个节点，并把它作为每一层的前驱。
// 之后沿 bottom level 删除 score <= max 的连续节点。由于删除范围在排序上连续，
// 同一组前驱可以被连续复用，避免为每个删除节点重复做 O(log n) 查找。
func (s *Set[M]) DeleteByScore(min float64, max float64) int {
	if s == nil || s.header == nil || min > max {
		return 0
	}
	var update [kMaxLevel]*node[M]
	x := s.header
	for i := s.level - 1; i >= 0; i-- {
		for x.levels[i].forward != nil && x.levels[i].forward.score < min {
			x = x.levels[i].forward
		}
		update[i] = x
	}

	var deleted int
	item := x.levels[0].forward
	for item != nil && item.score <= max {
		next := item.levels[0].forward
		delete(s.table, item.member)
		s.unlinkNode(item, update)
		deleted++
		item = next
	}
	return deleted
}

// Members 按 score/member 从小到大返回所有 member。
//
// 返回的切片是新分配的，调用方可以安全修改切片本身。
func (s *Set[M]) Members() []M {
	members := make([]M, 0, s.Len())
	s.Range(func(member M, _ float64) bool {
		members = append(members, member)
		return true
	})
	return members
}

// Scores 按 score/member 从小到大返回所有 score。
//
// 返回的 score 顺序与 Members 返回的 member 顺序一致。
func (s *Set[M]) Scores() []float64 {
	scores := make([]float64, 0, s.Len())
	s.Range(func(_ M, score float64) bool {
		scores = append(scores, score)
		return true
	})
	return scores
}

// ensure 确保零值 Set 在首次写入时可以使用。
func (s *Set[M]) ensure() {
	if s.table == nil {
		s.table = make(map[M]*node[M])
	}
	if s.header == nil {
		s.init()
	}
}

// init 初始化 skip list 的头节点和随机种子。
//
// header 是哨兵节点，不保存业务数据。它拥有 kMaxLevel 层，用来承接任意高度的
// 第一个真实节点；普通节点只分配自己实际随机出来的层数。
func (s *Set[M]) init() {
	s.header = &node[M]{levels: make([]level[M], kMaxLevel)}
	s.level = 1
	if s.seed == 0 {
		s.seed = kDefaultSeed
	}
}

// insertNode 把 member/score 插入 skip list，并返回新节点。
//
// 调用方必须保证 skip list 中还不存在这个 member。
// 插入过程会记录每一层的前驱节点 update 和前驱累计 rank，用于修正 forward 和 span。
//
// rank[i] 表示走到 update[i] 时已经跨过的 bottom level 节点数量。
// 新节点插入后，update[i] 原本指向后继节点的 span 会被拆成两段：
//   - update[i] 到新节点的 span；
//   - 新节点到原后继节点的 span。
//
// 对新节点不存在的更高层，只需要把经过该位置的 span 加 1，因为 bottom level
// 多了一个真实节点。
func (s *Set[M]) insertNode(member M, score float64) *node[M] {
	var update [kMaxLevel]*node[M]
	var rank [kMaxLevel]uint64

	x := s.header
	for i := s.level - 1; i >= 0; i-- {
		if i == s.level-1 {
			rank[i] = 0
		} else {
			rank[i] = rank[i+1]
		}
		for x.levels[i].forward != nil && nodeLessThan(x.levels[i].forward, score, member) {
			rank[i] += x.levels[i].span
			x = x.levels[i].forward
		}
		update[i] = x
	}

	nodeLevel := s.randomLevel()
	if nodeLevel > s.level {
		for i := s.level; i < nodeLevel; i++ {
			rank[i] = 0
			update[i] = s.header
			update[i].levels[i].span = uint64(s.length)
		}
		s.level = nodeLevel
	}

	x = &node[M]{
		member: member,
		score:  score,
		levels: make([]level[M], nodeLevel),
	}
	for i := 0; i < nodeLevel; i++ {
		x.levels[i].forward = update[i].levels[i].forward
		update[i].levels[i].forward = x

		x.levels[i].span = update[i].levels[i].span - (rank[0] - rank[i])
		update[i].levels[i].span = rank[0] - rank[i] + 1
	}
	for i := nodeLevel; i < s.level; i++ {
		update[i].levels[i].span++
	}

	if update[0] != s.header {
		x.backward = update[0]
	}
	if x.levels[0].forward != nil {
		x.levels[0].forward.backward = x
	} else {
		s.tail = x
	}
	s.length++
	return x
}

// deleteNode 从 skip list 中删除节点。
//
// 调用方负责维护 table，这里只维护 skip list 指针、span、tail、level 和 length。
// deleteNode 适合只知道目标节点、但还没有前驱 update 的场景，例如 Delete 和 PopMax。
// 它会先按 score/member 重新查找每一层前驱，再交给 unlinkNode 做真正摘除。
func (s *Set[M]) deleteNode(item *node[M]) {
	var update [kMaxLevel]*node[M]
	x := s.header
	for i := s.level - 1; i >= 0; i-- {
		for x.levels[i].forward != nil && nodeLessThan(x.levels[i].forward, item.score, item.member) {
			x = x.levels[i].forward
		}
		update[i] = x
	}
	s.unlinkNode(item, update)
}

// unlinkNode 根据每一层的前驱节点，把 item 从 skip list 中摘除。
//
// 如果 update[i].forward 正好是 item，说明 item 存在于第 i 层，需要把 update[i]
// 直接连到 item 的后继，并把两段 span 合并后减去被删除节点本身。
// 如果 item 不存在于第 i 层，说明这一层只是跨过了 item 所在的 bottom level 位置，
// 对应 span 只需要减 1。
//
// item 在 bottom level 的后继需要回指 item.backward；如果没有后继，说明 item 是 tail，
// 删除后 tail 要回退到 item.backward。最高层如果变空，也要同步降低 s.level。
func (s *Set[M]) unlinkNode(item *node[M], update [kMaxLevel]*node[M]) {
	for i := 0; i < s.level; i++ {
		if update[i].levels[i].forward == item {
			update[i].levels[i].span += item.levels[i].span - 1
			update[i].levels[i].forward = item.levels[i].forward
		} else {
			update[i].levels[i].span--
		}
	}
	if item.levels[0].forward != nil {
		item.levels[0].forward.backward = item.backward
	} else {
		s.tail = item.backward
	}
	for s.level > 1 && s.header.levels[s.level-1].forward == nil {
		s.level--
	}
	s.length--
}

// rankOf 返回 item 的 1-based rank。
//
// 返回 1-based rank 是为了和 skip list span 的累计值保持一致。
// 对外的 Rank 方法会再转换成 Go 使用更自然的 0-based rank。
func (s *Set[M]) rankOf(item *node[M]) (uint64, bool) {
	var rank uint64
	x := s.header
	for i := s.level - 1; i >= 0; i-- {
		for x.levels[i].forward != nil && nodeLessOrEqual(x.levels[i].forward, item.score, item.member) {
			rank += x.levels[i].span
			x = x.levels[i].forward
		}
		if x == item {
			return rank, true
		}
	}
	return 0, false
}

// findByRank 根据 1-based rank 返回节点，并在 update 中记录该节点每一层的前驱。
//
// DeleteByRank 会连续删除多个节点。删除第一个节点后，update 仍然指向删除范围前的
// 前驱节点，后续每次 unlinkNode 都可以继续使用同一组前驱修正 forward 和 span。
func (s *Set[M]) findByRank(rank uint64, update *[kMaxLevel]*node[M]) (uint64, *node[M]) {
	if rank == 0 || rank > uint64(s.length) {
		return 0, nil
	}
	var traversed uint64
	x := s.header
	for i := s.level - 1; i >= 0; i-- {
		for x.levels[i].forward != nil && traversed+x.levels[i].span < rank {
			traversed += x.levels[i].span
			x = x.levels[i].forward
		}
		if update != nil {
			update[i] = x
		}
	}
	return traversed + 1, x.levels[0].forward
}

// nodeByRank 根据 1-based rank 返回节点。
//
// nodeByRank 通过累计 span 定位节点。每次只要 traversed+span 不超过目标 rank，
// 就可以在当前层继续向前跳；跳不动时下沉到下一层。最终 traversed 等于 rank 时，
// 当前节点就是目标节点。
func (s *Set[M]) nodeByRank(rank uint64) *node[M] {
	if rank == 0 || rank > uint64(s.length) {
		return nil
	}
	var traversed uint64
	x := s.header
	for i := s.level - 1; i >= 0; i-- {
		for x.levels[i].forward != nil && traversed+x.levels[i].span <= rank {
			traversed += x.levels[i].span
			x = x.levels[i].forward
		}
		if traversed == rank {
			return x
		}
	}
	return nil
}

// firstInScoreRange 返回第一个 score >= min 的节点。
//
// 查找过程从最高层开始，持续跳过 score < min 的节点。循环结束时，x 是最后一个
// score < min 的节点，因此 x.levels[0].forward 就是第一个满足 score >= min 的节点。
func (s *Set[M]) firstInScoreRange(min float64) *node[M] {
	x := s.header
	for i := s.level - 1; i >= 0; i-- {
		for x.levels[i].forward != nil && x.levels[i].forward.score < min {
			x = x.levels[i].forward
		}
	}
	return x.levels[0].forward
}

// lastInScoreRange 返回最后一个 score <= max 的节点。
//
// 查找过程从最高层开始，持续跳过 score <= max 的节点。循环结束时，x 就是最后一个
// 满足 score <= max 的节点；如果 x 仍然是 header，说明没有任何节点落在该上界内。
func (s *Set[M]) lastInScoreRange(max float64) *node[M] {
	x := s.header
	for i := s.level - 1; i >= 0; i-- {
		for x.levels[i].forward != nil && x.levels[i].forward.score <= max {
			x = x.levels[i].forward
		}
	}
	if x == s.header {
		return nil
	}
	return x
}

// randomLevel 生成新节点层高。
//
// 每次晋升到上一层的概率为 1/4。绝大多数节点只会有 1 层，少量节点会成为高层索引。
// 这种分布能让查找路径保持期望 O(log n)，同时避免每个节点都持有大量 forward 指针。
func (s *Set[M]) randomLevel() int {
	lv := 1
	for lv < kMaxLevel && s.nextRand() < kLevelProbability {
		lv++
	}
	return lv
}

// nextRand 使用 xorshift64* 生成伪随机数。
//
// Set 不是并发安全结构，因此这里不做加锁。
func (s *Set[M]) nextRand() uint64 {
	x := s.seed
	if x == 0 {
		x = kDefaultSeed
	}
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	s.seed = x
	return x * 2685821657736338717
}

// nodeLessThan 判断 item 是否排在 score/member 前面。
//
// sortedset 文档明确不支持 NaN，因此这里直接使用浮点比较，避免 cmp.Compare
// 在插入、删除和 rank 查询热路径上的额外成本。
func nodeLessThan[M Member](item *node[M], score float64, member M) bool {
	return item.score < score || (item.score == score && item.member < member)
}

// nodeLessOrEqual 判断 item 是否排在 score/member 前面或正好相等。
func nodeLessOrEqual[M Member](item *node[M], score float64, member M) bool {
	return item.score < score || (item.score == score && item.member <= member)
}

// normalizeRange 把支持负数下标的闭区间转换成 [0, length-1] 内的有效范围。
//
// 负数下标按 Go 切片常见习惯表示从末尾倒数：-1 是最后一个元素。
// 归一化后如果区间为空，会返回 ok=false，让调用方直接跳过。
func normalizeRange(length int, start int, stop int) (int, int, bool) {
	if length <= 0 {
		return 0, 0, false
	}
	if start < 0 {
		start = length + start
	}
	if stop < 0 {
		stop = length + stop
	}
	if start < 0 {
		start = 0
	}
	if stop >= length {
		stop = length - 1
	}
	if start > stop || start >= length || stop < 0 {
		return 0, 0, false
	}
	return start, stop, true
}

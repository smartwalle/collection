package treemap

import "slices"

// insertElement 在 elements 的 index 位置插入 item。
//
// B-Tree 节点内部 elements 始终按 key 升序排列。
// 调用方会先通过二分得到正确的 index，然后用这个函数完成切片插入。
// 这里使用标准库 slices.Insert，让元素移动和容量扩展逻辑交给标准库维护。
func insertElement[K Key, V any](elements []element[K, V], index int, item element[K, V]) []element[K, V] {
	return slices.Insert(elements, index, item)
}

// removeElement 删除 elements[index]，并返回删除后的切片和被删除的元素。
//
// 删除 B-Tree 节点中的元素时，经常需要同时知道被删除元素，
// 例如合并子节点时要把父节点中的分隔元素下移到子节点。
// slices.Delete 会把 index 后面的元素整体左移，并返回新的切片视图。
func removeElement[K Key, V any](elements []element[K, V], index int) ([]element[K, V], element[K, V]) {
	item := elements[index]
	return slices.Delete(elements, index, index+1), item
}

// removeLastElement 删除 elements 中最后一个元素。
//
// 从左兄弟节点借元素时，需要把左兄弟的最大元素移到父节点。
// 因为 elements 是升序排列的，最大元素就是最后一个元素。
//
// 这里会把原尾部位置清零，避免 value 中如果带有指针时被底层数组继续引用。
func removeLastElement[K Key, V any](elements []element[K, V]) (element[K, V], []element[K, V]) {
	index := len(elements) - 1
	item := elements[index]
	var zero element[K, V]
	elements[index] = zero
	return item, elements[:index]
}

// removeFirstElement 删除 elements 中第一个元素。
//
// 从右兄弟节点借元素时，需要把右兄弟的最小元素移到父节点。
// 因为 elements 是升序排列的，最小元素就是第一个元素。
// 这里不手动清理尾部位置，交给 slices.Delete 使用标准库的实现细节。
func removeFirstElement[K Key, V any](elements []element[K, V]) (element[K, V], []element[K, V]) {
	item := elements[0]
	return item, slices.Delete(elements, 0, 1)
}

// truncateElements 把 elements 截断到指定长度。
//
// 分裂节点时，左节点会保留中间元素左侧的数据。
// 截断前会把被截掉的位置清零，避免底层数组继续持有 value 中的指针。
func truncateElements[K Key, V any](elements []element[K, V], length int) []element[K, V] {
	var zero element[K, V]
	for i := length; i < len(elements); i++ {
		elements[i] = zero
	}
	return elements[:length]
}

// insertChild 在 children 的 index 位置插入 child。
//
// 当一个满子节点分裂时，会产生新的右子节点；
// 新右子节点需要插入到原子节点后面，也就是 index+1 位置。
// children 的顺序和 elements 的分隔关系强相关，插入位置必须由调用方保证正确。
func insertChild[K Key, V any](children []*node[K, V], index int, child *node[K, V]) []*node[K, V] {
	return slices.Insert(children, index, child)
}

// removeChild 删除 children[index]，并返回删除后的切片和被删除的子节点。
//
// 合并两个子节点后，右子节点已经被并入左子节点，
// 父节点需要删除原来的右子节点指针。
// 返回被删除的子节点主要是为了让这个工具函数语义完整，当前调用方可以忽略它。
func removeChild[K Key, V any](children []*node[K, V], index int) ([]*node[K, V], *node[K, V]) {
	child := children[index]
	return slices.Delete(children, index, index+1), child
}

// removeLastChild 删除 children 中最后一个子节点。
//
// 从左兄弟借元素时，如果节点不是叶子节点，
// 左兄弟的最后一个子节点也要移动到当前子节点的最前面。
// 被移走的位置会被置为 nil，避免底层数组继续持有整棵子树。
func removeLastChild[K Key, V any](children []*node[K, V]) (*node[K, V], []*node[K, V]) {
	index := len(children) - 1
	child := children[index]
	children[index] = nil
	return child, children[:index]
}

// removeFirstChild 删除 children 中第一个子节点。
//
// 从右兄弟借元素时，如果节点不是叶子节点，
// 右兄弟的第一个子节点也要移动到当前子节点的最后面。
// 这里使用 slices.Delete 保持和其它切片删除操作一致。
func removeFirstChild[K Key, V any](children []*node[K, V]) (*node[K, V], []*node[K, V]) {
	child := children[0]
	return child, slices.Delete(children, 0, 1)
}

// truncateChildren 把 children 截断到指定长度。
//
// 分裂内部节点时，左节点只保留前半部分子节点。
// 截断前会把被截掉的位置置为 nil，避免底层数组继续持有子树引用。
func truncateChildren[K Key, V any](children []*node[K, V], length int) []*node[K, V] {
	for i := length; i < len(children); i++ {
		children[i] = nil
	}
	return children[:length]
}

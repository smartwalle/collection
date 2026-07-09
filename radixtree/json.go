package radixtree

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// MarshalJSON 按 key 从小到大序列化为 JSON 对象。
//
// 这里不通过中间 map，而是直接按 Tree 的 Range 顺序写入 bytes.Buffer。
// 这样可以保留 Radix Tree 的字典序输出，避免普通 map 的遍历随机性影响结果。
//
// JSON 对象的 key 本来就是字符串，所以这里只需要用 strconv.AppendQuote 处理转义。
func (t *Tree[V]) MarshalJSON() ([]byte, error) {
	if t == nil {
		// nil Tree 与标准库中 nil 指针类型的 JSON 行为保持一致，输出 null。
		return []byte("null"), nil
	}

	var buf bytes.Buffer
	buf.WriteByte('{')
	var encoder = json.NewEncoder(&buf)

	var index int
	var rangeErr error
	var keyBytes []byte
	t.Range(func(key string, value V) bool {
		if index > 0 {
			// JSON 对象成员之间用逗号分隔，第一个成员前面不写逗号。
			buf.WriteByte(',')
		}

		keyBytes = strconv.AppendQuote(keyBytes[:0], key)
		buf.Write(keyBytes)
		buf.WriteByte(':')

		if err := encoder.Encode(value); err != nil {
			// value 的序列化仍交给 encoding/json，保持和普通结构体、切片等类型一致。
			rangeErr = err
			return false
		}
		// Encoder.Encode 会在每个 value 后追加换行。
		// 这里直接截掉最后一个换行，保持 JSON 对象是紧凑格式。
		buf.Truncate(buf.Len() - 1)

		index++
		return true
	})
	if rangeErr != nil {
		return nil, rangeErr
	}

	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// UnmarshalJSON 从 JSON 对象反序列化。
//
// JSON 对象字段顺序不会影响 Tree；反序列化后仍按 key 字典序组织。
// 如果 JSON 中出现重复 key，后出现的值会覆盖先出现的值，
// 这和 Put 的覆盖语义保持一致。
//
// 反序列化会先写入临时 Tree，全部成功后再替换当前 Tree。
// 因此如果 JSON 解析失败，当前 Tree 中已有的数据不会被清空或部分覆盖。
func (t *Tree[V]) UnmarshalJSON(b []byte) error {
	if t == nil {
		// nil 接收者无法写入数据。这里保持空操作，避免反序列化时 panic。
		return nil
	}
	if bytes.EqualFold(bytes.TrimSpace(b), []byte("null")) {
		// null 表示空 Tree。null 是合法输入，所以这里会清空当前 Tree。
		t.Clear()
		return nil
	}

	tmp := New[V]()
	decoder := json.NewDecoder(bytes.NewReader(b))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return fmt.Errorf("expected JSON object")
	}

	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return fmt.Errorf("expected JSON object key")
		}

		var value V
		if err = decoder.Decode(&value); err != nil {
			// value 仍交给 encoding/json 反序列化，这样 V 可以是任意 JSON 支持的类型。
			return err
		}
		tmp.Put(key, value)
	}

	token, err = decoder.Token()
	if err != nil {
		return err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '}' {
		return fmt.Errorf("expected end of JSON object")
	}

	t.root = tmp.root
	t.length = tmp.length
	return nil
}

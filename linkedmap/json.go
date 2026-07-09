package linkedmap

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
)

// MarshalJSON 按插入顺序序列化为 JSON 对象。
//
// 这里不通过中间 map，而是直接按 Range 顺序写入 bytes.Buffer。
// 如果先转成普通 map 再序列化，会丢失 linked map 维护的插入顺序。
//
// JSON 对象的 key 最终只能是字符串，所以会先把 K 转成字符串，
// 再用 strconv.AppendQuote 对 key 字符串做转义，避免手写字符串转义逻辑。
func (m *Map[K, V]) MarshalJSON() ([]byte, error) {
	if m == nil {
		// nil Map 与标准库中 nil 指针类型的 JSON 行为保持一致，输出 null。
		return []byte("null"), nil
	}

	var buf bytes.Buffer
	buf.WriteByte('{')
	var encoder = json.NewEncoder(&buf)

	var index int
	var rangeErr error
	var keyBytes []byte
	m.Range(func(key K, value V) bool {
		if index > 0 {
			// JSON 对象成员之间用逗号分隔，第一个成员前面不写逗号。
			buf.WriteByte(',')
		}

		keyText, err := formatJSONKey(key)
		if err != nil {
			// Range 的回调没有 error 返回值，所以把错误保存在外层变量里，
			// 再返回 false 停止遍历。
			rangeErr = err
			return false
		}
		// keyText 虽然已经是 string，但仍然不能手动拼接双引号。
		// strconv.AppendQuote 会复用标准库的字符串转义规则，
		// 避免遗漏控制字符、引号、反斜杠等边界。
		keyBytes = strconv.AppendQuote(keyBytes[:0], keyText)
		buf.Write(keyBytes)
		buf.WriteByte(':')

		if err = encoder.Encode(value); err != nil {
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
// JSON 对象字段的出现顺序会成为 Map 的插入顺序。
// 如果 JSON 中出现重复 key，后出现的值会覆盖先出现的值，但不会改变该 key
// 第一次出现时确定的插入位置，这和 Put 覆盖已有 key 时不移动元素的语义一致。
//
// 反序列化会先写入临时 Map，全部成功后再替换当前 Map。
// 因此如果 JSON 解析失败，当前 Map 中已有的数据不会被清空或部分覆盖。
func (m *Map[K, V]) UnmarshalJSON(b []byte) error {
	if m == nil {
		// nil 接收者无法写入数据。这里保持空操作，避免反序列化时 panic。
		return nil
	}
	if bytes.EqualFold(bytes.TrimSpace(b), []byte("null")) {
		// null 表示空 Map。null 是合法输入，所以这里会清空当前 Map。
		m.Clear()
		return nil
	}

	tmp := New[K, V]()
	var decoder = json.NewDecoder(bytes.NewReader(b))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return fmt.Errorf("expected JSON object")
	}

	for decoder.More() {
		// JSON 对象的 key 会由 decoder.Token 返回 string。
		// decoder 会负责识别对象边界，因此这里不需要手动扫描逗号和冒号。
		token, err = decoder.Token()
		if err != nil {
			return err
		}
		rawKey, ok := token.(string)
		if !ok {
			return fmt.Errorf("expected JSON object key")
		}
		var key K
		key, err = parseJSONKey[K](rawKey)
		if err != nil {
			return err
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
	m.table = tmp.table
	m.head = tmp.head
	m.tail = tmp.tail
	return nil
}

// formatJSONKey 把 key 转成 JSON 对象可以使用的字符串 key。
//
// JSON 对象的成员名只能是字符串，但 linked map 的 key 可以是任何 Key 类型。
// 因此这里只支持可以明确转成字符串且可以再解析回来的基础类型。
//
// 如果 K 是结构体、数组等复杂 Key 类型，会返回错误。
func formatJSONKey[K Key](key K) (string, error) {
	value := reflect.ValueOf(key)
	switch value.Kind() {
	case reflect.String:
		return value.String(), nil
	case reflect.Bool:
		return strconv.FormatBool(value.Bool()), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(value.Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.FormatUint(value.Uint(), 10), nil
	case reflect.Float32:
		return strconv.FormatFloat(value.Float(), 'g', -1, 32), nil
	case reflect.Float64:
		return strconv.FormatFloat(value.Float(), 'g', -1, 64), nil
	default:
		return "", fmt.Errorf("unsupported JSON key type %T", key)
	}
}

// parseJSONKey 把 JSON 对象的字符串 key 解析回 K。
//
// 这里使用 reflect.TypeOf 获取 K 的实际类型，再根据 kind 调用对应的 strconv 解析函数。
// 解析后再 Convert 回原始 key 类型，这样命名类型不会丢失。
func parseJSONKey[K Key](text string) (K, error) {
	var key K
	keyType := reflect.TypeOf(key)
	if keyType == nil {
		// 理论上 Key 约束下不会出现 nil 类型，这里保留防御性检查。
		return key, fmt.Errorf("unsupported nil key type")
	}

	var value reflect.Value
	switch keyType.Kind() {
	case reflect.String:
		value = reflect.ValueOf(text).Convert(keyType)
	case reflect.Bool:
		parsed, err := strconv.ParseBool(text)
		if err != nil {
			return key, err
		}
		value = reflect.ValueOf(parsed).Convert(keyType)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		parsed, err := strconv.ParseInt(text, 10, keyType.Bits())
		if err != nil {
			return key, err
		}
		value = reflect.ValueOf(parsed).Convert(keyType)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		parsed, err := strconv.ParseUint(text, 10, keyType.Bits())
		if err != nil {
			return key, err
		}
		value = reflect.ValueOf(parsed).Convert(keyType)
	case reflect.Float32, reflect.Float64:
		parsed, err := strconv.ParseFloat(text, keyType.Bits())
		if err != nil {
			return key, err
		}
		value = reflect.ValueOf(parsed).Convert(keyType)
	default:
		return key, fmt.Errorf("unsupported JSON key type %s", keyType.String())
	}
	return value.Interface().(K), nil
}

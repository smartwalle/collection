package treemap

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
)

// MarshalJSON 按 key 从小到大序列化为 JSON 对象。
//
// Go 的 encoding/json 在序列化普通 map 时会自行处理 key 顺序。
// 这里不通过中间 map，而是直接按 Map 的 Range 顺序写入 bytes.Buffer，
// 这样可以保证输出顺序与 treemap 的 key 顺序一致。
// 如果先转成普通 map 再序列化，JSON key 会按字符串形式排序，
// 可能和 int、float 等 key 的自然顺序不一致。
//
// JSON 对象的 key 最终只能是字符串，所以会先把 K 转成字符串，
// 再用 json.Marshal 对 key 字符串做转义，避免手写字符串转义逻辑。
func (m *Map[K, V]) MarshalJSON() ([]byte, error) {
	if m == nil {
		// nil Map 与标准库中 nil 指针类型的 JSON 行为保持一致，输出 null。
		return []byte("null"), nil
	}

	var buf bytes.Buffer
	buf.WriteByte('{')
	var encoder = json.NewEncoder(&buf)

	var index = 0
	var rangeErr error
	var keyBytes []byte
	m.Range(func(key K, value V) bool {
		if index > 0 {
			// JSON 对象成员之间用逗号分隔，第一个成员前面不写逗号。
			buf.WriteByte(',')
		}

		var keyText, err = formatJSONKey(key)
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
// JSON 对象本身的 key 顺序不会影响 Map；反序列化后仍按 key 排序。
// 如果 JSON 中出现重复 key，后出现的值会覆盖先出现的值，
// 这和 Put 的覆盖语义保持一致。
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

	var degree = m.degree
	if degree <= 1 {
		degree = kDefaultDegree
	}
	// 先写入临时 Map，只有整个 JSON 对象解析成功后才替换当前数据。
	// 这样可以避免半截 JSON 或某个 value 解码失败时污染已有 Map。
	var tmp = NewWithDegree[K, V](degree)

	var decoder = json.NewDecoder(bytes.NewReader(b))
	var token, err = decoder.Token()
	if err != nil {
		return err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return fmt.Errorf("expected JSON object")
	}

	for decoder.More() {
		// JSON 对象的 key 会由 decoder.Token 返回 string。
		// 再根据 K 的实际类型把字符串解析回对应 key 类型。
		// decoder 会负责识别对象边界，因此这里不需要手动扫描逗号和冒号。
		token, err = decoder.Token()
		if err != nil {
			return err
		}
		var rawKey, ok = token.(string)
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
		// 如果 JSON 对象里存在重复 key，后面的值覆盖前面的值。
		// 这和 Go 标准库反序列化到 map 时的行为一致，也和 Put 的语义一致。
		tmp.Put(key, value)
	}

	token, err = decoder.Token()
	if err != nil {
		return err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '}' {
		return fmt.Errorf("expected end of JSON object")
	}
	m.root = tmp.root
	m.length = tmp.length
	m.degree = tmp.degree
	return nil
}

// formatJSONKey 把 key 转成 JSON 对象可以使用的字符串 key。
//
// JSON 对象的成员名只能是字符串，但 Map 的 key 可以是任何 cmp.Ordered 类型。
// 因此这里需要把字符串、整数、无符号整数、浮点数分别转成字符串。
//
// 转换后还会在 MarshalJSON 中再次调用 json.Marshal，
// 以便正确处理引号、反斜杠等 JSON 字符串转义。
func formatJSONKey[K Key](key K) (string, error) {
	var value = reflect.ValueOf(key)
	switch value.Kind() {
	case reflect.String:
		return value.String(), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(value.Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.FormatUint(value.Uint(), 10), nil
	case reflect.Float32:
		// float32 使用 32 位精度格式化，避免把原本 float32 的值扩展成不必要的长文本。
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
// 解析后再 Convert 回原始 key 类型，这样 int8、uint16、float32 这类具体类型不会丢失。
//
// 如果 K 是不支持作为 JSON 对象 key 的类型，会返回错误。
func parseJSONKey[K Key](text string) (K, error) {
	var key K
	var keyType = reflect.TypeOf(key)
	if keyType == nil {
		// 理论上 Key 约束下不会出现 nil 类型，这里保留防御性检查。
		return key, fmt.Errorf("unsupported nil key type")
	}

	var value reflect.Value
	switch keyType.Kind() {
	case reflect.String:
		value = reflect.ValueOf(text).Convert(keyType)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		// Bits 使用具体整数类型的位宽，例如 int8 是 8、int64 是 64。
		// 这样溢出时 ParseInt 会直接返回错误，而不是静默截断。
		var parsed, err = strconv.ParseInt(text, 10, keyType.Bits())
		if err != nil {
			return key, err
		}
		value = reflect.ValueOf(parsed).Convert(keyType)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		// 无符号类型同样按真实位宽解析，负数字符串会被标准库判定为错误。
		var parsed, err = strconv.ParseUint(text, 10, keyType.Bits())
		if err != nil {
			return key, err
		}
		value = reflect.ValueOf(parsed).Convert(keyType)
	case reflect.Float32, reflect.Float64:
		// ParseFloat 根据 keyType.Bits() 决定精度。
		// float32 会按 32 位舍入，最后再 Convert 回 K。
		var parsed, err = strconv.ParseFloat(text, keyType.Bits())
		if err != nil {
			return key, err
		}
		value = reflect.ValueOf(parsed).Convert(keyType)
	default:
		return key, fmt.Errorf("unsupported JSON key type %s", keyType.String())
	}
	return value.Interface().(K), nil
}

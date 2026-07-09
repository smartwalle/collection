package sortedset

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
)

// MarshalJSON 按 score/member 从小到大序列化为 JSON 对象。
//
// JSON 对象的成员名只能是字符串，因此会先把 member 转成字符串 key，
// 再用 strconv.AppendQuote 复用标准库的字符串转义规则。
//
// 输出格式为：
//
//	{
//	  "member": score
//	}
//
// value 固定为 score，而不是业务 value。Set 本身只保存 member 和 score，
// 这里的 JSON 语义对应 Redis sorted set 常见的 member-score 表达。
//
// 这里不通过中间 map，而是直接按 Range 顺序写入 bytes.Buffer，
// 这样可以保留 sorted set 当前的 score/member 排序结果。
// 如果先转成普通 map 再序列化，encoding/json 会按对象 key 排序，
// 输出顺序就不再体现 sorted set 的 score/member 顺序。
//
// NaN 不支持作为 score。MarshalJSON 不会提前检查，score 的编码交给
// encoding/json；如果 score 是 NaN，标准库会返回错误。
func (s *Set[M]) MarshalJSON() ([]byte, error) {
	if s == nil {
		// nil Set 与标准库中 nil 指针类型的 JSON 行为保持一致，输出 null。
		return []byte("null"), nil
	}

	var buf bytes.Buffer
	buf.WriteByte('{')
	var encoder = json.NewEncoder(&buf)

	var index int
	var rangeErr error
	var keyBytes []byte
	s.Range(func(member M, score float64) bool {
		if index > 0 {
			// JSON 对象成员之间用逗号分隔，第一个成员前面不写逗号。
			buf.WriteByte(',')
		}

		var keyText, err = formatJSONMember(member)
		if err != nil {
			// Range 的回调没有 error 返回值，所以把错误保存在外层变量里，
			// 再返回 false 停止遍历。
			rangeErr = err
			return false
		}
		keyBytes = strconv.AppendQuote(keyBytes[:0], keyText)
		buf.Write(keyBytes)
		buf.WriteByte(':')

		if err = encoder.Encode(score); err != nil {
			rangeErr = err
			return false
		}
		// Encoder.Encode 会在每个 score 后追加换行。
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
// JSON 对象的 key 会被解析成 member，value 会被解析成 score。
// JSON 对象字段的出现顺序不会影响 Set；反序列化后仍按 score/member 排序。
// 这和 linkedmap 不同：sorted set 的顺序由 score/member 决定，而不是 JSON 字段顺序。
//
// 如果 JSON 中出现重复 member，后出现的 score 会覆盖先出现的 score，
// 这和 Put 的覆盖语义保持一致。
//
// 反序列化会先写入临时 Set，全部成功后再替换当前 Set。
// 因此如果 JSON 解析失败，当前 Set 中已有的数据不会被清空或部分覆盖。
func (s *Set[M]) UnmarshalJSON(b []byte) error {
	if s == nil {
		// nil 接收者无法写入数据。这里保持空操作，避免反序列化时 panic。
		return nil
	}
	if bytes.EqualFold(bytes.TrimSpace(b), []byte("null")) {
		// null 表示空 Set。null 是合法输入，所以这里会清空当前 Set。
		s.Clear()
		return nil
	}

	var tmp = New[M]()
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
		// 再根据 M 的实际类型把字符串解析回对应 member 类型。
		token, err = decoder.Token()
		if err != nil {
			return err
		}
		var rawMember, ok = token.(string)
		if !ok {
			return fmt.Errorf("expected JSON object key")
		}
		var member M
		member, err = parseJSONMember[M](rawMember)
		if err != nil {
			return err
		}
		var score float64
		if err = decoder.Decode(&score); err != nil {
			return err
		}
		tmp.Put(member, score)
	}

	token, err = decoder.Token()
	if err != nil {
		return err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '}' {
		return fmt.Errorf("expected end of JSON object")
	}
	s.table = tmp.table
	s.header = tmp.header
	s.tail = tmp.tail
	s.level = tmp.level
	s.length = tmp.length
	s.seed = tmp.seed
	return nil
}

// formatJSONMember 把 member 转成 JSON 对象可以使用的字符串 key。
//
// JSON 对象的成员名只能是字符串，但 Set 的 member 可以是任何 Member 类型。
// 因此这里需要把字符串、整数、无符号整数、浮点数分别转成字符串。
//
// 这里不支持结构体等复杂类型。虽然某些复杂类型可以定义自己的编码方式，
// 但 Member 约束本身只允许自然有序类型，保持基础类型转换更清晰。
func formatJSONMember[M Member](member M) (string, error) {
	var value = reflect.ValueOf(member)
	switch value.Kind() {
	case reflect.String:
		return value.String(), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(value.Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.FormatUint(value.Uint(), 10), nil
	case reflect.Float32:
		return strconv.FormatFloat(value.Float(), 'g', -1, 32), nil
	case reflect.Float64:
		return strconv.FormatFloat(value.Float(), 'g', -1, 64), nil
	default:
		return "", fmt.Errorf("unsupported JSON member type %T", member)
	}
}

// parseJSONMember 把 JSON 对象的字符串 key 解析回 M。
//
// 这里使用 reflect.TypeOf 获取 M 的实际类型，再根据 kind 调用对应的 strconv 解析函数。
// 解析后再 Convert 回原始 member 类型，这样命名类型不会丢失。
//
// 解析过程按目标类型的位宽处理整数和浮点数，溢出或非法格式会由 strconv 返回错误。
func parseJSONMember[M Member](text string) (M, error) {
	var member M
	var memberType = reflect.TypeOf(member)
	if memberType == nil {
		// 理论上 Member 约束下不会出现 nil 类型，这里保留防御性检查。
		return member, fmt.Errorf("unsupported nil member type")
	}

	var value reflect.Value
	switch memberType.Kind() {
	case reflect.String:
		value = reflect.ValueOf(text).Convert(memberType)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		var parsed, err = strconv.ParseInt(text, 10, memberType.Bits())
		if err != nil {
			return member, err
		}
		value = reflect.ValueOf(parsed).Convert(memberType)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		var parsed, err = strconv.ParseUint(text, 10, memberType.Bits())
		if err != nil {
			return member, err
		}
		value = reflect.ValueOf(parsed).Convert(memberType)
	case reflect.Float32, reflect.Float64:
		var parsed, err = strconv.ParseFloat(text, memberType.Bits())
		if err != nil {
			return member, err
		}
		value = reflect.ValueOf(parsed).Convert(memberType)
	default:
		return member, fmt.Errorf("unsupported JSON member type %s", memberType.String())
	}
	return value.Interface().(M), nil
}

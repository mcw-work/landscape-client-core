package bpickle

import (
	"bytes"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// Marshal encodes v into bpickle wire format.
// Supported types: bool, int (all widths), float64, []byte, string, []any, map[string]any, nil.
// Returns an error for unsupported types.
func Marshal(v any) ([]byte, error) {
	return marshalValue(v)
}

// Unmarshal decodes a bpickle-encoded byte slice.
// Returns map[string]any for dicts, []any for lists, string for unicode,
// []byte for bytes, int64 for integers, float64 for floats, bool, nil.
func Unmarshal(data []byte) (any, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("bpickle: cannot unmarshal empty data")
	}
	val, _, err := unmarshalValue(data, 0)
	return val, err
}

// marshalValue dispatches encoding based on the concrete type of v.
func marshalValue(v any) ([]byte, error) {
	if v == nil {
		return []byte{'n'}, nil
	}
	switch val := v.(type) {
	case bool:
		if val {
			return []byte{'b', '1'}, nil
		}
		return []byte{'b', '0'}, nil
	case int:
		return marshalInt(int64(val))
	case int8:
		return marshalInt(int64(val))
	case int16:
		return marshalInt(int64(val))
	case int32:
		return marshalInt(int64(val))
	case int64:
		return marshalInt(val)
	case uint:
		return marshalInt(int64(val))
	case uint8:
		return marshalInt(int64(val))
	case uint16:
		return marshalInt(int64(val))
	case uint32:
		return marshalInt(int64(val))
	case uint64:
		if val > math.MaxInt64 {
			return nil, fmt.Errorf("bpickle: uint64 value %d overflows int64", val)
		}
		return marshalInt(int64(val))
	case float32:
		return marshalFloat64(float64(val))
	case float64:
		return marshalFloat64(val)
	case []byte:
		return marshalBytes(val)
	case string:
		return marshalString(val)
	case Tuple:
		return marshalTuple(val)
	case []any:
		return marshalList(val)
	case map[string]any:
		return marshalDict(val)
	case BytesDict:
		return marshalBytesDict(val)
	default:
		// Fallback: use reflection to handle concrete slice/array and
		// string-keyed map types (e.g. []map[string]any, map[string][]string).
		rv := reflect.ValueOf(v)
		switch rv.Kind() {
		case reflect.Slice, reflect.Array:
			list := make([]any, rv.Len())
			for i := range list {
				list[i] = rv.Index(i).Interface()
			}
			return marshalList(list)
		case reflect.Map:
			if rv.Type().Key().Kind() != reflect.String {
				return nil, fmt.Errorf("bpickle: unsupported type %T", v)
			}
			dict := make(map[string]any, rv.Len())
			for _, k := range rv.MapKeys() {
				dict[k.String()] = rv.MapIndex(k).Interface()
			}
			return marshalDict(dict)
		default:
			return nil, fmt.Errorf("bpickle: unsupported type %T", v)
		}
	}
}

// marshalInt encodes an int64 as i<decimal>; .
func marshalInt(v int64) ([]byte, error) {
	return []byte(fmt.Sprintf("i%d;", v)), nil
}

// marshalFloat64 encodes a float64 as f<repr>; matching Python's repr() output.
// Whole-number floats (no '.' or exponent) get a ".0" suffix for wire compatibility.
func marshalFloat64(v float64) ([]byte, error) {
	s := strconv.FormatFloat(v, 'g', -1, 64)
	if !strings.Contains(s, ".") && !strings.ContainsAny(s, "eE") {
		s += ".0"
	}
	return []byte("f" + s + ";"), nil
}

// marshalBytes encodes []byte as s<length>:<raw bytes> .
func marshalBytes(v []byte) ([]byte, error) {
	prefix := []byte(fmt.Sprintf("s%d:", len(v)))
	return append(prefix, v...), nil
}

// marshalString encodes a string as u<byte_length>:<utf8 bytes> .
func marshalString(v string) ([]byte, error) {
	b := []byte(v)
	prefix := []byte(fmt.Sprintf("u%d:", len(b)))
	return append(prefix, b...), nil
}

// Tuple is a fixed-length ordered sequence that marshals as a bpickle tuple
// (t...;) rather than a list (l...;). The Landscape server message schemas
// use Tuple for data-point records such as (timestamp, value).
type Tuple []any

// marshalList encodes []any as l<items>; .
func marshalList(v []any) ([]byte, error) {
	result := []byte{'l'}
	for _, item := range v {
		encoded, err := marshalValue(item)
		if err != nil {
			return nil, err
		}
		result = append(result, encoded...)
	}
	return append(result, ';'), nil
}

// marshalTuple encodes a Tuple as t<items>; .
func marshalTuple(v Tuple) ([]byte, error) {
	result := []byte{'t'}
	for _, item := range v {
		encoded, err := marshalValue(item)
		if err != nil {
			return nil, err
		}
		result = append(result, encoded...)
	}
	return append(result, ';'), nil
}

// BytesDict is a map whose keys are encoded as bpickle bytes (Python bytes)
// rather than unicode strings. Use for messages where the schema specifies
// Bytes() keys, such as network-activity's activities dict.
type BytesDict map[string]any

// marshalBytesKeyMap encodes map[[]byte][]any as d<key><val>...; with keys as bpickle bytes.
// Used for network-activity where interface names must be Python bytes, not str.
func marshalBytesKeyMap(v BytesDict) ([]byte, error) {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	result := []byte{'d'}
	for _, k := range keys {
		encodedKey, err := marshalBytes([]byte(k))
		if err != nil {
			return nil, err
		}
		encodedVal, err := marshalValue(v[k])
		if err != nil {
			return nil, err
		}
		result = append(result, encodedKey...)
		result = append(result, encodedVal...)
	}
	return append(result, ';'), nil
}

// marshalBytesDict is an alias so the case statement can reference it cleanly.
func marshalBytesDict(v BytesDict) ([]byte, error) { return marshalBytesKeyMap(v) }

// marshalDict encodes map[string]any as d<key><val>...; with keys sorted.
func marshalDict(v map[string]any) ([]byte, error) {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	result := []byte{'d'}
	for _, k := range keys {
		encodedKey, err := marshalString(k)
		if err != nil {
			return nil, err
		}
		encodedVal, err := marshalValue(v[k])
		if err != nil {
			return nil, err
		}
		result = append(result, encodedKey...)
		result = append(result, encodedVal...)
	}
	return append(result, ';'), nil
}

// unmarshalValue decodes one value starting at pos, returning (value, newPos, error).
func unmarshalValue(data []byte, pos int) (any, int, error) {
	if pos >= len(data) {
		return nil, pos, fmt.Errorf("bpickle: unexpected end of data at position %d", pos)
	}
	switch data[pos] {
	case 'n':
		return nil, pos + 1, nil
	case 'b':
		return unmarshalBool(data, pos)
	case 'i':
		return unmarshalInt(data, pos)
	case 'f':
		return unmarshalFloat(data, pos)
	case 's':
		return unmarshalBytes(data, pos)
	case 'u':
		return unmarshalUnicode(data, pos)
	case 'l':
		return unmarshalList(data, pos)
	case 't':
		return unmarshalTuple(data, pos)
	case 'd':
		return unmarshalDict(data, pos)
	default:
		return nil, pos, fmt.Errorf("bpickle: unknown type marker %q at position %d", data[pos], pos)
	}
}

func unmarshalBool(data []byte, pos int) (any, int, error) {
	if pos+2 > len(data) {
		return nil, pos, fmt.Errorf("bpickle: truncated bool at position %d", pos)
	}
	b := data[pos+1]
	if b != '0' && b != '1' {
		return nil, pos, fmt.Errorf("bpickle: invalid bool byte %q at position %d", b, pos+1)
	}
	return b == '1', pos + 2, nil
}

func unmarshalInt(data []byte, pos int) (any, int, error) {
	rel := bytes.IndexByte(data[pos:], ';')
	if rel == -1 {
		return nil, pos, fmt.Errorf("bpickle: unterminated int at position %d", pos)
	}
	end := pos + rel
	v, err := strconv.ParseInt(string(data[pos+1:end]), 10, 64)
	if err != nil {
		return nil, pos, fmt.Errorf("bpickle: invalid int: %w", err)
	}
	return v, end + 1, nil
}

func unmarshalFloat(data []byte, pos int) (any, int, error) {
	rel := bytes.IndexByte(data[pos:], ';')
	if rel == -1 {
		return nil, pos, fmt.Errorf("bpickle: unterminated float at position %d", pos)
	}
	end := pos + rel
	v, err := strconv.ParseFloat(string(data[pos+1:end]), 64)
	if err != nil {
		return nil, pos, fmt.Errorf("bpickle: invalid float: %w", err)
	}
	return v, end + 1, nil
}

// unmarshalLengthPrefixed parses the length-prefixed body used by 's' and 'u'.
// Format: <marker><decimal>:<body>
// Returns the raw body bytes and the position after the body.
func unmarshalLengthPrefixed(data []byte, pos int) ([]byte, int, error) {
	// Find the ':' that terminates the length field.
	rel := bytes.IndexByte(data[pos+1:], ':')
	if rel == -1 {
		return nil, pos, fmt.Errorf("bpickle: missing ':' delimiter at position %d", pos)
	}
	colon := pos + 1 + rel
	length, err := strconv.Atoi(string(data[pos+1 : colon]))
	if err != nil {
		return nil, pos, fmt.Errorf("bpickle: invalid length at position %d: %w", pos, err)
	}
	if length < 0 {
		return nil, pos, fmt.Errorf("bpickle: negative length %d at position %d", length, pos)
	}
	start := colon + 1
	if length > len(data)-start {
		return nil, pos, fmt.Errorf("bpickle: data truncated at position %d (need %d bytes, have %d)", pos, length, len(data)-start)
	}
	end := start + length
	return data[start:end], end, nil
}

func unmarshalBytes(data []byte, pos int) (any, int, error) {
	raw, newPos, err := unmarshalLengthPrefixed(data, pos)
	if err != nil {
		return nil, pos, err
	}
	result := make([]byte, len(raw))
	copy(result, raw)
	return result, newPos, nil
}

func unmarshalUnicode(data []byte, pos int) (any, int, error) {
	raw, newPos, err := unmarshalLengthPrefixed(data, pos)
	if err != nil {
		return nil, pos, err
	}
	return string(raw), newPos, nil
}

func unmarshalList(data []byte, pos int) (any, int, error) {
	pos++ // consume 'l'
	result := make([]any, 0)
	for {
		if pos >= len(data) {
			return nil, pos, fmt.Errorf("bpickle: unterminated list")
		}
		if data[pos] == ';' {
			return result, pos + 1, nil
		}
		val, newPos, err := unmarshalValue(data, pos)
		if err != nil {
			return nil, pos, err
		}
		pos = newPos
		result = append(result, val)
	}
}

// unmarshalTuple decodes a Python tuple ('t') as []any since Go has no tuples.
func unmarshalTuple(data []byte, pos int) (any, int, error) {
	pos++ // consume 't'
	result := make([]any, 0)
	for {
		if pos >= len(data) {
			return nil, pos, fmt.Errorf("bpickle: unterminated tuple")
		}
		if data[pos] == ';' {
			return result, pos + 1, nil
		}
		val, newPos, err := unmarshalValue(data, pos)
		if err != nil {
			return nil, pos, err
		}
		pos = newPos
		result = append(result, val)
	}
}

func unmarshalDict(data []byte, pos int) (any, int, error) {
	pos++ // consume 'd'
	result := make(map[string]any)
	for {
		if pos >= len(data) {
			return nil, pos, fmt.Errorf("bpickle: unterminated dict")
		}
		if data[pos] == ';' {
			return result, pos + 1, nil
		}

		// Keys may be 's' (bytes) or 'u' (unicode); both become Go strings.
		keyAny, newPos, err := unmarshalValue(data, pos)
		if err != nil {
			return nil, pos, err
		}
		pos = newPos

		var key string
		switch k := keyAny.(type) {
		case string:
			key = k
		case []byte:
			key = string(k)
		default:
			return nil, pos, fmt.Errorf("bpickle: dict key must be string or bytes, got %T", keyAny)
		}

		val, newPos, err := unmarshalValue(data, pos)
		if err != nil {
			return nil, pos, err
		}
		pos = newPos
		result[key] = val
	}
}

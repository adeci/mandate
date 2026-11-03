package record

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
)

const maxInt = 1<<53 - 1

// canonical encodes v in the RFC 8785 form records use: objects, arrays,
// strings and integers, keys sorted, no whitespace, no null. encoding/json
// alone does not do: it escapes <, >, & and U+2028/U+2029.
func canonical(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var x any
	if err := d.Decode(&x); err != nil {
		return nil, err
	}
	return appendCanonical(nil, x)
}

func appendCanonical(b []byte, v any) ([]byte, error) {
	var err error
	switch v := v.(type) {
	case string:
		return appendString(b, v), nil
	case json.Number:
		n, perr := strconv.ParseInt(string(v), 10, 64)
		if perr != nil || n > maxInt || n < -maxInt {
			return nil, fmt.Errorf("%s is not an integer of at most 2^53-1 in magnitude", v)
		}
		return strconv.AppendInt(b, n, 10), nil
	case []any:
		b = append(b, '[')
		for i, e := range v {
			if i > 0 {
				b = append(b, ',')
			}
			if b, err = appendCanonical(b, e); err != nil {
				return nil, err
			}
		}
		return append(b, ']'), nil
	case map[string]any:
		b = append(b, '{')
		// Byte order equals JCS's UTF-16 order only for ASCII keys.
		for i, k := range slices.Sorted(maps.Keys(v)) {
			for j := range len(k) {
				if k[j] >= 0x80 {
					return nil, fmt.Errorf("key %q is not ASCII", k)
				}
			}
			if i > 0 {
				b = append(b, ',')
			}
			b = append(appendString(b, k), ':')
			if b, err = appendCanonical(b, v[k]); err != nil {
				return nil, err
			}
		}
		return append(b, '}'), nil
	case nil:
		return nil, errors.New("null is not allowed")
	default:
		return nil, fmt.Errorf("%T is not allowed", v)
	}
}

func appendString(b []byte, s string) []byte {
	const hex = "0123456789abcdef"
	b = append(b, '"')
	for i := range len(s) {
		c := s[i]
		switch c {
		case '"', '\\':
			b = append(b, '\\', c)
		case '\b':
			b = append(b, `\b`...)
		case '\f':
			b = append(b, `\f`...)
		case '\n':
			b = append(b, `\n`...)
		case '\r':
			b = append(b, `\r`...)
		case '\t':
			b = append(b, `\t`...)
		default:
			if c < 0x20 {
				b = append(b, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
			} else {
				b = append(b, c)
			}
		}
	}
	return append(b, '"')
}

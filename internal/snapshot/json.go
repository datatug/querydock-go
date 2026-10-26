// Package snapshot validates the portable QueryDock contract. It never interprets SQL.
package snapshot

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

var ErrInvalid = errors.New("invalid snapshot")

// ParseJSON rejects invalid UTF-8, lone surrogates, duplicate decoded keys,
// non-finite numbers, trailing input and nesting deeper than the browser's 64.
func ParseJSON(source []byte) (any, error) {
	if !utf8.Valid(source) || !validSurrogates(source) {
		return nil, ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(source))
	d.UseNumber()
	value, err := decode(d, 0)
	if err != nil {
		return nil, ErrInvalid
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, ErrInvalid
	}
	return value, nil
}

// encoding/json replaces lone escaped surrogates; reject them before decoding.
func validSurrogates(source []byte) bool {
	for i := 0; i < len(source); i++ {
		if source[i] != '\\' {
			continue
		}
		i++
		if i >= len(source) || source[i] != 'u' {
			continue
		}
		if i+4 >= len(source) {
			return false
		}
		n, err := strconv.ParseUint(string(source[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if n >= 0xdc00 && n <= 0xdfff {
			return false
		}
		if n < 0xd800 || n > 0xdbff {
			continue
		}
		if i+6 >= len(source) || source[i+1] != '\\' || source[i+2] != 'u' {
			return false
		}
		low, err := strconv.ParseUint(string(source[i+3:i+7]), 16, 16)
		if err != nil || low < 0xdc00 || low > 0xdfff {
			return false
		}
		i += 6
	}
	return true
}

func decode(d *json.Decoder, depth int) (any, error) {
	if depth > 64 {
		return nil, ErrInvalid
	}
	token, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch value := token.(type) {
	case json.Number:
		number, err := strconv.ParseFloat(string(value), 64)
		if err != nil || math.IsInf(number, 0) {
			return nil, ErrInvalid
		}
		return number, nil
	case json.Delim:
		if value == '{' {
			object := make(map[string]any)
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return nil, err
				}
				name := key.(string) // Decoder enforces string keys in object context.
				if _, exists := object[name]; exists {
					return nil, ErrInvalid
				}
				child, err := decode(d, depth+1)
				if err != nil {
					return nil, err
				}
				object[name] = child
			}
			_, err := d.Token()
			return object, err
		}
		array := make([]any, 0)
		for d.More() {
			child, err := decode(d, depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, child)
		}
		_, err := d.Token()
		return array, err
	default:
		return token, nil
	}
}

// Canonicalize implements RFC 8785 over strict JSON, including UTF-16 key order
// and ECMAScript's shortest binary64 representation and exponent cutovers.
func Canonicalize(source []byte) (string, error) {
	value, err := ParseJSON(source)
	if err != nil {
		return "", err
	}
	return canonical(value), nil
}

func canonical(value any) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case bool:
		return strconv.FormatBool(v)
	case string:
		return quote(v)
	case float64:
		return number(v)
	case []any:
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = canonical(item)
		}
		return "[" + strings.Join(parts, ",") + "]"
	default:
		object := v.(map[string]any)
		keys := make([]string, 0, len(object))
		for key := range object {
			keys = append(keys, key)
		}
		slices.SortFunc(keys, func(a, b string) int { return slices.Compare(utf16.Encode([]rune(a)), utf16.Encode([]rune(b))) })
		parts := make([]string, len(keys))
		for i, key := range keys {
			parts[i] = quote(key) + ":" + canonical(object[key])
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
}

func quote(value string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 {
				b.WriteString(`\u00`)
				b.WriteByte("0123456789abcdef"[r>>4])
				b.WriteByte("0123456789abcdef"[r&15])
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func number(value float64) string {
	if value == 0 {
		return "0"
	}
	text := strconv.FormatFloat(value, 'e', -1, 64)
	parts := strings.Split(text, "e")
	exponent, _ := strconv.Atoi(parts[1]) // FormatFloat always produces a decimal exponent.
	if exponent >= -6 && exponent < 21 {
		return strconv.FormatFloat(value, 'f', -1, 64)
	}
	sign := ""
	if exponent >= 0 {
		sign = "+"
	}
	return parts[0] + "e" + sign + strconv.Itoa(exponent)
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

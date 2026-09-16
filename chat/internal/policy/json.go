// Package policy is the Go port of Warden's SBX policy broker: the control
// registry, per-sandbox policy engines, credential sources, sharing services
// and the inspected loopback gateways that used to run as Python and mitmproxy.
package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"unicode/utf8"
)

// Dumps renders a value the way the Python broker did: sorted keys, compact
// separators, ASCII-only escapes and no NaN/Infinity. Directory names and
// audit hashes are derived from this form, so it must stay byte-compatible.
func Dumps(value any) string {
	var buf bytes.Buffer
	if err := writeCanonical(&buf, value); err != nil {
		panic(err)
	}
	return buf.String()
}

// DumpsErr is Dumps for callers that must not panic on unsupported values.
func DumpsErr(value any) (string, error) {
	var buf bytes.Buffer
	if err := writeCanonical(&buf, value); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func writeCanonical(buf *bytes.Buffer, value any) error {
	switch v := value.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if v {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		writeString(buf, v)
	case json.Number:
		if _, err := strconv.ParseFloat(string(v), 64); err != nil {
			return errors.New("invalid JSON number")
		}
		buf.WriteString(string(v))
	case int:
		buf.WriteString(strconv.Itoa(v))
	case int64:
		buf.WriteString(strconv.FormatInt(v, 10))
	case int32:
		buf.WriteString(strconv.FormatInt(int64(v), 10))
	case uint:
		buf.WriteString(strconv.FormatUint(uint64(v), 10))
	case uint64:
		buf.WriteString(strconv.FormatUint(v, 10))
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return errors.New("nonfinite JSON")
		}
		buf.WriteString(formatFloat(v))
	case float32:
		return writeCanonical(buf, float64(v))
	case []byte:
		writeString(buf, string(v))
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeString(buf, k)
			buf.WriteByte(':')
			if err := writeCanonical(buf, v[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case map[string]string:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeString(buf, k)
			buf.WriteByte(':')
			writeString(buf, v[k])
		}
		buf.WriteByte('}')
	case []any:
		buf.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case []string:
		buf.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeString(buf, item)
		}
		buf.WriteByte(']')
	case [][]string:
		buf.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	default:
		// Structs and other typed values go through encoding/json first so
		// their field tags apply, then are canonicalized like decoded data.
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		generic, err := StrictJSON(raw)
		if err != nil {
			return err
		}
		return writeCanonical(buf, generic)
	}
	return nil
}

func formatFloat(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e16 {
		return strconv.FormatFloat(v, 'f', 1, 64)
	}
	s := strconv.FormatFloat(v, 'g', -1, 64)
	if i := bytes.IndexByte([]byte(s), 'e'); i >= 0 && s[i+1] != '-' && s[i+1] != '+' {
		s = s[:i+1] + "+" + s[i+1:]
	}
	return s
}

const hexDigits = "0123456789abcdef"

func writeString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch c {
			case '"':
				buf.WriteString(`\"`)
			case '\\':
				buf.WriteString(`\\`)
			case '\n':
				buf.WriteString(`\n`)
			case '\r':
				buf.WriteString(`\r`)
			case '\t':
				buf.WriteString(`\t`)
			case '\b':
				buf.WriteString(`\b`)
			case '\f':
				buf.WriteString(`\f`)
			default:
				if c < 0x20 || c == 0x7f {
					buf.WriteString(`\u00`)
					buf.WriteByte(hexDigits[c>>4])
					buf.WriteByte(hexDigits[c&0xf])
				} else {
					buf.WriteByte(c)
				}
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			r = 0xfffd
		}
		if r >= 0x10000 {
			r -= 0x10000
			writeU16(buf, 0xd800+(r>>10))
			writeU16(buf, 0xdc00+(r&0x3ff))
		} else {
			writeU16(buf, r)
		}
		i += size
	}
	buf.WriteByte('"')
}

func writeU16(buf *bytes.Buffer, r rune) {
	buf.WriteString(`\u`)
	buf.WriteByte(hexDigits[(r>>12)&0xf])
	buf.WriteByte(hexDigits[(r>>8)&0xf])
	buf.WriteByte(hexDigits[(r>>4)&0xf])
	buf.WriteByte(hexDigits[r&0xf])
}

// StrictJSON parses one JSON document into generic values, rejecting
// duplicate object keys, nonfinite numbers and trailing data. Numbers are
// kept as json.Number so integers survive a round trip unchanged.
func StrictJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	value, err := strictValue(dec, 0)
	if err != nil {
		return nil, err
	}
	if _, err = dec.Token(); err != io.EOF {
		return nil, errors.New("trailing JSON data")
	}
	return value, nil
}

func strictValue(dec *json.Decoder, depth int) (any, error) {
	if depth > 256 {
		return nil, errors.New("JSON nesting too deep")
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := map[string]any{}
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyTok.(string)
				if !ok {
					return nil, errors.New("invalid JSON key")
				}
				if _, dup := obj[key]; dup {
					return nil, errors.New("duplicate JSON key")
				}
				val, err := strictValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				obj[key] = val
			}
			if _, err = dec.Token(); err != nil {
				return nil, err
			}
			return obj, nil
		case '[':
			list := []any{}
			for dec.More() {
				val, err := strictValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				list = append(list, val)
			}
			if _, err = dec.Token(); err != nil {
				return nil, err
			}
			return list, nil
		}
		return nil, errors.New("unexpected JSON delimiter")
	case json.Number:
		if _, err := strconv.ParseFloat(string(t), 64); err != nil {
			return nil, errors.New("invalid JSON number")
		}
		return t, nil
	default:
		return tok, nil
	}
}

// Helpers for reading generic JSON values with Python-like tolerance.

func asMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func asList(v any) ([]any, bool) {
	l, ok := v.([]any)
	return l, ok
}

func asString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

// asInt accepts exact integers only, mirroring `type(v) is int` checks.
func asInt(v any) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		if i, err := strconv.ParseInt(string(n), 10, 64); err == nil {
			return i, true
		}
		return 0, false
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		if n == math.Trunc(n) && !math.IsInf(n, 0) && math.Abs(n) < 1<<53 {
			return int64(n), true
		}
		return 0, false
	}
	return 0, false
}

// asNumber accepts ints and floats, mirroring `type(v) in (int, float)`.
func asNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := strconv.ParseFloat(string(n), 64)
		return f, err == nil
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

func keysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sameKeys(m map[string]any, expected ...string) bool {
	if len(m) != len(expected) {
		return false
	}
	for _, k := range expected {
		if _, ok := m[k]; !ok {
			return false
		}
	}
	return true
}

// jsonEqual compares two generic JSON values by canonical form.
func jsonEqual(a, b any) bool {
	x, err1 := DumpsErr(a)
	y, err2 := DumpsErr(b)
	return err1 == nil && err2 == nil && x == y
}

func cloneJSON(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	out, err := StrictJSON(raw)
	if err != nil {
		return nil
	}
	return out
}

func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("json: %v", err))
	}
	return raw
}

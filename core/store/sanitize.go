package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"
)

// Postgres rejects NUL in text (SQLSTATE 22021) and \u0000 in jsonb (22P05);
// both are replaced with U+FFFD, as is invalid UTF-8.
const replacement = "�"

// CleanText makes s storable in a Postgres text column.
func CleanText(s string) string {
	if utf8.ValidString(s) && !strings.ContainsRune(s, 0) {
		return s
	}
	return strings.ReplaceAll(strings.ToValidUTF8(s, replacement), "\x00", replacement)
}

// CleanValue sanitizes every string in a JSON-like value (maps, slices, strings).
func CleanValue(v any) any {
	switch x := v.(type) {
	case string:
		return CleanText(x)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[CleanText(k)] = CleanValue(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = CleanValue(val)
		}
		return out
	default:
		return v
	}
}

// CleanJSON makes a JSON document storable in a Postgres jsonb column.
func CleanJSON(b []byte) []byte {
	if utf8.Valid(b) && !bytes.Contains(b, []byte(`\u0000`)) {
		return b
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		// Not JSON: store it as a JSON string.
		out, _ := json.Marshal(CleanText(string(b)))
		return out
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(CleanValue(v))
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// Clean sanitizes every text field of the message in place.
func (m *Message) Clean() {
	m.ID = CleanText(m.ID)
	m.Content = CleanText(m.Content)
	m.ToolCallID = CleanText(m.ToolCallID)
	m.Error = CleanText(m.Error)
	for i := range m.ToolCalls {
		tc := &m.ToolCalls[i]
		tc.ID = CleanText(tc.ID)
		tc.Function.Name = CleanText(tc.Function.Name)
		tc.Function.Arguments = CleanText(tc.Function.Arguments)
	}
	if m.Metadata != nil {
		m.Metadata, _ = CleanValue(m.Metadata).(map[string]any)
	}
}

// ErrInvalidData marks errors that will never succeed on retry (bad data, constraint).
var ErrInvalidData = errors.New("invalid data")

// dataErrorClassifiers are the backends' (Postgres...) extra permanent-error tests.
var dataErrorClassifiers []func(error) bool

// RegisterDataError adds a backend test for permanent errors (called from init).
func RegisterDataError(f func(error) bool) { dataErrorClassifiers = append(dataErrorClassifiers, f) }

// IsDataError reports a permanent error: ErrInvalidData or one a registered
// backend classifies as such (Postgres: SQLSTATE class 22 data exception or 23
// integrity constraint). Anything else (connection, timeout, server shutdown)
// is transient.
func IsDataError(err error) bool {
	if errors.Is(err, ErrInvalidData) {
		return true
	}
	for _, f := range dataErrorClassifiers {
		if f(err) {
			return true
		}
	}
	return false
}

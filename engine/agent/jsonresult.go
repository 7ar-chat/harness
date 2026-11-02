package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"
)

// JSON tool results stay valid JSON when the harness adds to them or cuts
// them (harness-v3.md "Review round 1"): the verification is a field, a
// spilled result keeps its shape with its large strings cut in place.

// isJSONObject reports whether a tool result is one JSON object.
func isJSONObject(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") && json.Valid([]byte(s))
}

// addJSONField appends key: value to a JSON object (its other fields keep
// their order and bytes); false when obj is not an object or has the key.
func addJSONField(obj, key string, value any) (string, bool) {
	obj = strings.TrimSpace(obj)
	n, err := parseJSON(obj)
	if err != nil || n.kind != '{' {
		return obj, false
	}
	for _, k := range n.keys {
		if k == key {
			return obj, false
		}
	}
	v, err := marshalNoEscape(value)
	if err != nil {
		return obj, false
	}
	body := strings.TrimSpace(obj[:len(obj)-1])
	k, _ := marshalNoEscape(key)
	if len(n.keys) == 0 {
		return "{" + k + ":" + v + "}", true
	}
	return body + "," + k + ":" + v + "}", true
}

func marshalNoEscape(v any) (string, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// jsonNode is a parsed JSON value that keeps the order of object keys.
type jsonNode struct {
	kind  byte // '{', '[', 's' (string), 'v' (any other scalar, kept raw)
	keys  []string
	vals  []*jsonNode
	str   string
	raw   string
	runes int // rune count of str
}

func parseJSON(s string) (*jsonNode, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	n, err := parseNode(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing data after the JSON value")
	}
	return n, nil
}

func parseNode(dec *json.Decoder) (*jsonNode, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		n := &jsonNode{kind: byte(t)}
		for dec.More() {
			if n.kind == '{' {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, _ := kt.(string)
				n.keys = append(n.keys, k)
			}
			v, err := parseNode(dec)
			if err != nil {
				return nil, err
			}
			n.vals = append(n.vals, v)
		}
		if _, err := dec.Token(); err != nil { // the closing delimiter
			return nil, err
		}
		return n, nil
	case string:
		return &jsonNode{kind: 's', str: t, runes: utf8.RuneCountInString(t)}, nil
	case json.Number:
		return &jsonNode{kind: 'v', raw: t.String()}, nil
	case bool:
		return &jsonNode{kind: 'v', raw: fmt.Sprint(t)}, nil
	case nil:
		return &jsonNode{kind: 'v', raw: "null"}, nil
	}
	return nil, fmt.Errorf("unexpected JSON token %v", tok)
}

func (n *jsonNode) encode(b *strings.Builder) {
	switch n.kind {
	case 's':
		s, _ := marshalNoEscape(n.str)
		b.WriteString(s)
	case 'v':
		b.WriteString(n.raw)
	case '{', '[':
		b.WriteByte(n.kind)
		for i, v := range n.vals {
			if i > 0 {
				b.WriteByte(',')
			}
			if n.kind == '{' {
				k, _ := marshalNoEscape(n.keys[i])
				b.WriteString(k + ":")
			}
			v.encode(b)
		}
		if n.kind == '{' {
			b.WriteByte('}')
		} else {
			b.WriteByte(']')
		}
	}
}

func (n *jsonNode) String() string {
	var b strings.Builder
	n.encode(&b)
	return b.String()
}

func (n *jsonNode) strings(out *[]*jsonNode) {
	switch n.kind {
	case 's':
		*out = append(*out, n)
	case '{', '[':
		for _, v := range n.vals {
			v.strings(out)
		}
	}
}

// set replaces or appends a top-level field of an object node.
func (n *jsonNode) set(key string, v *jsonNode) {
	for i, k := range n.keys {
		if k == key {
			n.vals[i] = v
			return
		}
	}
	n.keys, n.vals = append(n.keys, key), append(n.vals, v)
}

// minKeptChars: a string is never cut below this many characters.
const minKeptChars = 400

// shrinkJSON cuts the largest string values of a JSON-object result in place
// (head + marker + tail; the marker names file when not "") until the result
// fits in limit characters, and adds extra fields (outputPath, spilled…).
// ok is false when s is not an object or cannot be made to fit that way.
func shrinkJSON(s string, limit int, marker func(omitted, total int) string, extra map[string]*jsonNode) (string, bool) {
	root, err := parseJSON(strings.TrimSpace(s))
	if err != nil || root.kind != '{' {
		return s, false
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		root.set(k, extra[k])
	}
	var leaves []*jsonNode
	root.strings(&leaves)
	for range 64 {
		size := utf8.RuneCountInString(root.String())
		if size <= limit {
			return root.String(), true
		}
		sort.SliceStable(leaves, func(i, j int) bool { return leaves[i].runes > leaves[j].runes })
		if len(leaves) == 0 || leaves[0].runes <= minKeptChars {
			return s, false
		}
		big := leaves[0]
		r := []rune(big.str)
		// Escaping makes a string longer than its runes: cut a bit more than the excess.
		excess := size - limit
		keep := max(minKeptChars, len(r)-excess-excess/8-200)
		m := marker(len(r)-keep, len(r))
		keep = max(minKeptChars/2, keep-utf8.RuneCountInString(m))
		head := keep * 2 / 3
		tail := keep - head
		big.str = string(r[:head]) + m + string(r[len(r)-tail:])
		big.runes = utf8.RuneCountInString(big.str)
		if big.runes >= len(r) {
			return s, false
		}
	}
	return s, false
}

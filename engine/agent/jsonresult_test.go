package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestAddJSONFieldAppendsWithoutRewritingTheExistingFields(t *testing.T) {
	// Arrange
	obj := `  {"b": 1,  "a":"x <y> & z"}  `

	// Act
	got, ok := addJSONField(obj, "verification", []any{map[string]any{"summary": "a<b&c"}})

	// Assert
	want := `{"b": 1,  "a":"x <y> & z","verification":[{"summary":"a<b&c"}]}`
	if !ok || got != want {
		t.Fatalf("got %q %v, want %q", got, ok, want)
	}
	if !json.Valid([]byte(got)) {
		t.Fatalf("result is not valid JSON: %s", got)
	}
}

func TestAddJSONFieldFillsAnEmptyObject(t *testing.T) {
	for _, obj := range []string{`{}`, ` { } `} {
		// Act
		got, ok := addJSONField(obj, "k", 1)

		// Assert
		if !ok || got != `{"k":1}` {
			t.Errorf("%q: got %q %v", obj, got, ok)
		}
	}
}

func TestAddJSONFieldRefusesWhatItCannotAddTo(t *testing.T) {
	for name, tc := range map[string]struct {
		obj   string
		key   string
		value any
	}{
		"existing key":  {`{"k":1}`, "k", 2},
		"array":         {`[1,2]`, "k", 2},
		"not json":      {`plain text`, "k", 2},
		"trailing":      {`{"a":1} {"b":2}`, "k", 2},
		"unmarshalable": {`{"a":1}`, "k", make(chan int)},
	} {
		t.Run(name, func(t *testing.T) {
			// Act
			got, ok := addJSONField(tc.obj, tc.key, tc.value)

			// Assert
			if ok || got != strings.TrimSpace(tc.obj) {
				t.Fatalf("got %q %v, want the object back unchanged", got, ok)
			}
		})
	}
}

func TestParseJSONRoundTripsCompactDocumentsExactly(t *testing.T) {
	// Big numbers, nulls and bools keep their bytes; objects keep their key order.
	for _, doc := range []string{
		`{"z":1,"a":[1,true,null,"x",{"n":[]}],"big":12345678901234567890,"f":1.50,"e":{}}`,
		`[]`, `[[],[1,[2]]]`, `"just a string"`, `null`, `false`, `-0.5e3`,
	} {
		// Act
		n, err := parseJSON(doc)

		// Assert
		if err != nil {
			t.Fatalf("%s: %v", doc, err)
		}
		if got := n.String(); got != doc {
			t.Errorf("round trip changed the document:\n got %s\nwant %s", got, doc)
		}
	}
}

func TestParseJSONRejectsMalformedDocuments(t *testing.T) {
	for name, doc := range map[string]string{
		"empty":            ``,
		"trailing value":   `{"a":1} 2`,
		"truncated object": `{"a":1`,
		"truncated array":  `[1,2`,
		"bad key":          `{1:2}`,
		"missing value":    `{"a":}`,
		"bad nested":       `{"a":[1,}`,
	} {
		t.Run(name, func(t *testing.T) {
			if n, err := parseJSON(doc); err == nil {
				t.Fatalf("parsed %q as %s", doc, n)
			}
		})
	}
}

func TestShrinkJSONCutsTheLargestStringsInPlaceAndStaysValid(t *testing.T) {
	// Arrange
	huge := strings.Repeat("0123456789", 600) // 6000 runes
	doc := mustJSON(t, map[string]any{"exitCode": 0, "stdout": huge, "stderr": "warn", "nested": map[string]any{"log": strings.Repeat("é\"\n", 800)}})
	marker := func(omitted, total int) string { return fmt.Sprintf("[…%d of %d…]", omitted, total) }
	extra := map[string]*jsonNode{"spilled": {kind: 'v', raw: "true"}, "outputPath": {kind: 's', str: "/workspace/.work/tool-output/x.txt", runes: 34}}

	// Act
	got, ok := shrinkJSON(doc, 3000, marker, extra)

	// Assert
	if !ok {
		t.Fatal("not shrunk")
	}
	if n := utf8.RuneCountInString(got); n > 3000 {
		t.Fatalf("result has %d runes, limit 3000", n)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatalf("result is not valid JSON: %v\n%s", err, got)
	}
	if out["spilled"] != true || out["outputPath"] != "/workspace/.work/tool-output/x.txt" || out["stderr"] != "warn" || out["exitCode"] != float64(0) {
		t.Errorf("fields lost or changed: %v", out)
	}
	s := out["stdout"].(string)
	if !strings.HasPrefix(s, "0123456789") || !strings.HasSuffix(s, "0123456789") || !strings.Contains(s, "[…") {
		t.Errorf("stdout is not head + marker + tail: %q", s[:40])
	}
}

func TestShrinkJSONReplacesAnExistingFieldWithTheExtraOne(t *testing.T) {
	// Arrange
	doc := `{"outputPath":"old","a":"b"}`
	extra := map[string]*jsonNode{"outputPath": {kind: 's', str: "new", runes: 3}}

	// Act
	got, ok := shrinkJSON(doc, 1000, func(int, int) string { return "" }, extra)

	// Assert
	if !ok || got != `{"outputPath":"new","a":"b"}` {
		t.Fatalf("got %q %v", got, ok)
	}
}

func TestShrinkJSONGivesUpWhenNothingLargeCanBeCut(t *testing.T) {
	marker := func(int, int) string { return "…" }
	many := mustJSON(t, map[string]any{"items": make([]int, 3000)})
	smallStrings := map[string]any{}
	for i := range 200 {
		smallStrings[fmt.Sprintf("k%03d", i)] = strings.Repeat("x", 100)
	}
	for name, tc := range map[string]struct {
		doc   string
		limit int
	}{
		"not an object":   {`["a","b"]`, 1},
		"not json":        {`hello`, 1},
		"no strings":      {many, 100},
		"only small cuts": {mustJSON(t, smallStrings), 500},
	} {
		t.Run(name, func(t *testing.T) {
			// Act
			got, ok := shrinkJSON(tc.doc, tc.limit, marker, nil)

			// Assert
			if ok || got != tc.doc {
				t.Fatalf("ok=%v, result changed=%v", ok, got != tc.doc)
			}
		})
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestIsJSONObjectOnlyAcceptsOneValidObject(t *testing.T) {
	for doc, want := range map[string]bool{
		`{"a":1}`: true, "  {}\n": true, `[1]`: false, `{"a":1`: false, `{a}`: false, `text {}`: false, ``: false,
	} {
		if got := isJSONObject(doc); got != want {
			t.Errorf("isJSONObject(%q) = %v", doc, got)
		}
	}
}

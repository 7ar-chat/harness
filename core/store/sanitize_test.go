package store_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/core/store"
)

func TestCleanTextMakesAnyStringStorableInPostgres(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"clean text is untouched", "héllo wörld 日本", "héllo wörld 日本"},
		{"empty", "", ""},
		{"NUL becomes the replacement character", "a\x00b", "a\uFFFDb"},
		{"every NUL is replaced", "\x00\x00", "\uFFFD\uFFFD"},
		{"an invalid byte becomes the replacement character", "ab\xffcd", "ab\uFFFDcd"},
		{"a truncated multi-byte sequence", "ok\xe6\x97", "ok\uFFFD"},
		{"both problems at once", "\x00\xff", "\uFFFD\uFFFD"},
	}
	for _, c := range cases {
		// Act
		got := store.CleanText(c.in)

		// Assert
		if got != c.want {
			t.Errorf("%s: CleanText(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestCleanValueSanitizesKeysAndNestedStringsButNotOtherTypes(t *testing.T) {
	// Arrange
	in := map[string]any{
		"k\x00ey": "v\x00al",
		"list":    []any{"a\xff", float64(3), true, nil, map[string]any{"deep\x00": "x\x00"}},
		"n":       json.Number("7"),
	}

	// Act
	got := store.CleanValue(in).(map[string]any)

	// Assert
	if got["k\uFFFDey"] != "v\uFFFDal" {
		t.Errorf("key/value not cleaned: %v", got)
	}
	list := got["list"].([]any)
	if list[0] != "a\uFFFD" || list[1] != float64(3) || list[2] != true || list[3] != nil {
		t.Errorf("list = %v", list)
	}
	if deep := list[4].(map[string]any); deep["deep\uFFFD"] != "x\uFFFD" {
		t.Errorf("nested map = %v", deep)
	}
	if got["n"] != json.Number("7") {
		t.Errorf("number changed: %v", got["n"])
	}
	if _, ok := in["k\x00ey"]; !ok {
		t.Error("the input map was modified")
	}
}

func TestCleanJSONOnlyRewritesDocumentsPostgresWouldRefuse(t *testing.T) {
	// Arrange
	clean := []byte(`{"a":"b","n":1.50}`)

	// Act
	same := store.CleanJSON(clean)
	nul := store.CleanJSON([]byte(`{"text":"a\u0000b","n":12345678901234567890,"list":["\u0000"]}`))
	invalid := store.CleanJSON([]byte("{\"text\":\"a\xffb\"}"))
	notJSON := store.CleanJSON([]byte("plain \xff text"))

	// Assert
	if string(same) != string(clean) {
		t.Errorf("a valid document was rewritten: %s", same)
	}
	if !json.Valid(nul) || strings.Contains(string(nul), `\u0000`) {
		t.Errorf("NUL escape survived: %s", nul)
	}
	var doc struct {
		Text string
		N    json.Number
		List []string
	}
	dec := json.NewDecoder(strings.NewReader(string(nul)))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if doc.Text != "a\uFFFDb" || doc.List[0] != "\uFFFD" {
		t.Errorf("cleaned document = %+v", doc)
	}
	if doc.N.String() != "12345678901234567890" {
		t.Errorf("a large number lost precision: %s", doc.N)
	}
	if !json.Valid(invalid) || !strings.Contains(string(invalid), "a\uFFFDb") {
		t.Errorf("invalid UTF-8 not replaced: %q", invalid)
	}
	var s string
	if err := json.Unmarshal(notJSON, &s); err != nil || s != "plain \uFFFD text" {
		t.Errorf("non-JSON input stored as %q (%v), want a JSON string", notJSON, err)
	}
}

func TestMessageCleanSanitizesEveryTextField(t *testing.T) {
	// Arrange
	m := &store.Message{
		ID: "m\x00", Content: "c\xff", ToolCallID: "t\x00", Error: "e\x00",
		ToolCalls: []store.ToolCall{{ID: "i\x00", Type: "function", Function: store.ToolCallFunction{Name: "n\x00", Arguments: `{"x":"\x00"}` + "\x00"}}},
		Metadata:  map[string]any{"k\x00": []any{"v\x00"}},
	}

	// Act
	m.Clean()

	// Assert
	tc := m.ToolCalls[0]
	for name, got := range map[string]string{"id": m.ID, "content": m.Content, "toolCallId": m.ToolCallID, "error": m.Error, "call id": tc.ID, "call name": tc.Function.Name, "call args": tc.Function.Arguments} {
		if strings.ContainsRune(got, 0) || !strings.Contains(got, "\uFFFD") && name != "call args" {
			t.Errorf("%s = %q", name, got)
		}
	}
	if !strings.HasSuffix(tc.Function.Arguments, "\uFFFD") {
		t.Errorf("call args = %q", tc.Function.Arguments)
	}
	if v := m.Metadata["k\uFFFD"]; fmt.Sprint(v) != "[v\uFFFD]" {
		t.Errorf("metadata = %v", m.Metadata)
	}
}

func TestMessageCleanLeavesNilMetadataNil(t *testing.T) {
	// Arrange
	m := &store.Message{Content: "x"}

	// Act
	m.Clean()

	// Assert
	if m.Metadata != nil {
		t.Fatalf("metadata became %v", m.Metadata)
	}
}

type pgLikeError struct{ code string }

func (e pgLikeError) Error() string { return "pg " + e.code }

func TestIsDataErrorRecognisesPermanentFailuresOnly(t *testing.T) {
	// Arrange: a backend registers its classifier (as pgstore does from init)
	store.RegisterDataError(func(err error) bool {
		var pe pgLikeError
		return errors.As(err, &pe) && strings.HasPrefix(pe.code, "22")
	})

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"ErrInvalidData", store.ErrInvalidData, true},
		{"wrapped ErrInvalidData", fmt.Errorf("insert: %w", store.ErrInvalidData), true},
		{"a registered data exception", pgLikeError{"22021"}, true},
		{"a wrapped data exception", fmt.Errorf("x: %w", pgLikeError{"22P05"}), true},
		{"a transient error", pgLikeError{"57P01"}, false},
		{"a plain error", errors.New("connection refused"), false},
		{"not found is not a data error", store.ErrNotFound, false},
	}
	for _, c := range cases {
		// Act
		got := store.IsDataError(c.err)

		// Assert
		if got != c.want {
			t.Errorf("%s: IsDataError = %v, want %v", c.name, got, c.want)
		}
	}
}

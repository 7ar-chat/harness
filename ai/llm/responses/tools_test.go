package responses_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/responses"
)

func schema(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestStrictAcceptsOnlySchemasTheProviderCanEnforceAsIs(t *testing.T) {
	// Strict mode needs every property required, additionalProperties false at each object, typed nodes and supported keywords only; anything else must fall back to non-strict so optional parameters stay optional.
	// Arrange
	cases := []struct {
		name string
		json string
		want bool
	}{
		{"flat object", `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"],"additionalProperties":false}`, true},
		{"no parameters", `{"type":"object","properties":{},"required":[],"additionalProperties":false}`, true},
		{"nested object and array", `{"type":"object","properties":{"o":{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"],"additionalProperties":false},"l":{"type":"array","items":{"type":"string"}}},"required":["o","l"],"additionalProperties":false}`, true},
		{"nullable via type list", `{"type":"object","properties":{"a":{"type":["string","null"]}},"required":["a"],"additionalProperties":false}`, true},
		{"anyOf", `{"type":"object","properties":{"a":{"anyOf":[{"type":"string"},{"type":"null"}]}},"required":["a"],"additionalProperties":false}`, true},
		{"enum and description", `{"type":"object","properties":{"a":{"type":"string","enum":["x","y"],"description":"d"}},"required":["a"],"additionalProperties":false}`, true},
		{"top level not object", `{"type":"string"}`, false},
		{"optional property", `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}},"required":["a"],"additionalProperties":false}`, false},
		{"required but undeclared", `{"type":"object","properties":{"a":{"type":"string"}},"required":["a","ghost"],"additionalProperties":false}`, false},
		{"additionalProperties missing", `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`, false},
		{"additionalProperties true", `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"],"additionalProperties":true}`, false},
		{"additionalProperties schema", `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"],"additionalProperties":{"type":"string"}}`, false},
		{"unsupported keyword", `{"type":"object","properties":{"a":{"type":"string","minLength":1}},"required":["a"],"additionalProperties":false}`, false},
		{"unsupported keyword at root", `{"type":"object","properties":{},"required":[],"additionalProperties":false,"$schema":"x"}`, false},
		{"untyped property", `{"type":"object","properties":{"a":{"description":"anything"}},"required":["a"],"additionalProperties":false}`, false},
		{"unknown type", `{"type":"object","properties":{"a":{"type":"date"}},"required":["a"],"additionalProperties":false}`, false},
		{"array without items", `{"type":"object","properties":{"a":{"type":"array"}},"required":["a"],"additionalProperties":false}`, false},
		{"array of loose objects", `{"type":"object","properties":{"a":{"type":"array","items":{"type":"object","properties":{"x":{"type":"string"}},"required":[]}}},"required":["a"],"additionalProperties":false}`, false},
		{"property not a schema", `{"type":"object","properties":{"a":true},"required":["a"],"additionalProperties":false}`, false},
		{"anyOf with loose branch", `{"type":"object","properties":{"a":{"anyOf":[{"type":"string"},{"type":"object"}]}},"required":["a"],"additionalProperties":false}`, false},
		{"anyOf with non-schema branch", `{"type":"object","properties":{"a":{"anyOf":["string"]}},"required":["a"],"additionalProperties":false}`, false},
		{"type of wrong kind", `{"type":"object","properties":{"a":{"type":5}},"required":["a"],"additionalProperties":false}`, false},
		{"empty type list", `{"type":"object","properties":{"a":{"type":[]}},"required":["a"],"additionalProperties":false}`, false},
		{"type list with non-string only", `{"type":"object","properties":{"a":{"type":[1]}},"required":["a"],"additionalProperties":false}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Act
			got := responses.Strict(schema(t, tc.json))

			// Assert
			if got != tc.want {
				t.Fatalf("Strict = %v, want %v for %s", got, tc.want, tc.json)
			}
		})
	}
}

// sentTools runs a call on a model offering infos and returns the tools array of the request.
func sentTools(t *testing.T, infos []*ai.ToolInfo) []map[string]any {
	t.Helper()
	url, rec := serve(t, stream(okStream))
	m, err := newModel(t, url).WithTools(infos)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Generate(context.Background(), hi()); err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	raw, _ := rec.all()[0].body["tools"].([]any)
	for _, x := range raw {
		out = append(out, x.(map[string]any))
	}
	return out
}

func TestToolsWithoutParametersAreSentAsAnEmptyObjectSchema(t *testing.T) {
	// A tool with no (or an empty) parameter schema is offered as an object with no properties; description and name are kept.
	// Arrange
	infos := []*ai.ToolInfo{{Name: "now", Desc: "current time"}, {Name: "empty", Parameters: map[string]any{}}}

	// Act
	tools := sentTools(t, infos)

	// Assert
	if len(tools) != 2 {
		t.Fatalf("tools %v", tools)
	}
	for _, tool := range tools {
		params := tool["parameters"].(map[string]any)
		if tool["type"] != "function" || params["type"] != "object" || !reflect.DeepEqual(params["properties"], map[string]any{}) {
			t.Errorf("tool %v", tool)
		}
	}
	if tools[0]["name"] != "now" || tools[0]["description"] != "current time" {
		t.Errorf("tool %v", tools[0])
	}
}

func TestObjectSchemaWithoutPropertiesGetsAnEmptyPropertiesMap(t *testing.T) {
	// OpenAI rejects an object schema lacking "properties", so one is added to a schema that declares only its type.
	// Arrange
	infos := []*ai.ToolInfo{{Name: "t", Parameters: map[string]any{"type": "object", "additionalProperties": false}}}

	// Act
	tools := sentTools(t, infos)

	// Assert
	params := tools[0]["parameters"].(map[string]any)
	if _, ok := params["properties"].(map[string]any); !ok {
		t.Fatalf("parameters %v, want a properties object", params)
	}
	if tools[0]["strict"] != true {
		t.Fatalf("strict = %v, want true: no properties means nothing optional", tools[0]["strict"])
	}
}

func TestToolSchemasAreCopiedNotShared(t *testing.T) {
	// The request may normalise the schema it sends; the caller's ToolInfo.Parameters stays untouched.
	// Arrange
	params := map[string]any{"type": "object"}
	infos := []*ai.ToolInfo{{Name: "t", Parameters: params}}

	// Act
	_ = sentTools(t, infos)

	// Assert
	if _, added := params["properties"]; added {
		t.Fatalf("caller's schema was modified: %v", params)
	}
}

func TestUnserialisableToolSchemaFallsBackToNoParameters(t *testing.T) {
	// A schema that cannot be encoded to JSON does not break the request: the tool is offered with an empty object schema.
	// Arrange
	infos := []*ai.ToolInfo{{Name: "t", Parameters: map[string]any{"type": "object", "bad": make(chan int)}}}

	// Act
	tools := sentTools(t, infos)

	// Assert
	params := tools[0]["parameters"].(map[string]any)
	if len(tools) != 1 || params["type"] != "object" || !reflect.DeepEqual(params["properties"], map[string]any{}) || len(params) != 2 {
		t.Fatalf("tools %v", tools)
	}
}

package responses

import (
	"encoding/json"
	"fmt"

	"github.com/cloudwego/eino/schema"
)

// functionTool is a function tool of a Responses request.
type functionTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
	Strict      bool           `json:"strict"`
}

// toFunctionTools converts eino tool infos; a tool is strict when its schema
// already meets the strict-mode rules (no rewrite: optional parameters stay optional).
func toFunctionTools(infos []*schema.ToolInfo) ([]functionTool, error) {
	out := make([]functionTool, 0, len(infos))
	for _, info := range infos {
		if info == nil {
			continue
		}
		params := map[string]any{"type": "object", "properties": map[string]any{}}
		if info.ParamsOneOf != nil {
			js, err := info.ParamsOneOf.ToJSONSchema()
			if err != nil {
				return nil, fmt.Errorf("tool %s: %w", info.Name, err)
			}
			if js != nil {
				b, err := json.Marshal(js)
				if err != nil {
					return nil, fmt.Errorf("tool %s: %w", info.Name, err)
				}
				var m map[string]any
				if err := json.Unmarshal(b, &m); err != nil {
					return nil, fmt.Errorf("tool %s: %w", info.Name, err)
				}
				if m != nil {
					params = m
				}
			}
		}
		if _, ok := params["properties"]; !ok && params["type"] == "object" {
			params["properties"] = map[string]any{}
		}
		out = append(out, functionTool{Type: "function", Name: info.Name, Description: info.Desc, Parameters: params, Strict: Strict(params)})
	}
	return out, nil
}

// strictKeywords are the JSON Schema keywords a strict function schema may use here.
var strictKeywords = map[string]bool{
	"type": true, "properties": true, "required": true, "additionalProperties": true, "items": true,
	"enum": true, "const": true, "description": true, "title": true, "anyOf": true,
}

// Strict reports whether a JSON schema can be sent with strict:true as is: every
// object lists all its properties as required and sets additionalProperties
// false, every node has a type (or anyOf), and only supported keywords appear.
func Strict(s map[string]any) bool {
	if s["type"] != "object" {
		return false
	}
	return strictNode(s)
}

func strictNode(s map[string]any) bool {
	for k := range s {
		if !strictKeywords[k] {
			return false
		}
	}
	if anyOf, ok := s["anyOf"].([]any); ok {
		for _, v := range anyOf {
			sub, ok := v.(map[string]any)
			if !ok || !strictNode(sub) {
				return false
			}
		}
		return true
	}
	types := typeNames(s["type"])
	if len(types) == 0 {
		return false
	}
	for _, t := range types {
		switch t {
		case "object":
			if ap, ok := s["additionalProperties"].(bool); !ok || ap {
				return false
			}
			props, _ := s["properties"].(map[string]any)
			required := map[string]bool{}
			if req, ok := s["required"].([]any); ok {
				for _, r := range req {
					if name, ok := r.(string); ok {
						required[name] = true
					}
				}
			}
			if len(required) != len(props) {
				return false
			}
			for name, v := range props {
				sub, ok := v.(map[string]any)
				if !ok || !required[name] || !strictNode(sub) {
					return false
				}
			}
		case "array":
			items, ok := s["items"].(map[string]any)
			if !ok || !strictNode(items) {
				return false
			}
		case "string", "number", "integer", "boolean", "null":
		default:
			return false
		}
	}
	return true
}

func typeNames(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/eino-contrib/jsonschema"
)

// ToolInfo describes a tool to the model.
type ToolInfo struct {
	Name string `json:"name"`
	Desc string `json:"desc"`
	// Parameters is the JSON schema of the arguments object (nil: no parameters).
	Parameters map[string]any `json:"parameters,omitempty"`
}

// BaseTool is a tool the model can be told about.
type BaseTool interface {
	Info(ctx context.Context) (*ToolInfo, error)
}

// InvokableTool is a BaseTool the agent can run: arguments in as a JSON object, a text result out.
type InvokableTool interface {
	BaseTool
	InvokableRun(ctx context.Context, arguments string) (string, error)
}

// InferTool builds a tool whose parameters schema is reflected from T (json and jsonschema struct
// tags) and whose result is D rendered as JSON (a string is returned as is).
func InferTool[T, D any](name, desc string, fn func(ctx context.Context, input T) (D, error)) (InvokableTool, error) {
	r := &jsonschema.Reflector{Anonymous: true, DoNotReference: true}
	js := r.Reflect(newInstance[T]())
	js.Version = ""
	params, err := toMap(js)
	if err != nil {
		return nil, fmt.Errorf("tool %s: parameters schema: %w", name, err)
	}
	return &funcTool[T, D]{info: &ToolInfo{Name: name, Desc: desc, Parameters: params}, fn: fn}, nil
}

type funcTool[T, D any] struct {
	info *ToolInfo
	fn   func(ctx context.Context, input T) (D, error)
}

func (t *funcTool[T, D]) Info(context.Context) (*ToolInfo, error) { return t.info, nil }

// The error wording below is what the model sees when a call fails; it is kept stable.
func (t *funcTool[T, D]) InvokableRun(ctx context.Context, arguments string) (string, error) {
	inst := newInstance[T]()
	if err := json.Unmarshal([]byte(arguments), &inst); err != nil {
		return "", fmt.Errorf("[LocalFunc] failed to unmarshal arguments in json, toolName=%s, err=%w", t.info.Name, err)
	}
	out, err := t.fn(ctx, inst)
	if err != nil {
		return "", fmt.Errorf("[LocalFunc] failed to invoke tool, toolName=%s, err=%w", t.info.Name, err)
	}
	s, err := renderResult(out)
	if err != nil {
		return "", fmt.Errorf("[LocalFunc] failed to marshal output in json, toolName=%s, err=%w", t.info.Name, err)
	}
	return s, nil
}

func renderResult(v any) (string, error) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

func toMap(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// newInstance returns a T ready to unmarshal into: a pointer type gets its element allocated.
func newInstance[T any]() T {
	var zero T
	t := reflect.TypeOf((*T)(nil)).Elem()
	if t.Kind() == reflect.Pointer {
		return reflect.New(t.Elem()).Interface().(T)
	}
	return zero
}

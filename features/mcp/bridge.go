package mcptools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GhaziBenDahmane/harness/ai"
)

// maxToolPages bounds how many pages of tools one server may list.
const maxToolPages = 20

// toolSession is the part of an MCP client session the bridge uses.
type toolSession interface {
	ListTools(ctx context.Context, params *mcp.ListToolsParams) (*mcp.ListToolsResult, error)
	CallTool(ctx context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error)
}

// bridgeConfig describes the tools to take from one server.
type bridgeConfig struct {
	Session    toolSession
	ServerName string
	// Only, when not empty, restricts the tools to these (raw) names.
	Only []string
	// Expose maps a server tool to the name the model sees (must be non-empty and unique).
	Expose func(server string, t *mcp.Tool) string
	// MaxChars caps a result (0: no cap), keeping PreserveTail characters of its end.
	MaxChars     int
	PreserveTail int
}

// listBridgeTools lists the server's tools (every page) and wraps them as ai tools.
func listBridgeTools(ctx context.Context, c bridgeConfig) ([]ai.BaseTool, error) {
	raw, err := listAllTools(ctx, c.Session)
	if err != nil {
		return nil, fmt.Errorf("list mcp tools: %w", err)
	}
	only := map[string]bool{}
	for _, n := range c.Only {
		only[n] = true
	}
	seen := map[string]bool{}
	out := make([]ai.BaseTool, 0, len(raw))
	for _, t := range raw {
		if len(only) > 0 && !only[t.Name] {
			continue
		}
		exposed := c.Expose(c.ServerName, t)
		if exposed == "" {
			return nil, fmt.Errorf("map mcp tool name fail, tool name: %s: exposed tool name is empty", t.Name)
		}
		if seen[exposed] {
			return nil, fmt.Errorf("duplicate mcp exposed tool name: %s", exposed)
		}
		seen[exposed] = true
		params, err := toParams(t.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("conv mcp tool input schema fail: %w, tool name: %s", err, t.Name)
		}
		out = append(out, &bridgeTool{
			session: c.Session, raw: t.Name, maxChars: c.MaxChars, preserveTail: c.PreserveTail,
			info: &ai.ToolInfo{Name: exposed, Desc: t.Description, Parameters: params},
		})
	}
	return out, nil
}

func listAllTools(ctx context.Context, s toolSession) ([]*mcp.Tool, error) {
	var tools []*mcp.Tool
	cursor := ""
	seen := map[string]bool{}
	for page := 0; ; page++ {
		if page >= maxToolPages {
			return nil, fmt.Errorf("exceeded max tool pages: %d", maxToolPages)
		}
		if cursor != "" {
			if seen[cursor] {
				return nil, fmt.Errorf("repeated cursor: %s", cursor)
			}
			seen[cursor] = true
		}
		res, err := s.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		tools = append(tools, res.Tools...)
		if res.NextCursor == "" {
			return tools, nil
		}
		cursor = res.NextCursor
	}
}

func toParams(schema any) (map[string]any, error) {
	if schema == nil {
		return nil, nil
	}
	b, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

type bridgeTool struct {
	session      toolSession
	raw          string
	info         *ai.ToolInfo
	maxChars     int
	preserveTail int
}

func (t *bridgeTool) Info(context.Context) (*ai.ToolInfo, error) { return t.info, nil }

func (t *bridgeTool) InvokableRun(ctx context.Context, arguments string) (string, error) {
	res, err := t.session.CallTool(ctx, &mcp.CallToolParams{Name: t.raw, Arguments: json.RawMessage(arguments)})
	if err != nil {
		return "", fmt.Errorf("failed to call mcp tool: %w", err)
	}
	if res == nil {
		return "", errors.New("mcp tool result is nil")
	}
	out := map[string]any{"content": res.Content}
	if res.IsError {
		out["isError"] = true
	}
	s, err := marshalJSON(out)
	if err != nil {
		return "", fmt.Errorf("failed to marshal mcp tool result: %w", err)
	}
	if t.maxChars > 0 && utf8.RuneCountInString(s) > t.maxChars {
		if s, err = truncatedResult(s, t.maxChars, t.preserveTail); err != nil {
			return "", fmt.Errorf("failed to marshal mcp tool result: %w", err)
		}
	}
	if res.IsError {
		return "", fmt.Errorf("failed to call mcp tool, mcp server return error: %s", s)
	}
	return s, nil
}

func marshalJSON(v any) (string, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// truncatedResult wraps the head and tail of an oversized result in a text envelope of at most maxChars.
func truncatedResult(original string, maxChars, tail int) (string, error) {
	originalChars := utf8.RuneCountInString(original)
	payloadChars := maxChars
	for payloadChars >= 0 {
		payload := ""
		if payloadChars > 0 {
			payload = truncateString(original, payloadChars, tail)
		}
		s, err := marshalJSON(map[string]any{
			"content": []map[string]any{{"type": "text", "text": payload}},
			"_meta":   map[string]any{"truncated": true, "originalChars": originalChars, "returnedChars": utf8.RuneCountInString(payload)},
		})
		if err != nil {
			return "", err
		}
		over := utf8.RuneCountInString(s) - maxChars
		if over <= 0 {
			return s, nil
		}
		if payload == "" {
			return "", fmt.Errorf("truncated mcp tool result exceeds max chars: max=%d actual=%d", maxChars, utf8.RuneCountInString(s))
		}
		payloadChars = max(payloadChars-over, 0)
	}
	return "", fmt.Errorf("truncated mcp tool result exceeds max chars: max=%d", maxChars)
}

func truncateString(s string, maxChars, tail int) string {
	n := utf8.RuneCountInString(s)
	if maxChars <= 0 || n <= maxChars {
		return s
	}
	marker := fmt.Sprintf("...[truncated original_chars=%d returned_chars=%d]...", n, maxChars)
	m := utf8.RuneCountInString(marker)
	if m >= maxChars {
		return string([]rune(marker)[:maxChars])
	}
	tail = min(max(tail, 0), maxChars-m)
	head := maxChars - m - tail
	r := []rune(s)
	return string(r[:head]) + marker + string(r[len(r)-tail:])
}

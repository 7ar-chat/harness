package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Discover asks the provider which models it serves: GET {BaseURL}/models of an
// OpenAI-compatible endpoint, GET {BaseURL}/v1/models of Anthropic. The key goes
// only to the address in o. The ids come back sorted.
func Discover(ctx context.Context, hc *http.Client, o Options) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	base := strings.TrimRight(o.BaseURL, "/")
	var url string
	hdr := http.Header{}
	switch o.Provider {
	case "anthropic":
		if base == "" {
			base = "https://api.anthropic.com"
		}
		url = base + "/v1/models?limit=100"
		hdr.Set("x-api-key", o.APIKey)
		hdr.Set("anthropic-version", "2023-06-01")
	default:
		if base == "" {
			base = "https://api.openai.com/v1"
		}
		url = base + "/models"
		if o.APIKey != "" {
			hdr.Set("Authorization", "Bearer "+o.APIKey)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header = hdr
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("model discovery: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("model discovery: %s answered %s", base, resp.Status)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("model discovery: unreadable answer from %s", base)
	}
	ids := make([]string, 0, len(out.Data))
	for _, d := range out.Data {
		if d.ID != "" {
			ids = append(ids, d.ID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

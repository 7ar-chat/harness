package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/GhaziBenDahmane/harness/config"
)

// discoverer returns the func /model uses to ask the selected provider which
// models it serves. The key goes only to the address the configuration chose
// (it was already trusted to receive it for the runs themselves).
func discoverer(cfg *config.Config) func(context.Context) ([]string, error) {
	return func(ctx context.Context) ([]string, error) {
		return discoverModels(ctx, http.DefaultClient, cfg.LLMProvider, cfg.LLMBaseURL, cfg.LLMAPIKey)
	}
}

// discoverModels lists GET {base}/models of an OpenAI-compatible endpoint, or
// {base}/v1/models of Anthropic.
func discoverModels(ctx context.Context, hc *http.Client, typ, base, key string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	base = strings.TrimRight(base, "/")
	var url string
	hdr := http.Header{}
	switch typ {
	case "anthropic":
		if base == "" {
			base = "https://api.anthropic.com"
		}
		url = base + "/v1/models?limit=100"
		hdr.Set("x-api-key", key)
		hdr.Set("anthropic-version", "2023-06-01")
	default:
		if base == "" {
			base = "https://api.openai.com/v1"
		}
		url = base + "/models"
		if key != "" {
			hdr.Set("Authorization", "Bearer "+key)
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

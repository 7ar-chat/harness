// Package modelinfo describes models: their context window, output limit,
// capabilities and per-token prices, read from LiteLLM's public price table
// (model_prices_and_context_window.json). It imports nothing from the harness;
// callers pass the provider they configured as a hint to Lookup.
//
// The table is data, not authority: ids are matched conservatively (exact, then
// provider-prefixed, then the same name under another provider or a dated
// release of the same model) and never across unrelated models.
package modelinfo

import (
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

// Info is what is known about one model. A zero field is unknown.
type Info struct {
	ID       string
	Provider string
	// ContextWindow is the input window in tokens, MaxOutput the largest answer.
	ContextWindow, MaxOutput int
	// Prices are in dollars per token. Priced is false when the table has no
	// price at all for the model (a zero price is then "unknown", not "free").
	InputPerToken, OutputPerToken, CacheReadPerToken, CacheWritePerToken float64
	Priced                                                               bool
	// Tier holds the higher prices some models charge for a request whose
	// prompt exceeds Tier.Above tokens (nil: one price).
	Tier                     *Tier
	Vision, Reasoning, Tools bool
}

// Tier is the price list of requests above a prompt size ("_above_200k_tokens").
// A zero price falls back to the base price.
type Tier struct {
	Above                                int
	Input, Output, CacheRead, CacheWrite float64
}

// Usage is the token usage a cost is computed from. In counts every input token
// including the cached ones; reasoning tokens are part of Out.
type Usage struct {
	In, Out, Cached int
	// Prompt is the size of the largest single prompt, which selects the price
	// tier (0: the base prices).
	Prompt int
}

// Cost is the price of u in dollars. ok is false when the model has no price,
// in which case the amount is 0. Cached tokens are billed at the cache-read
// price when known, else at the input price.
func Cost(i Info, u Usage) (usd float64, ok bool) {
	if !i.Priced {
		return 0, false
	}
	in, out, cr := i.InputPerToken, i.OutputPerToken, i.CacheReadPerToken
	if t := i.Tier; t != nil && t.Above > 0 && u.Prompt > t.Above {
		in, out, cr = orElse(t.Input, in), orElse(t.Output, out), orElse(t.CacheRead, cr)
	}
	if cr <= 0 {
		cr = in
	}
	cached := min(max(u.Cached, 0), max(u.In, 0))
	fresh := max(u.In, 0) - cached
	return float64(fresh)*in + float64(cached)*cr + float64(max(u.Out, 0))*out, true
}

func orElse(v, def float64) float64 {
	if v > 0 {
		return v
	}
	return def
}

// Table is a set of models. It is safe for concurrent use; the zero value and a
// nil *Table are empty.
type Table struct {
	mu      sync.RWMutex
	entries map[string]Info
	byLast  map[string][]string // name after the last "/" -> keys
	byNorm  map[string][]string // normalized name -> keys
	memo    map[string]found
	ready   atomic.Bool
}

type found struct {
	info Info
	ok   bool
}

// Ready reports that loading has finished (successfully or not): until then the
// table may simply not have arrived yet.
func (t *Table) Ready() bool { return t != nil && t.ready.Load() }

// Len is the number of models.
func (t *Table) Len() int {
	if t == nil {
		return 0
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.entries)
}

// Set adds entries, replacing the models of the same id.
func (t *Table) Set(entries map[string]Info) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries == nil {
		t.entries = map[string]Info{}
	}
	for k, v := range entries {
		v.ID = k
		t.entries[k] = v
	}
	t.byLast, t.byNorm, t.memo = map[string][]string{}, map[string][]string{}, nil
	for k := range t.entries {
		last := lastSegment(k)
		t.byLast[last] = append(t.byLast[last], k)
		n := normalize(k)
		t.byNorm[n] = append(t.byNorm[n], k)
	}
	for _, m := range []map[string][]string{t.byLast, t.byNorm} {
		for _, ks := range m {
			slices.Sort(ks)
		}
	}
}

// Lookup finds the model id as served by the provider providerHint (the
// configured provider type or name; "" when unknown): an exact entry, then
// "<hint>/<id>", then the same name under another prefix, then the same model
// as a dated release or an undated alias. A miss is (Info{}, false).
func (t *Table) Lookup(id, providerHint string) (Info, bool) {
	id = strings.TrimSpace(id)
	if t == nil || id == "" {
		return Info{}, false
	}
	hint := strings.ToLower(strings.TrimSpace(providerHint))
	key := id + "\x00" + hint
	t.mu.RLock()
	m, hit := t.memo[key]
	t.mu.RUnlock()
	if hit {
		return m.info, m.ok
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	info, ok := t.find(id, hint)
	if t.memo == nil {
		t.memo = map[string]found{}
	}
	t.memo[key] = found{info, ok}
	return info, ok
}

func (t *Table) find(id, hint string) (Info, bool) {
	if i, ok := t.entries[id]; ok {
		return i, true
	}
	if hint != "" {
		if i, ok := t.entries[hint+"/"+id]; ok {
			return i, true
		}
	}
	if k := t.best(t.byLast[lastSegment(id)], hint); k != "" {
		return t.entries[k], true
	}
	n := normalize(id)
	if k := t.best(t.byNorm[n], hint); k != "" {
		return t.entries[k], true
	}
	// A dated id of an undated entry (claude-sonnet-4-5-20250929 -> claude-sonnet-4-5)...
	if base, ok := stripDate(n); ok {
		if k := t.best(t.byNorm[base], hint); k != "" {
			return t.entries[k], true
		}
	}
	// ...and an undated id of dated entries: the latest release.
	var latest string
	for nk := range t.byNorm {
		if rest, ok := strings.CutPrefix(nk, n+"-"); ok && reDateOnly.MatchString(rest) && nk > latest {
			latest = nk
		}
	}
	if latest != "" {
		return t.entries[t.best(t.byNorm[latest], hint)], true
	}
	return Info{}, false
}

// best picks among keys naming the same model: the provider asked for, then a
// bare id, then an entry prefixed with its own provider, then the shortest key.
func (t *Table) best(keys []string, hint string) string {
	rank := func(k string) int {
		e := t.entries[k]
		switch {
		case hint != "" && strings.ToLower(e.Provider) == hint:
			return 0
		case !strings.ContainsAny(k, "/."):
			return 1
		case strings.HasPrefix(k, e.Provider+"/"):
			return 2
		}
		return 3
	}
	var out string
	for _, k := range keys {
		if out == "" || rank(k) < rank(out) || rank(k) == rank(out) && len(k) < len(out) {
			out = k
		}
	}
	return out
}

var (
	reDate     = regexp.MustCompile(`-(\d{8}|\d{4}-\d{2}-\d{2})$`)
	reDateOnly = regexp.MustCompile(`^(\d{8}|\d{4}-\d{2}-\d{2})$`)
	reVersion  = regexp.MustCompile(`-v\d+(:\d+)?$`)
	reBedrock  = regexp.MustCompile(`^((us|eu|apac|au|jp|global|us-gov)\.)?(anthropic|meta|amazon|mistral|cohere|ai21|deepseek|openai|qwen|writer)\.`)
	reAtDate   = regexp.MustCompile(`@(\d{8})$`)

	errNoEntry = errors.New("modelinfo: no model entries")
)

func lastSegment(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// normalize reduces an id to the model name: lower case, no provider prefix, no
// bedrock region/vendor prefix or version suffix, "@date" written as "-date".
func normalize(s string) string {
	s = strings.ToLower(lastSegment(s))
	s = reBedrock.ReplaceAllString(s, "")
	s = reVersion.ReplaceAllString(s, "")
	return reAtDate.ReplaceAllString(s, "-$1")
}

func stripDate(s string) (string, bool) {
	if loc := reDate.FindStringIndex(s); loc != nil {
		return s[:loc[0]], true
	}
	return s, false
}

// Parse reads the LiteLLM table: a JSON object of model id -> specification.
// The "sample_spec" documentation entry, entries that are not objects and
// non-chat models (embeddings, images…) are skipped; fields of the wrong type
// are ignored.
func Parse(data []byte) (map[string]Info, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	out := make(map[string]Info, len(raw))
	for id, r := range raw {
		if id == "sample_spec" {
			continue
		}
		var f map[string]any
		if json.Unmarshal(r, &f) != nil || f == nil {
			continue
		}
		if mode, _ := f["mode"].(string); mode != "" && mode != "chat" && mode != "responses" && mode != "completion" {
			continue
		}
		out[id] = fromFields(id, f)
	}
	if len(out) == 0 {
		return nil, errNoEntry
	}
	return out, nil
}

func fromFields(id string, f map[string]any) Info {
	num := func(k string) float64 {
		v, _ := f[k].(float64)
		return max(v, 0)
	}
	flag := func(k string) bool { b, _ := f[k].(bool); return b }
	i := Info{ID: id}
	i.Provider, _ = f["litellm_provider"].(string)
	maxIn, maxOut, maxTok := int(num("max_input_tokens")), int(num("max_output_tokens")), int(num("max_tokens"))
	i.ContextWindow, i.MaxOutput = maxIn, maxOut
	if maxIn == 0 && maxOut == 0 {
		i.ContextWindow = maxTok // a legacy entry: max_tokens is all there is
	} else if maxOut == 0 {
		i.MaxOutput = maxTok
	}
	i.InputPerToken, i.OutputPerToken = num("input_cost_per_token"), num("output_cost_per_token")
	i.CacheReadPerToken, i.CacheWritePerToken = num("cache_read_input_token_cost"), num("cache_creation_input_token_cost")
	_, hasIn := f["input_cost_per_token"].(float64)
	_, hasOut := f["output_cost_per_token"].(float64)
	i.Priced = hasIn || hasOut
	t := Tier{Above: 200_000, Input: num("input_cost_per_token_above_200k_tokens"), Output: num("output_cost_per_token_above_200k_tokens"),
		CacheRead: num("cache_read_input_token_cost_above_200k_tokens"), CacheWrite: num("cache_creation_input_token_cost_above_200k_tokens")}
	if t.Input > 0 || t.Output > 0 || t.CacheRead > 0 || t.CacheWrite > 0 {
		i.Tier = &t
	}
	i.Vision, i.Reasoning, i.Tools = flag("supports_vision"), flag("supports_reasoning"), flag("supports_function_calling")
	return i
}

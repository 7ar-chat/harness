package bench

// The hard tier: multi-file features, a bug far from its symptom, a data race, a cross-package refactor,
// repository conventions, a config format change, a misleading README, and a Node task. See tasks_hard2.go.
func hardTasks() []Task {
	ts := []Task{goRetryLayer, goPaginate, goBugElsewhere, goRace, goRefactorNotify, goConventions, goConfigFormat, pyMisleadingReadme, nodeCSV}
	for i := range ts {
		ts[i].Tier = TierHard
	}
	return ts
}

// ---- go-retry-layer: a feature across config, client and transport ----

var goRetryLayer = Task{
	Name: "go-retry-layer",
	Prompt: "Package api has a Client that calls a Transport. Add a retry layer:\n" +
		"- Config gets Retries (int, extra attempts after the first; default 0) and Backoff (func(attempt int) time.Duration, attempt counting from 0; nil means 50ms doubling per attempt).\n" +
		"- Client.Get retries only when the transport error satisfies errors.Is(err, ErrTemporary); any other error is returned at once, unwrapped.\n" +
		"- Waiting between attempts must stop as soon as the context is done, returning the context's error. A cancelled context is never retried.\n" +
		"- When retries run out, return the last error wrapped as \"after N attempts: %w\" so errors.Is still works.\n" +
		"- Client.Stats() returns Stats{Calls, Attempts, Retries int}: Get calls, transport calls, and retries performed; it must be safe for concurrent use.\n" +
		"- ParseConfig reads RETRIES from the map: absent means 0; anything that is not a non-negative integer is an error mentioning RETRIES." + noAsking,
	Needs: []string{"go"},
	Files: map[string]string{
		"go.mod": "module retryfeat\n\ngo 1.21\n",
		"api/transport.go": `package api

import (
	"context"
	"errors"
)

// Transport performs one request.
type Transport interface {
	Do(ctx context.Context, path string) ([]byte, error)
}

// ErrTemporary marks failures worth retrying.
var ErrTemporary = errors.New("temporary failure")

// ErrNotFound is a permanent failure.
var ErrNotFound = errors.New("not found")
`,
		"api/config.go": `package api

import "errors"

// Config configures a Client.
type Config struct {
	BaseURL string
}

// ParseConfig reads settings from a key/value map (for example the environment).
func ParseConfig(kv map[string]string) (Config, error) {
	c := Config{BaseURL: kv["BASE_URL"]}
	if c.BaseURL == "" {
		return c, errors.New("BASE_URL is required")
	}
	return c, nil
}
`,
		"api/client.go": `package api

import "context"

// Client fetches paths through a Transport.
type Client struct {
	cfg Config
	t   Transport
}

// New returns a Client.
func New(cfg Config, t Transport) *Client { return &Client{cfg: cfg, t: t} }

// Get fetches path.
func (c *Client) Get(ctx context.Context, path string) ([]byte, error) {
	return c.t.Do(ctx, path)
}
`,
		"api/client_test.go": `package api

import (
	"context"
	"testing"
)

type fake struct{ body string }

func (f fake) Do(ctx context.Context, path string) ([]byte, error) { return []byte(f.body), nil }

func TestGetReturnsTheBody(t *testing.T) {
	c := New(Config{BaseURL: "x"}, fake{"hi"})
	b, err := c.Get(context.Background(), "/a")
	if err != nil || string(b) != "hi" {
		t.Fatalf("%q %v", b, err)
	}
}
`,
	},
	Hidden: map[string]string{
		"api/client_test.go": `package api

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type script struct {
	mu    sync.Mutex
	errs  []error
	calls int
}

func (s *script) Do(ctx context.Context, path string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.calls
	s.calls++
	if i < len(s.errs) && s.errs[i] != nil {
		return nil, s.errs[i]
	}
	return []byte("ok"), nil
}

func fast(int) time.Duration { return time.Millisecond }

func TestRetriesTemporaryErrorsUntilSuccess(t *testing.T) {
	s := &script{errs: []error{ErrTemporary, ErrTemporary}}
	c := New(Config{BaseURL: "x", Retries: 3, Backoff: fast}, s)
	b, err := c.Get(context.Background(), "/a")
	if err != nil || string(b) != "ok" {
		t.Fatalf("%q %v", b, err)
	}
	if st := c.Stats(); st.Calls != 1 || st.Attempts != 3 || st.Retries != 2 {
		t.Fatalf("stats %+v", st)
	}
}

func TestGivesUpAfterTheRetriesAndKeepsTheCause(t *testing.T) {
	s := &script{errs: []error{ErrTemporary, ErrTemporary, ErrTemporary, ErrTemporary}}
	c := New(Config{BaseURL: "x", Retries: 2, Backoff: fast}, s)
	_, err := c.Get(context.Background(), "/a")
	if !errors.Is(err, ErrTemporary) || err.Error() != "after 3 attempts: "+ErrTemporary.Error() {
		t.Fatalf("err %v", err)
	}
	if s.calls != 3 {
		t.Fatalf("transport called %d times, want 3", s.calls)
	}
}

func TestPermanentErrorsAreNotRetriedNorWrapped(t *testing.T) {
	s := &script{errs: []error{ErrNotFound}}
	c := New(Config{BaseURL: "x", Retries: 5, Backoff: fast}, s)
	_, err := c.Get(context.Background(), "/a")
	if err != ErrNotFound || s.calls != 1 {
		t.Fatalf("err %v calls %d", err, s.calls)
	}
}

func TestZeroRetriesMeansOneAttempt(t *testing.T) {
	s := &script{errs: []error{ErrTemporary}}
	c := New(Config{BaseURL: "x"}, s)
	_, err := c.Get(context.Background(), "/a")
	if err != ErrTemporary || s.calls != 1 {
		t.Fatalf("err %v calls %d", err, s.calls)
	}
}

func TestBackoffReceivesTheAttemptNumber(t *testing.T) {
	var seen []int
	s := &script{errs: []error{ErrTemporary, ErrTemporary, ErrTemporary}}
	c := New(Config{BaseURL: "x", Retries: 3, Backoff: func(a int) time.Duration { seen = append(seen, a); return 0 }}, s)
	if _, err := c.Get(context.Background(), "/a"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 || seen[0] != 0 || seen[1] != 1 || seen[2] != 2 {
		t.Fatalf("backoff attempts %v", seen)
	}
}

func TestCancellingTheContextStopsTheWait(t *testing.T) {
	s := &script{errs: []error{ErrTemporary, ErrTemporary}}
	c := New(Config{BaseURL: "x", Retries: 5, Backoff: func(int) time.Duration { return time.Hour }}, s)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Get(ctx, "/a")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
}

func TestACancelledContextIsNeverAttempted(t *testing.T) {
	s := &script{}
	c := New(Config{BaseURL: "x", Retries: 2, Backoff: fast}, s)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Get(ctx, "/a"); !errors.Is(err, context.Canceled) || s.calls != 0 {
		t.Fatalf("err %v calls %d", err, s.calls)
	}
}

func TestStatsAreSafeForConcurrentUse(t *testing.T) {
	c := New(Config{BaseURL: "x"}, &script{})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, _ = c.Get(context.Background(), "/a")
				_ = c.Stats()
			}
		}()
	}
	wg.Wait()
	if st := c.Stats(); st.Calls != 1000 || st.Attempts != 1000 {
		t.Fatalf("stats %+v", st)
	}
}
`,
		"api/config_test.go": `package api

import "testing"

func TestParseConfigRetries(t *testing.T) {
	for _, c := range []struct {
		in      string
		want    int
		wantErr bool
	}{{"", 0, false}, {"0", 0, false}, {"4", 4, false}, {"-1", 0, true}, {"two", 0, true}, {"1.5", 0, true}} {
		kv := map[string]string{"BASE_URL": "http://x"}
		if c.in != "" {
			kv["RETRIES"] = c.in
		}
		cfg, err := ParseConfig(kv)
		if (err != nil) != c.wantErr {
			t.Errorf("RETRIES=%q: err %v", c.in, err)
			continue
		}
		if err != nil && !contains(err.Error(), "RETRIES") {
			t.Errorf("RETRIES=%q: error %q does not mention RETRIES", c.in, err)
		}
		if err == nil && cfg.Retries != c.want {
			t.Errorf("RETRIES=%q: got %d, want %d", c.in, cfg.Retries, c.want)
		}
	}
	if _, err := ParseConfig(map[string]string{}); err == nil {
		t.Error("a missing BASE_URL must still be an error")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
`,
	},
	Verify: "go vet ./... && go test -count=1 ./...",
	Solution: map[string]string{
		"api/config.go": `package api

import (
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Config configures a Client.
type Config struct {
	BaseURL string
	// Retries is the number of extra attempts after the first.
	Retries int
	// Backoff is the wait before retry number attempt (from 0); nil doubles 50ms.
	Backoff func(attempt int) time.Duration
}

// ParseConfig reads settings from a key/value map (for example the environment).
func ParseConfig(kv map[string]string) (Config, error) {
	c := Config{BaseURL: kv["BASE_URL"]}
	if c.BaseURL == "" {
		return c, errors.New("BASE_URL is required")
	}
	if s, ok := kv["RETRIES"]; ok && s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return c, fmt.Errorf("RETRIES must be a non-negative integer, got %q", s)
		}
		c.Retries = n
	}
	return c, nil
}
`,
		"api/client.go": `package api

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Stats counts what a Client did.
type Stats struct {
	Calls    int
	Attempts int
	Retries  int
}

// Client fetches paths through a Transport.
type Client struct {
	cfg   Config
	t     Transport
	mu    sync.Mutex
	stats Stats
}

// New returns a Client.
func New(cfg Config, t Transport) *Client { return &Client{cfg: cfg, t: t} }

// Stats returns a snapshot of the counters.
func (c *Client) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}

func (c *Client) count(f func(*Stats)) {
	c.mu.Lock()
	f(&c.stats)
	c.mu.Unlock()
}

func (c *Client) backoff(attempt int) time.Duration {
	if c.cfg.Backoff != nil {
		return c.cfg.Backoff(attempt)
	}
	return 50 * time.Millisecond << attempt
}

// Get fetches path, retrying temporary failures.
func (c *Client) Get(ctx context.Context, path string) ([]byte, error) {
	c.count(func(s *Stats) { s.Calls++ })
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c.count(func(s *Stats) { s.Attempts++ })
		b, err := c.t.Do(ctx, path)
		if err == nil {
			return b, nil
		}
		if !errors.Is(err, ErrTemporary) {
			return nil, err
		}
		if attempt >= c.cfg.Retries {
			if attempt == 0 {
				return nil, err
			}
			return nil, fmt.Errorf("after %d attempts: %w", attempt+1, err)
		}
		c.count(func(s *Stats) { s.Retries++ })
		if wait := c.backoff(attempt); wait > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		}
	}
}
`,
	},
}

// ---- go-paginate: cursor pagination through store, service and HTTP handler ----

var goPaginate = Task{
	Name: "go-paginate",
	Prompt: "Add cursor pagination to the items listing, through all three layers:\n" +
		"- store.Store.Page(prefix string, afterID, limit int) []Item returns up to limit items whose Name starts with prefix and whose ID is greater than afterID, in ID order.\n" +
		"- service.Service.Page(prefix, cursor string, limit int) (service.Page, error), Page{Items []store.Item; NextCursor string}. The cursor is the decimal ID of the last item of the previous page (empty: first page). limit <= 0 means 20 and is capped at 100. NextCursor is empty on the last page and non-empty only when more items follow. A cursor that is not a non-negative integer returns service.ErrBadCursor.\n" +
		"- GET /items?prefix=&limit=&cursor= answers JSON {\"items\": [...], \"next_cursor\": \"...\"}; a bad cursor or a limit that is not an integer is a 400.\n" +
		"Keep the existing store.List and service.Search." + noAsking,
	Needs: []string{"go"},
	Files: map[string]string{
		"go.mod": "module pagefeat\n\ngo 1.21\n",
		"store/store.go": `// Package store keeps items in memory.
package store

import (
	"sort"
	"strings"
)

// Item is a stored item.
type Item struct {
	ID   int
	Name string
}

// Store holds items ordered by ID.
type Store struct{ items []Item }

// New returns a Store with a copy of items.
func New(items []Item) *Store {
	c := append([]Item(nil), items...)
	sort.Slice(c, func(i, j int) bool { return c[i].ID < c[j].ID })
	return &Store{items: c}
}

// List returns every item whose Name starts with prefix.
func (s *Store) List(prefix string) []Item {
	var out []Item
	for _, it := range s.items {
		if strings.HasPrefix(it.Name, prefix) {
			out = append(out, it)
		}
	}
	return out
}
`,
		"service/service.go": `// Package service is the business layer over the store.
package service

import "pagefeat/store"

// Service searches items.
type Service struct{ st *store.Store }

// New returns a Service.
func New(st *store.Store) *Service { return &Service{st: st} }

// Search returns every item whose name starts with prefix.
func (s *Service) Search(prefix string) []store.Item { return s.st.List(prefix) }
`,
		"server/server.go": `// Package server exposes the service over HTTP.
package server

import (
	"encoding/json"
	"net/http"

	"pagefeat/service"
)

// Handler returns the HTTP handler.
func Handler(svc *service.Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/items", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(svc.Search(r.URL.Query().Get("prefix")))
	})
	return mux
}
`,
		"server/server_test.go": `package server

import (
	"net/http/httptest"
	"testing"

	"pagefeat/service"
	"pagefeat/store"
)

func TestItemsAreListed(t *testing.T) {
	h := Handler(service.New(store.New([]store.Item{{ID: 1, Name: "a"}})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/items", nil))
	if rec.Code != 200 {
		t.Fatal(rec.Code)
	}
}
`,
	},
	Hidden: map[string]string{
		"store/store_test.go": `package store

import "testing"

func items() []Item {
	return []Item{{5, "pear"}, {1, "apple"}, {3, "apricot"}, {2, "banana"}, {4, "avocado"}}
}

func TestPageWalksInIDOrder(t *testing.T) {
	s := New(items())
	got := s.Page("a", 0, 2)
	if len(got) != 2 || got[0].ID != 1 || got[1].ID != 3 {
		t.Fatalf("first page %v", got)
	}
	got = s.Page("a", 3, 2)
	if len(got) != 1 || got[0].ID != 4 {
		t.Fatalf("second page %v", got)
	}
	if got = s.Page("a", 4, 2); len(got) != 0 {
		t.Fatalf("past the end %v", got)
	}
}

func TestPageWithoutPrefixAndListStillWork(t *testing.T) {
	s := New(items())
	if got := s.Page("", 2, 10); len(got) != 3 || got[0].ID != 3 {
		t.Fatalf("%v", got)
	}
	if got := s.List("ap"); len(got) != 2 {
		t.Fatalf("%v", got)
	}
}
`,
		"service/service_test.go": `package service

import (
	"errors"
	"testing"

	"pagefeat/store"
)

func many(n int) *Service {
	var its []store.Item
	for i := 1; i <= n; i++ {
		its = append(its, store.Item{ID: i * 10, Name: "item"})
	}
	return New(store.New(its))
}

func TestPagesChainThroughTheCursor(t *testing.T) {
	s := many(5)
	p, err := s.Page("item", "", 2)
	if err != nil || len(p.Items) != 2 || p.NextCursor != "20" {
		t.Fatalf("%+v %v", p, err)
	}
	p, err = s.Page("item", p.NextCursor, 2)
	if err != nil || len(p.Items) != 2 || p.Items[0].ID != 30 || p.NextCursor != "40" {
		t.Fatalf("%+v %v", p, err)
	}
	p, err = s.Page("item", p.NextCursor, 2)
	if err != nil || len(p.Items) != 1 || p.NextCursor != "" {
		t.Fatalf("last page %+v %v", p, err)
	}
}

func TestAnExactlyFullLastPageHasNoNextCursor(t *testing.T) {
	p, err := many(4).Page("item", "20", 2)
	if err != nil || len(p.Items) != 2 || p.NextCursor != "" {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestLimitDefaultsAndCap(t *testing.T) {
	s := many(150)
	if p, _ := s.Page("", "", 0); len(p.Items) != 20 {
		t.Fatalf("default limit gave %d", len(p.Items))
	}
	if p, _ := s.Page("", "", 1000); len(p.Items) != 100 {
		t.Fatalf("capped limit gave %d", len(p.Items))
	}
}

func TestBadCursors(t *testing.T) {
	for _, c := range []string{"abc", "-4", "1.5"} {
		if _, err := many(3).Page("", c, 2); !errors.Is(err, ErrBadCursor) {
			t.Errorf("cursor %q: %v", c, err)
		}
	}
}

func TestSearchStillReturnsEverything(t *testing.T) {
	if got := many(30).Search("item"); len(got) != 30 {
		t.Fatalf("%d", len(got))
	}
}
`,
		"server/server_test.go": `package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"pagefeat/service"
	"pagefeat/store"
)

func handler() *httptest.Server {
	var its []store.Item
	for i := 1; i <= 5; i++ {
		its = append(its, store.Item{ID: i, Name: "n"})
	}
	return httptest.NewServer(Handler(service.New(store.New(its))))
}

func get(t *testing.T, srv *httptest.Server, q string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/items"+q, nil)
	srv.Config.Handler.ServeHTTP(rec, req)
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m
}

func TestPaginatedJSON(t *testing.T) {
	srv := handler()
	defer srv.Close()
	code, m := get(t, srv, "?limit=2")
	items, _ := m["items"].([]any)
	if code != 200 || len(items) != 2 || m["next_cursor"] != "2" {
		t.Fatalf("%d %v", code, m)
	}
	code, m = get(t, srv, "?limit=2&cursor=4")
	items, _ = m["items"].([]any)
	if code != 200 || len(items) != 1 {
		t.Fatalf("%d %v", code, m)
	}
	if c, ok := m["next_cursor"]; ok && c != "" {
		t.Fatalf("last page has a cursor: %v", m)
	}
}

func TestBadInputIs400(t *testing.T) {
	srv := handler()
	defer srv.Close()
	for _, q := range []string{"?cursor=zzz", "?limit=many", "?cursor=-1"} {
		if code, _ := get(t, srv, q); code != 400 {
			t.Errorf("%s: status %d", q, code)
		}
	}
}
`,
	},
	Verify: "go vet ./... && go test -count=1 ./...",
	Solution: map[string]string{
		"store/store.go": `// Package store keeps items in memory.
package store

import (
	"sort"
	"strings"
)

// Item is a stored item.
type Item struct {
	ID   int
	Name string
}

// Store holds items ordered by ID.
type Store struct{ items []Item }

// New returns a Store with a copy of items.
func New(items []Item) *Store {
	c := append([]Item(nil), items...)
	sort.Slice(c, func(i, j int) bool { return c[i].ID < c[j].ID })
	return &Store{items: c}
}

// List returns every item whose Name starts with prefix.
func (s *Store) List(prefix string) []Item {
	var out []Item
	for _, it := range s.items {
		if strings.HasPrefix(it.Name, prefix) {
			out = append(out, it)
		}
	}
	return out
}

// Page returns up to limit items matching prefix with an ID greater than afterID.
func (s *Store) Page(prefix string, afterID, limit int) []Item {
	var out []Item
	for _, it := range s.items {
		if len(out) >= limit {
			break
		}
		if it.ID > afterID && strings.HasPrefix(it.Name, prefix) {
			out = append(out, it)
		}
	}
	return out
}
`,
		"service/service.go": `// Package service is the business layer over the store.
package service

import (
	"errors"
	"strconv"

	"pagefeat/store"
)

// ErrBadCursor is returned for a cursor that is not a non-negative integer.
var ErrBadCursor = errors.New("bad cursor")

// Page is one page of items.
type Page struct {
	Items      []store.Item
	NextCursor string
}

// Service searches items.
type Service struct{ st *store.Store }

// New returns a Service.
func New(st *store.Store) *Service { return &Service{st: st} }

// Search returns every item whose name starts with prefix.
func (s *Service) Search(prefix string) []store.Item { return s.st.List(prefix) }

// Page returns the page of items after cursor.
func (s *Service) Page(prefix, cursor string, limit int) (Page, error) {
	if limit <= 0 {
		limit = 20
	}
	limit = min(limit, 100)
	after := 0
	if cursor != "" {
		n, err := strconv.Atoi(cursor)
		if err != nil || n < 0 {
			return Page{}, ErrBadCursor
		}
		after = n
	}
	items := s.st.Page(prefix, after, limit+1)
	p := Page{Items: items}
	if len(items) > limit {
		p.Items = items[:limit]
		p.NextCursor = strconv.Itoa(p.Items[limit-1].ID)
	}
	return p, nil
}
`,
		"server/server.go": `// Package server exposes the service over HTTP.
package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"pagefeat/service"
)

// Handler returns the HTTP handler.
func Handler(svc *service.Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/items", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit := 0
		if s := q.Get("limit"); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil {
				http.Error(w, "bad limit", http.StatusBadRequest)
				return
			}
			limit = n
		}
		p, err := svc.Page(q.Get("prefix"), q.Get("cursor"), limit)
		if errors.Is(err, service.ErrBadCursor) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": p.Items, "next_cursor": p.NextCursor})
	})
	return mux
}
`,
		"server/server_test.go": `package server

import (
	"net/http/httptest"
	"testing"

	"pagefeat/service"
	"pagefeat/store"
)

func TestItemsAreListed(t *testing.T) {
	h := Handler(service.New(store.New([]store.Item{{ID: 1, Name: "a"}})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/items", nil))
	if rec.Code != 200 {
		t.Fatal(rec.Code)
	}
}
`,
	},
}

// ---- go-bug-elsewhere: the symptom is in the report, the cause is in the money package ----

var goBugElsewhere = Task{
	Name:   "go-bug-elsewhere",
	Prompt: "TestRenderTotals in package report fails: an invoice of three items at 19.99 prints a total that is a few cents short. Find the root cause and fix it properly; do not change the tests." + noAsking,
	Needs:  []string{"go"},
	Files: map[string]string{
		"go.mod": "module billing\n\ngo 1.21\n",
		"money/money.go": `// Package money handles amounts in integer cents.
package money

import (
	"fmt"
	"strconv"
	"strings"
)

// Cents is an amount in hundredths of the currency unit.
type Cents int64

// Parse reads a decimal amount such as "19.99" or "-3".
func Parse(s string) (Cents, error) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, fmt.Errorf("money: bad amount %q", s)
	}
	return Cents(f * 100), nil
}

// String formats the amount with two decimals.
func (c Cents) String() string {
	sign := ""
	if c < 0 {
		sign, c = "-", -c
	}
	return fmt.Sprintf("%s%d.%02d", sign, c/100, c%100)
}
`,
		"invoice/invoice.go": `// Package invoice totals invoice lines.
package invoice

import "billing/money"

// Line is one invoice line.
type Line struct {
	Desc  string
	Price string // decimal amount per unit
	Qty   int
}

// Total is the sum of price times quantity over the lines.
func Total(lines []Line) (money.Cents, error) {
	var sum money.Cents
	for _, l := range lines {
		p, err := money.Parse(l.Price)
		if err != nil {
			return 0, err
		}
		sum += p * money.Cents(l.Qty)
	}
	return sum, nil
}

// WithTax adds a tax of ratePct percent, rounding half up.
func WithTax(c money.Cents, ratePct int) money.Cents {
	return c + (c*money.Cents(ratePct)+50)/100
}
`,
		"report/report.go": `// Package report renders invoices as text.
package report

import (
	"fmt"
	"strings"

	"billing/invoice"
)

// Render prints the lines and the total.
func Render(lines []invoice.Line) (string, error) {
	total, err := invoice.Total(lines)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, l := range lines {
		fmt.Fprintf(&b, "%-10s %3d x %s\n", l.Desc, l.Qty, l.Price)
	}
	fmt.Fprintf(&b, "TOTAL %s\n", total)
	return b.String(), nil
}
`,
		"report/report_test.go": `package report

import (
	"strings"
	"testing"

	"billing/invoice"
)

func TestRenderTotals(t *testing.T) {
	out, err := Render([]invoice.Line{{Desc: "widget", Price: "19.99", Qty: 3}})
	if err != nil || !strings.Contains(out, "TOTAL 59.97") {
		t.Fatalf("%q %v", out, err)
	}
}
`,
	},
	Hidden: map[string]string{
		"report/report_test.go": `package report

import (
	"strings"
	"testing"

	"billing/invoice"
)

func TestRenderTotals(t *testing.T) {
	out, err := Render([]invoice.Line{{Desc: "widget", Price: "19.99", Qty: 3}, {Desc: "gum", Price: "0.29", Qty: 1}})
	if err != nil || !strings.Contains(out, "TOTAL 60.26") {
		t.Fatalf("%q %v", out, err)
	}
}
`,
		"money/money_test.go": `package money

import "testing"

func TestParse(t *testing.T) {
	for in, want := range map[string]Cents{
		"19.99": 1999, "0.29": 29, "1.15": 115, "4.35": 435, "0.1": 10, "100": 10000, "-1.15": -115, " 2.5 ": 250, "0.07": 7, "8.2": 820,
	} {
		got, err := Parse(in)
		if err != nil || got != want {
			t.Errorf("Parse(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := Parse("abc"); err == nil {
		t.Error("Parse(abc) must fail")
	}
}

func TestString(t *testing.T) {
	if got := Cents(-5).String(); got != "-0.05" {
		t.Errorf("got %s", got)
	}
}
`,
		"invoice/invoice_test.go": `package invoice

import (
	"testing"

	"billing/money"
)

func TestTotalAndTax(t *testing.T) {
	got, err := Total([]Line{{Price: "1.15", Qty: 3}, {Price: "4.35", Qty: 2}})
	if err != nil || got != 1215 {
		t.Fatalf("Total = %d, %v", got, err)
	}
	if got := WithTax(money.Cents(1000), 20); got != 1200 {
		t.Fatalf("WithTax = %d", got)
	}
	if got := WithTax(money.Cents(1999), 5); got != 2099 {
		t.Fatalf("WithTax = %d", got)
	}
}
`,
	},
	Verify: "go vet ./... && go test -count=1 ./...",
	Solution: map[string]string{
		"money/money.go": `// Package money handles amounts in integer cents.
package money

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Cents is an amount in hundredths of the currency unit.
type Cents int64

// Parse reads a decimal amount such as "19.99" or "-3".
func Parse(s string) (Cents, error) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, fmt.Errorf("money: bad amount %q", s)
	}
	return Cents(math.Round(f * 100)), nil
}

// String formats the amount with two decimals.
func (c Cents) String() string {
	sign := ""
	if c < 0 {
		sign, c = "-", -c
	}
	return fmt.Sprintf("%s%d.%02d", sign, c/100, c%100)
}
`,
	},
}

// ---- go-race: a data race behind a cache with statistics ----

var goRace = Task{
	Name:   "go-race",
	Prompt: "Package cache is used from many goroutines in production and corrupts its statistics (and sometimes crashes with a concurrent map error). Make Cache safe for concurrent use without changing its public API or behaviour. Verify with the race detector (go test -race) if it is available.",
	Needs:  []string{"go"},
	Files: map[string]string{
		"go.mod": "module racefix\n\ngo 1.21\n",
		"cache/cache.go": `// Package cache is a small string cache with hit statistics.
package cache

// Cache maps keys to values and counts lookups.
type Cache struct {
	items  map[string]string
	hits   int
	misses int
}

// New returns an empty Cache.
func New() *Cache { return &Cache{items: map[string]string{}} }

// Set stores v under k.
func (c *Cache) Set(k, v string) { c.items[k] = v }

// Get returns the value of k and whether it was present; every call counts as a hit or a miss.
func (c *Cache) Get(k string) (string, bool) {
	v, ok := c.items[k]
	if ok {
		c.hits++
	} else {
		c.misses++
	}
	return v, ok
}

// Stats returns the number of hits and misses so far.
func (c *Cache) Stats() (hits, misses int) { return c.hits, c.misses }

// Len returns the number of stored keys.
func (c *Cache) Len() int { return len(c.items) }

// Items returns the stored pairs; the caller may keep and modify the result.
func (c *Cache) Items() map[string]string { return c.items }
`,
		"cache/cache_test.go": `package cache

import "testing"

func TestGetSetStats(t *testing.T) {
	c := New()
	c.Set("a", "1")
	if v, ok := c.Get("a"); !ok || v != "1" {
		t.Fatal("miss")
	}
	c.Get("b")
	if h, m := c.Stats(); h != 1 || m != 1 {
		t.Fatalf("%d %d", h, m)
	}
}
`,
	},
	Hidden: map[string]string{
		"cache/cache_test.go": `package cache

import (
	"fmt"
	"sync"
	"testing"
)

func TestGetSetStats(t *testing.T) {
	c := New()
	c.Set("a", "1")
	if v, ok := c.Get("a"); !ok || v != "1" {
		t.Fatal("miss")
	}
	c.Get("b")
	if h, m := c.Stats(); h != 1 || m != 1 {
		t.Fatalf("%d %d", h, m)
	}
}

func TestConcurrentUseKeepsExactStatistics(t *testing.T) {
	c := New()
	const workers, rounds = 16, 3000
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				k := fmt.Sprintf("k%d", i%40)
				if i%3 == 0 {
					c.Set(k, "v")
				}
				c.Get(k)
				_ = c.Len()
			}
		}(w)
	}
	wg.Wait()
	h, m := c.Stats()
	if h+m != workers*rounds {
		t.Fatalf("hits %d + misses %d != %d lookups", h, m, workers*rounds)
	}
}

func TestItemsIsASnapshotSafeToIterateAndModify(t *testing.T) {
	c := New()
	for i := 0; i < 20; i++ {
		c.Set(fmt.Sprint(i), "v")
	}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				c.Set(fmt.Sprint(100+i%30), "x")
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				snap := c.Items()
				for k := range snap {
					snap[k] = "mutated"
				}
				snap["extra"] = "y"
			}
		}()
	}
	wg.Wait()
	for k, v := range c.Items() {
		if v == "mutated" || k == "extra" {
			t.Fatalf("a caller's change leaked into the cache: %s=%s", k, v)
		}
	}
}
`,
	},
	Verify: "F=; if go test -race -run NONE ./... >/dev/null 2>&1; then F=-race; fi; go vet ./... && go test $F -count=3 ./...",
	Solution: map[string]string{
		"cache/cache.go": `// Package cache is a small string cache with hit statistics.
package cache

import "sync"

// Cache maps keys to values and counts lookups. It is safe for concurrent use.
type Cache struct {
	mu     sync.Mutex
	items  map[string]string
	hits   int
	misses int
}

// New returns an empty Cache.
func New() *Cache { return &Cache{items: map[string]string{}} }

// Set stores v under k.
func (c *Cache) Set(k, v string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[k] = v
}

// Get returns the value of k and whether it was present; every call counts as a hit or a miss.
func (c *Cache) Get(k string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.items[k]
	if ok {
		c.hits++
	} else {
		c.misses++
	}
	return v, ok
}

// Stats returns the number of hits and misses so far.
func (c *Cache) Stats() (hits, misses int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses
}

// Len returns the number of stored keys.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// Items returns a copy of the stored pairs; the caller may keep and modify it.
func (c *Cache) Items() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]string, len(c.items))
	for k, v := range c.items {
		out[k] = v
	}
	return out
}
`,
	},
}

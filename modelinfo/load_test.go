package modelinfo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const tinyTable = `{"m1":{"mode":"chat","litellm_provider":"x","max_input_tokens":1000,"input_cost_per_token":1e-6,"output_cost_per_token":2e-6}}`

// priceServer serves body with an ETag and counts the requests; a request whose
// If-None-Match is the ETag gets 304.
type priceServer struct {
	*httptest.Server
	hits, conditional atomic.Int32
	body              string
	status            int
}

func newPriceServer(t *testing.T, body string) *priceServer {
	t.Helper()
	p := &priceServer{body: body, status: http.StatusOK}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.hits.Add(1)
		if p.status != http.StatusOK {
			http.Error(w, "boom", p.status)
			return
		}
		if r.Header.Get("If-None-Match") == `"v1"` {
			p.conditional.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write([]byte(p.body))
	}))
	t.Cleanup(p.Close)
	return p
}

func age(t *testing.T, path string, d time.Duration) {
	t.Helper()
	old := time.Now().Add(-d)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

func TestLoadFetchesAndWritesTheCacheOwnerOnly(t *testing.T) {
	srv := newPriceServer(t, tinyTable)
	dir := t.TempDir()

	tb, err := Load(context.Background(), Options{CacheDir: dir, URL: srv.URL})

	if err != nil || !tb.Ready() {
		t.Fatalf("err %v ready %v", err, tb.Ready())
	}
	if i, ok := tb.Lookup("m1", ""); !ok || i.ContextWindow != 1000 {
		t.Fatalf("m1 %+v %v", i, ok)
	}
	st, err := os.Stat(filepath.Join(dir, "model_prices.json"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("cache %v %v", st, err)
	}
}

func TestLoadUsesAFreshCacheWithoutTheNetwork(t *testing.T) {
	srv := newPriceServer(t, tinyTable)
	dir := t.TempDir()
	if _, err := Load(context.Background(), Options{CacheDir: dir, URL: srv.URL}); err != nil {
		t.Fatal(err)
	}

	tb, err := Load(context.Background(), Options{CacheDir: dir, URL: srv.URL})

	if err != nil || srv.hits.Load() != 1 || tb.Len() != 1 {
		t.Fatalf("err %v hits %d len %d", err, srv.hits.Load(), tb.Len())
	}
}

func TestLoadRevalidatesAStaleCacheWithItsETag(t *testing.T) {
	srv := newPriceServer(t, tinyTable)
	dir := t.TempDir()
	cache := filepath.Join(dir, "model_prices.json")
	if _, err := Load(context.Background(), Options{CacheDir: dir, URL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	age(t, cache, 48*time.Hour)

	tb, err := Load(context.Background(), Options{CacheDir: dir, URL: srv.URL})

	if err != nil || srv.conditional.Load() != 1 || tb.Len() != 1 {
		t.Fatalf("err %v conditional %d len %d", err, srv.conditional.Load(), tb.Len())
	}
	if st, _ := os.Stat(cache); time.Since(st.ModTime()) > time.Hour {
		t.Fatal("a 304 did not renew the cache")
	}
}

func TestLoadReplacesAStaleCacheWhenTheTableChanged(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "model_prices.json")
	if err := os.WriteFile(cache, []byte(`{"old":{"mode":"chat","max_input_tokens":1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	age(t, cache, 48*time.Hour)
	srv := newPriceServer(t, tinyTable)

	tb, err := Load(context.Background(), Options{CacheDir: dir, URL: srv.URL})

	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tb.Lookup("m1", ""); !ok {
		t.Fatal("new table not used")
	}
	if b, _ := os.ReadFile(cache); !strings.Contains(string(b), "m1") {
		t.Fatalf("cache not rewritten: %s", b)
	}
}

func TestLoadFallsBackToTheStaleCacheWhenTheNetworkFails(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "model_prices.json")
	if err := os.WriteFile(cache, []byte(tinyTable), 0o600); err != nil {
		t.Fatal(err)
	}
	age(t, cache, 72*time.Hour)
	srv := newPriceServer(t, "")
	srv.status = http.StatusInternalServerError

	tb, err := Load(context.Background(), Options{CacheDir: dir, URL: srv.URL})

	if err != nil || tb.Len() != 1 {
		t.Fatalf("err %v len %d", err, tb.Len())
	}
}

func TestLoadWithNothingReturnsAnEmptyTableAndAnError(t *testing.T) {
	srv := newPriceServer(t, "")
	srv.status = http.StatusBadGateway

	tb, err := Load(context.Background(), Options{CacheDir: t.TempDir(), URL: srv.URL})

	if err == nil || tb == nil || tb.Len() != 0 || !tb.Ready() {
		t.Fatalf("err %v table %+v", err, tb)
	}
	if _, ok := tb.Lookup("m1", ""); ok {
		t.Fatal("empty table answered")
	}
}

func TestLoadOfflineNeverAsksTheNetworkAndAcceptsAnyAge(t *testing.T) {
	srv := newPriceServer(t, tinyTable)
	dir := t.TempDir()
	cache := filepath.Join(dir, "model_prices.json")
	if err := os.WriteFile(cache, []byte(tinyTable), 0o600); err != nil {
		t.Fatal(err)
	}
	age(t, cache, 400*time.Hour)

	tb, err := Load(context.Background(), Options{CacheDir: dir, URL: srv.URL, Offline: true})
	_, errNone := Load(context.Background(), Options{CacheDir: t.TempDir(), URL: srv.URL, Offline: true})

	if err != nil || tb.Len() != 1 || srv.hits.Load() != 0 || errNone == nil {
		t.Fatalf("err %v len %d hits %d errNone %v", err, tb.Len(), srv.hits.Load(), errNone)
	}
}

func TestLoadWithoutACacheDirKeepsTheTableInMemory(t *testing.T) {
	srv := newPriceServer(t, tinyTable)

	tb, err := Load(context.Background(), Options{URL: srv.URL})

	if err != nil || tb.Len() != 1 {
		t.Fatalf("err %v len %d", err, tb.Len())
	}
}

func TestLoadRefusesATableOverTheSizeCapAndJunk(t *testing.T) {
	big := newPriceServer(t, strings.Repeat(" ", maxBytes+1))
	junk := newPriceServer(t, "<html>captive portal</html>")
	dir := t.TempDir()

	_, errBig := Load(context.Background(), Options{URL: big.URL})
	_, errJunk := Load(context.Background(), Options{CacheDir: dir, URL: junk.URL})

	if errBig == nil || errJunk == nil {
		t.Fatalf("big %v junk %v", errBig, errJunk)
	}
	if _, err := os.Stat(filepath.Join(dir, "model_prices.json")); err == nil {
		t.Fatal("junk was cached")
	}
}

func TestLoadSendsAPlainGet(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		_, _ = w.Write([]byte(tinyTable))
	}))
	defer srv.Close()

	if _, err := Load(context.Background(), Options{URL: srv.URL}); err != nil {
		t.Fatal(err)
	}

	for _, h := range []string{"Authorization", "Cookie", "X-Api-Key", "If-None-Match"} {
		if got.Header.Get(h) != "" {
			t.Fatalf("header %s sent", h)
		}
	}
	if got.Method != http.MethodGet {
		t.Fatalf("method %s", got.Method)
	}
}

func TestOverrideFileWinsPerModel(t *testing.T) {
	srv := newPriceServer(t, tinyTable)
	override := filepath.Join(t.TempDir(), "model-prices.json")
	body := `{"m1":{"max_input_tokens":42,"input_cost_per_token":0,"output_cost_per_token":0},"private":{"max_input_tokens":8000,"input_cost_per_token":1e-6,"output_cost_per_token":3e-6}}`
	if err := os.WriteFile(override, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	tb, err := Load(context.Background(), Options{URL: srv.URL, Override: override})

	if err != nil {
		t.Fatal(err)
	}
	if i, _ := tb.Lookup("m1", ""); i.ContextWindow != 42 || i.InputPerToken != 0 {
		t.Fatalf("m1 %+v", i)
	}
	if _, ok := tb.Lookup("private", ""); !ok {
		t.Fatal("private model missing")
	}
}

func TestMissingOverrideIsNotAnError(t *testing.T) {
	srv := newPriceServer(t, tinyTable)

	_, err := Load(context.Background(), Options{URL: srv.URL, Override: filepath.Join(t.TempDir(), "absent.json")})

	if err != nil {
		t.Fatal(err)
	}
}

func TestFileSourceReplacesTheURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mine.json")
	if err := os.WriteFile(path, []byte(tinyTable), 0o600); err != nil {
		t.Fatal(err)
	}

	tb, err := Load(context.Background(), Options{File: path, URL: "http://127.0.0.1:1/unused"})

	if err != nil || tb.Len() != 1 {
		t.Fatalf("err %v len %d", err, tb.Len())
	}
}

func TestLoadAsyncReturnsAtOnceAndBecomesReady(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = w.Write([]byte(tinyTable))
	}))
	defer srv.Close()

	tb := LoadAsync(context.Background(), Options{URL: srv.URL})
	early := tb.Ready()
	close(release)

	deadline := time.Now().Add(5 * time.Second)
	for !tb.Ready() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if early || !tb.Ready() || tb.Len() != 1 {
		t.Fatalf("early %v ready %v len %d", early, tb.Ready(), tb.Len())
	}
}

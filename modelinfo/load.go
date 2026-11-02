package modelinfo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Defaults of Options.
const (
	// DefaultURL is LiteLLM's public price table.
	DefaultURL = "https://raw.githubusercontent.com/BerriAI/litellm/refs/heads/main/model_prices_and_context_window.json"
	// DefaultTTL is how long the cached copy is used without asking again.
	DefaultTTL = 24 * time.Hour

	cacheFile   = "model_prices.json"
	etagFile    = "model_prices.etag"
	fetchTimout = 8 * time.Second
	maxBytes    = 20 << 20
)

// Options configure Load.
type Options struct {
	// CacheDir holds the downloaded table (model_prices.json) and its ETag; ""
	// keeps nothing on disk (the table is fetched again at every start).
	CacheDir string
	// URL replaces DefaultURL.
	URL string
	// File, when set, is a local table read instead of the URL (and never cached).
	File string
	// Override is a user file in the same schema; its models win over the table's.
	Override string
	// TTL is the age under which the cache is used as is (default DefaultTTL).
	TTL time.Duration
	// HTTPClient performs the request (default http.DefaultClient).
	HTTPClient *http.Client
	// Offline makes no request: the cache, however old, is all there is.
	Offline bool
}

// Load reads the table: the cache when fresh, else the URL (a conditional GET
// that sends nothing but the request itself), else the stale cache. With none
// of them the table is empty and the error says why; callers may ignore it, a
// table without data answers every Lookup with false.
func Load(ctx context.Context, o Options) (*Table, error) {
	t := &Table{}
	err := t.load(ctx, o)
	return t, err
}

// LoadAsync returns the table at once and fills it in the background; Ready
// reports when it is done. Starting never waits for the network.
func LoadAsync(ctx context.Context, o Options) *Table {
	t := &Table{}
	go func() { _ = t.load(ctx, o) }()
	return t
}

func (t *Table) load(ctx context.Context, o Options) error {
	defer t.ready.Store(true)
	var data []byte
	var err error
	if o.File != "" {
		data, err = os.ReadFile(o.File)
	} else {
		data, err = fetchCached(ctx, o)
	}
	var errs []error
	if err == nil {
		var entries map[string]Info
		if entries, err = Parse(data); err == nil {
			t.Set(entries)
		}
	}
	if err != nil {
		errs = append(errs, err)
	}
	if o.Override != "" {
		switch data, oerr := os.ReadFile(o.Override); {
		case errors.Is(oerr, os.ErrNotExist):
		case oerr != nil:
			errs = append(errs, oerr)
		default:
			entries, perr := Parse(data)
			if perr != nil {
				errs = append(errs, fmt.Errorf("%s: %w", o.Override, perr))
				break
			}
			t.Set(entries)
		}
	}
	return errors.Join(errs...)
}

// fetchCached returns the table JSON from the cache or the network.
func fetchCached(ctx context.Context, o Options) ([]byte, error) {
	ttl := o.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	var path, etagPath string
	if o.CacheDir != "" {
		path, etagPath = filepath.Join(o.CacheDir, cacheFile), filepath.Join(o.CacheDir, etagFile)
	}
	var cached []byte
	var age time.Duration
	if path != "" {
		if st, err := os.Stat(path); err == nil {
			if b, err := os.ReadFile(path); err == nil {
				cached, age = b, time.Since(st.ModTime())
			}
		}
	}
	if cached != nil && (age < ttl || o.Offline) {
		return cached, nil
	}
	if o.Offline {
		return nil, errors.New("modelinfo: offline and no cached table")
	}
	body, etag, notModified, err := get(ctx, o, etagFor(etagPath, cached))
	switch {
	case err != nil && cached != nil:
		return cached, nil // stale beats nothing
	case err != nil:
		return nil, err
	case notModified:
		now := time.Now()
		_ = os.Chtimes(path, now, now)
		return cached, nil
	}
	if _, err := Parse(body); err != nil { // never replace a good cache with junk
		if cached != nil {
			return cached, nil
		}
		return nil, err
	}
	if path != "" {
		if err := writeAtomic(path, body); err == nil && etag != "" {
			_ = writeAtomic(etagPath, []byte(etag))
		}
	}
	return body, nil
}

// etagFor is the stored ETag, only when the cached table it belongs to exists.
func etagFor(path string, cached []byte) string {
	if path == "" || cached == nil {
		return ""
	}
	b, _ := os.ReadFile(path)
	return strings.TrimSpace(string(b))
}

func get(ctx context.Context, o Options, etag string) (body []byte, newETag string, notModified bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimout)
	defer cancel()
	url := o.URL
	if url == "" {
		url = DefaultURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", false, err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	hc := o.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, "", false, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotModified && etag != "":
		return nil, etag, true, nil
	case resp.StatusCode != http.StatusOK:
		return nil, "", false, fmt.Errorf("modelinfo: %s: %s", url, resp.Status)
	}
	body, err = io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, "", false, err
	}
	if len(body) > maxBytes {
		return nil, "", false, fmt.Errorf("modelinfo: %s: more than %d MB", url, maxBytes>>20)
	}
	return body, resp.Header.Get("ETag"), false, nil
}

// writeAtomic replaces path with data (mode 0600) through a temporary file.
func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".model_prices-*")
	if err != nil {
		return err
	}
	name := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(name)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

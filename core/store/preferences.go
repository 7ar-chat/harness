package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// MaxPreferencesBytes bounds one person's preferences document (its JSON, all keys together).
const MaxPreferencesBytes = 8 << 10

// ErrPreferencesTooLarge: the document would be bigger than MaxPreferencesBytes after the change.
var ErrPreferencesTooLarge = errors.New("store: preferences are too large")

// ErrPreferencesKey: a key is empty, too long or has characters outside a-z, 0-9, dot, dash and underscore.
var ErrPreferencesKey = errors.New("store: invalid preferences key")

var prefKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// UserPreferences is a person's own small settings document: what the interface remembers for them across browsers (where
// they are in the getting-started steps, which explorer sections are folded, panel sizes). Values are JSON; UpdatedAt is
// when the document last changed (zero when it never did).
type UserPreferences struct {
	Values    map[string]json.RawMessage `json:"values"`
	UpdatedAt time.Time                  `json:"updatedAt"`
}

// MergePreferences applies patch to cur and returns the new values: a key set to JSON null (or absent after the merge)
// is removed, any other key is replaced. It returns ErrPreferencesKey or ErrPreferencesTooLarge, leaving cur untouched.
// Every store uses it so they agree on what a patch means.
func MergePreferences(cur, patch map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	out := make(map[string]json.RawMessage, len(cur)+len(patch))
	for k, v := range cur {
		out[k] = v
	}
	for k, v := range patch {
		if !prefKeyRe.MatchString(k) {
			return nil, fmt.Errorf("%w: %q", ErrPreferencesKey, k)
		}
		if !json.Valid(v) {
			return nil, fmt.Errorf("%w: the value of %q is not JSON", ErrPreferencesKey, k)
		}
		var c bytes.Buffer // stored compact, so every store reads back the same bytes
		if err := json.Compact(&c, v); err != nil {
			return nil, fmt.Errorf("%w: the value of %q is not JSON", ErrPreferencesKey, k)
		}
		if c.String() == "null" {
			delete(out, k)
			continue
		}
		out[k] = c.Bytes()
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	if len(b) > MaxPreferencesBytes {
		return nil, ErrPreferencesTooLarge
	}
	return out, nil
}

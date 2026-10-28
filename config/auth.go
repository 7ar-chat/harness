package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

// The credentials file ~/.har/auth.json:
//
//	{"providers": {"<name>": {"api_key": "..."}}}
//
// It is the only file API keys are allowed in. It must not be readable by the
// group or others (mode 0600); a file that is refuses to be used.

type authDoc struct {
	Providers map[string]authEntry `json:"providers"`
}

type authEntry struct {
	APIKey string `json:"api_key"`
}

// AuthPath is the credentials file of the user directory dir.
func AuthPath(dir string) string { return filepath.Join(dir, AuthFile) }

var providerName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidProviderName reports whether name can name a provider.
func ValidProviderName(name string) bool { return providerName.MatchString(name) }

// MaskKey shows the last 4 characters of key ("****" when it is too short to show any).
func MaskKey(key string) string {
	if len(key) < 12 {
		return "****"
	}
	return "****" + key[len(key)-4:]
}

// checkAuthMode refuses a credentials file the group or others can read.
func checkAuthMode(file string, info os.FileInfo) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s is readable by other users (mode %04o): refusing to use it; run: chmod 600 %s", file, info.Mode().Perm(), file)
	}
	return nil
}

// ReadAuth returns the API keys of the credentials file in dir by provider name.
// A missing file is no error (an empty map); a file with a loose mode, or one
// that is not valid, is, and the error names the file and never holds a key.
func ReadAuth(dir string) (map[string]string, error) {
	out := map[string]string{}
	if dir == "" {
		return out, nil
	}
	file := AuthPath(dir)
	info, err := os.Stat(file)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	if err := checkAuthMode(file, info); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	var doc authDoc
	if len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("%s: not valid JSON (expected {\"providers\": {\"<name>\": {\"api_key\": \"...\"}}}): %v", file, jsonErrorKind(err))
		}
	}
	for name, e := range doc.Providers {
		out[name] = e.APIKey
	}
	return out, nil
}

// jsonErrorKind describes a decoding error without echoing any of the input.
func jsonErrorKind(err error) string {
	var se *json.SyntaxError
	if errors.As(err, &se) {
		return fmt.Sprintf("syntax error at byte %d", se.Offset)
	}
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) {
		return "wrong type for " + te.Field
	}
	return "invalid content"
}

// SetAuthKey stores the key of a provider in the credentials file of dir,
// creating the directory (0700) and the file (0600) when needed.
func SetAuthKey(dir, provider, key string) error {
	if !ValidProviderName(provider) {
		return fmt.Errorf("invalid provider name %q (letters, digits, '.', '_' and '-')", provider)
	}
	if strings.TrimSpace(key) == "" {
		return errors.New("empty API key")
	}
	keys, err := ReadAuth(dir)
	if err != nil {
		return err
	}
	keys[provider] = strings.TrimSpace(key)
	return writeAuth(dir, keys)
}

// RemoveAuthKey deletes the key of a provider; it reports whether there was one.
func RemoveAuthKey(dir, provider string) (bool, error) {
	keys, err := ReadAuth(dir)
	if err != nil {
		return false, err
	}
	if _, ok := keys[provider]; !ok {
		return false, nil
	}
	delete(keys, provider)
	return true, writeAuth(dir, keys)
}

// AuthProviders lists the provider names that have a key, sorted, with the keys.
func AuthProviders(keys map[string]string) []string {
	names := make([]string, 0, len(keys))
	for n := range keys {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// writeAuth replaces the file atomically (temp file in the same directory,
// then rename), always mode 0600.
func writeAuth(dir string, keys map[string]string) error {
	doc := authDoc{Providers: map[string]authEntry{}}
	for n, k := range keys {
		doc.Providers[n] = authEntry{APIKey: k}
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return writePrivateFile(dir, AuthFile, data)
}

// writePrivateFile writes dir/name atomically (temp file in dir, then rename),
// mode 0600, creating dir (0700) when needed.
func writePrivateFile(dir, name string, data []byte) error {
	if dir == "" {
		return errors.New("no user directory (set HAR_HOME, or unset HARNESS_NO_USER_DIR)")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		cleanup()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

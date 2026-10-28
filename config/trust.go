package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The trust file ~/.har/trust.json lists the project roots the user trusts:
//
//	{"projects": ["/abs/path/to/repo", ...]}
//
// A trusted project may add permission `allow` rules and bring its own MCP
// servers; an untrusted one can only select among what the user configured and
// tighten permissions. Like auth.json it must not be readable by the group or
// others, and it is rewritten atomically.

// TrustEnv trusts the current project for one process (CI, scripts) when it has
// a true value ("1", "true", ...).
const TrustEnv = "HARNESS_TRUST_PROJECT"

// TrustFile is the name of the trust file in the user directory.
const TrustFile = "trust.json"

type trustDoc struct {
	Projects []string `json:"projects"`
}

// TrustPath is the trust file of the user directory dir.
func TrustPath(dir string) string { return filepath.Join(dir, TrustFile) }

// CanonicalProject makes a project root comparable: absolute, symbolic links resolved when it exists.
func CanonicalProject(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		abs = r
	}
	return filepath.Clean(abs), nil
}

// ReadTrust returns the trusted project roots of dir. A missing file is no
// error; a file with a loose mode or invalid content is.
func ReadTrust(dir string) ([]string, error) {
	if dir == "" {
		return nil, nil
	}
	file := TrustPath(dir)
	info, err := os.Stat(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
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
	var doc trustDoc
	if len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("%s: not valid JSON (expected {\"projects\": [\"/abs/path\"]}): %v", file, jsonErrorKind(err))
		}
	}
	return doc.Projects, nil
}

// IsTrusted reports whether root is listed in the trust file of dir.
func IsTrusted(dir, root string) (bool, error) {
	list, err := ReadTrust(dir)
	if err != nil || len(list) == 0 || root == "" {
		return false, err
	}
	want, err := CanonicalProject(root)
	if err != nil {
		return false, err
	}
	for _, p := range list {
		if c, err := CanonicalProject(p); err == nil && c == want {
			return true, nil
		}
	}
	return false, nil
}

// AddTrust lists root in the trust file of dir; it reports whether it was added (false: already listed).
func AddTrust(dir, root string) (bool, error) {
	c, err := CanonicalProject(root)
	if err != nil {
		return false, err
	}
	list, err := ReadTrust(dir)
	if err != nil {
		return false, err
	}
	for _, p := range list {
		if p == c {
			return false, nil
		}
	}
	return true, writeTrust(dir, append(list, c))
}

// RemoveTrust removes root from the trust file of dir; it reports whether it was listed.
func RemoveTrust(dir, root string) (bool, error) {
	c, err := CanonicalProject(root)
	if err != nil {
		return false, err
	}
	list, err := ReadTrust(dir)
	if err != nil {
		return false, err
	}
	var kept []string
	found := false
	for _, p := range list {
		if pc, err := CanonicalProject(p); p == c || (err == nil && pc == c) {
			found = true
			continue
		}
		kept = append(kept, p)
	}
	if !found {
		return false, nil
	}
	return true, writeTrust(dir, kept)
}

func writeTrust(dir string, list []string) error {
	sort.Strings(list)
	if list == nil {
		list = []string{}
	}
	data, err := json.MarshalIndent(trustDoc{Projects: list}, "", "  ")
	if err != nil {
		return err
	}
	return writePrivateFile(dir, TrustFile, data)
}

// trustedFromEnv reports a true HARNESS_TRUST_PROJECT.
func trustedFromEnv(get func(string) string) bool {
	switch strings.ToLower(strings.TrimSpace(get(TrustEnv))) {
	case "", "0", "false", "no", "off":
		return false
	}
	return true
}

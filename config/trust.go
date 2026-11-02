package config

import (
	"crypto/sha256"
	"encoding/hex"
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
// tighten permissions. A stdio MCP server of a project (a program to run) also needs its own approval, kept in
// the "mcp" list of the same file and bound to the hash of its command (MCPFingerprint). Like auth.json it must not be readable by the group or
// others, and it is rewritten atomically.

// TrustEnv trusts the current project for one process (CI, scripts) when it has
// a true value ("1", "true", ...).
const TrustEnv = "HARNESS_TRUST_PROJECT"

// TrustFile is the name of the trust file in the user directory.
const TrustFile = "trust.json"

type trustDoc struct {
	Projects []string   `json:"projects"`
	MCP      []MCPTrust `json:"mcp,omitempty"`
}

// MCPTrust records that the user approved running one stdio MCP server that a project defines: the project
// root, the server name and the SHA-256 of its command and arguments (MCPFingerprint). Changing the command
// or an argument no longer matches, so the server must be approved again.
type MCPTrust struct {
	Project string `json:"project"`
	Server  string `json:"server"`
	Hash    string `json:"command_sha256"`
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
	doc, err := readTrustDoc(dir)
	return doc.Projects, err
}

// readTrustDoc reads the whole trust file (projects and approved MCP servers).
func readTrustDoc(dir string) (trustDoc, error) {
	var doc trustDoc
	if dir == "" {
		return doc, nil
	}
	file := TrustPath(dir)
	info, err := os.Stat(file)
	if errors.Is(err, os.ErrNotExist) {
		return doc, nil
	}
	if err != nil {
		return doc, fmt.Errorf("%s: %w", file, err)
	}
	if err := checkAuthMode(file, info); err != nil {
		return doc, err
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return doc, fmt.Errorf("%s: %w", file, err)
	}
	if len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &doc); err != nil {
			return trustDoc{}, fmt.Errorf("%s: not valid JSON (expected {\"projects\": [\"/abs/path\"]}): %v", file, jsonErrorKind(err))
		}
	}
	return doc, nil
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
	doc, err := readTrustDoc(dir)
	if err != nil {
		return false, err
	}
	for _, p := range doc.Projects {
		if p == c {
			return false, nil
		}
	}
	doc.Projects = append(doc.Projects, c)
	return true, writeTrust(dir, doc)
}

// RemoveTrust removes root from the trust file of dir; it reports whether it was listed.
func RemoveTrust(dir, root string) (bool, error) {
	c, err := CanonicalProject(root)
	if err != nil {
		return false, err
	}
	doc, err := readTrustDoc(dir)
	if err != nil {
		return false, err
	}
	var kept []string
	found := false
	for _, p := range doc.Projects {
		if pc, err := CanonicalProject(p); p == c || (err == nil && pc == c) {
			found = true
			continue
		}
		kept = append(kept, p)
	}
	if !found {
		return false, nil
	}
	doc.Projects = kept
	var mcp []MCPTrust
	for _, t := range doc.MCP { // the approvals of a project the user stops trusting go with it
		if t.Project != c {
			mcp = append(mcp, t)
		}
	}
	doc.MCP = mcp
	return true, writeTrust(dir, doc)
}

func writeTrust(dir string, doc trustDoc) error {
	sort.Strings(doc.Projects)
	if doc.Projects == nil {
		doc.Projects = []string{}
	}
	sort.Slice(doc.MCP, func(i, j int) bool {
		a, b := doc.MCP[i], doc.MCP[j]
		return a.Project+"\x00"+a.Server < b.Project+"\x00"+b.Server
	})
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return writePrivateFile(dir, TrustFile, data)
}

// MCPFingerprint identifies what a stdio MCP server runs: the SHA-256 of its command, arguments and environment
// (the environment can redirect a program, e.g. NODE_OPTIONS). The name and the project are recorded beside it.
func MCPFingerprint(s MCPServer) string {
	h := sha256.New()
	field := func(v string) { fmt.Fprintf(h, "%d:%s;", len(v), v) }
	field(s.Command)
	for _, a := range s.Args {
		field(a)
	}
	field("env")
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		field(k)
		field(s.Env[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// MCPApproved reports whether the user approved this exact stdio server of the project root (name, command,
// arguments and environment unchanged since the approval).
func MCPApproved(dir, root string, s MCPServer) (bool, error) {
	doc, err := readTrustDoc(dir)
	if err != nil || len(doc.MCP) == 0 || root == "" {
		return false, err
	}
	want, err := CanonicalProject(root)
	if err != nil {
		return false, err
	}
	hash := MCPFingerprint(s)
	for _, t := range doc.MCP {
		if t.Server == s.Name && t.Hash == hash {
			if c, err := CanonicalProject(t.Project); err == nil && c == want {
				return true, nil
			}
		}
	}
	return false, nil
}

// ApproveMCP records the approval of a project's stdio server, replacing an earlier approval of the same name
// (whose command may have been different); it reports whether the file changed.
func ApproveMCP(dir, root string, s MCPServer) (bool, error) {
	c, err := CanonicalProject(root)
	if err != nil {
		return false, err
	}
	doc, err := readTrustDoc(dir)
	if err != nil {
		return false, err
	}
	hash := MCPFingerprint(s)
	var kept []MCPTrust
	for _, t := range doc.MCP {
		if t.Project == c && t.Server == s.Name {
			if t.Hash == hash {
				return false, nil
			}
			continue
		}
		kept = append(kept, t)
	}
	doc.MCP = append(kept, MCPTrust{Project: c, Server: s.Name, Hash: hash})
	return true, writeTrust(dir, doc)
}

// RevokeMCP removes the approval of the project's stdio server of that name; it reports whether one was listed.
func RevokeMCP(dir, root, server string) (bool, error) {
	c, err := CanonicalProject(root)
	if err != nil {
		return false, err
	}
	doc, err := readTrustDoc(dir)
	if err != nil {
		return false, err
	}
	var kept []MCPTrust
	found := false
	for _, t := range doc.MCP {
		if t.Project == c && t.Server == server {
			found = true
			continue
		}
		kept = append(kept, t)
	}
	if !found {
		return false, nil
	}
	doc.MCP = kept
	return true, writeTrust(dir, doc)
}

// trustedFromEnv reports a true HARNESS_TRUST_PROJECT.
func trustedFromEnv(get func(string) string) bool {
	switch strings.ToLower(strings.TrimSpace(get(TrustEnv))) {
	case "", "0", "false", "no", "off":
		return false
	}
	return true
}

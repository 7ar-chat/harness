// Package swebench turns SWE-bench instances (https://www.swebench.com) into bench tasks and scores
// the resulting repositories the way SWE-bench does.
//
// The agent sees the repository at base_commit WITHOUT the instance's test_patch and is asked to fix
// the issue. The test files the test_patch touches are Hidden: bench.Check writes their post-patch
// contents over the repository before Verify runs, so an agent cannot pass by weakening the tests.
// Evaluate does the same for a git working tree outside bench (a factory worker's checkout):
// it restores the touched test files from base_commit, applies test_patch, runs the FAIL_TO_PASS and
// PASS_TO_PASS tests and reports an instance as resolved only when every F2P test passes and no P2P
// test breaks.
//
// bench.Task has no per-task setup hook, so Suite materialises each repository once and embeds it in
// Task.Files. That is only sensible for small repositories (Options.MaxRepoBytes, default 2 MiB of
// UTF-8 text); larger ones get a setup script the agent runs instead, see Suite.
package swebench

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// Instance is one SWE-bench task instance.
type Instance struct {
	ID               string
	Repo             string // "owner/name"
	BaseCommit       string
	ProblemStatement string
	// Patch is the gold fix; it is never shown to the agent.
	Patch string
	// TestPatch adds or changes the tests that decide the instance.
	TestPatch              string
	FailToPass, PassToPass []string // pytest node ids
	EnvironmentSetupCommit string
	Version                string
	HintsText              string
}

// record mirrors one JSONL line. Encoding/json matches field names case-insensitively, so exports
// that lower-case FAIL_TO_PASS decode too.
type record struct {
	ID                     string          `json:"instance_id"`
	Repo                   string          `json:"repo"`
	BaseCommit             string          `json:"base_commit"`
	ProblemStatement       string          `json:"problem_statement"`
	Patch                  string          `json:"patch"`
	TestPatch              string          `json:"test_patch"`
	FailToPass             testList        `json:"FAIL_TO_PASS"`
	PassToPass             testList        `json:"PASS_TO_PASS"`
	EnvironmentSetupCommit string          `json:"environment_setup_commit"`
	Version                json.RawMessage `json:"version"`
	HintsText              string          `json:"hints_text"`
}

// testList is a list of node ids stored either as a JSON array or as a JSON string holding one
// (the Hugging Face dataset uses the latter).
type testList []string

func (l *testList) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if bytes.Equal(b, []byte("null")) {
		*l = nil
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if strings.TrimSpace(s) == "" {
			*l = nil
			return nil
		}
		b = []byte(s)
	}
	var out []string
	if err := json.Unmarshal(b, &out); err != nil {
		return fmt.Errorf("want an array of test ids or a JSON-encoded one: %w", err)
	}
	*l = out
	return nil
}

var (
	repoRe   = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	commitRe = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)
)

// Load reads SWE-bench instances from JSONL, one object per line (blank lines are skipped). Required:
// instance_id, repo, base_commit, problem_statement, test_patch and a non-empty FAIL_TO_PASS;
// instance ids must be unique. Errors name the 1-based line.
func Load(r io.Reader) ([]Instance, error) {
	br := bufio.NewReaderSize(r, 1<<20) // lines carry whole patches: no Scanner token limit
	var out []Instance
	seen := map[string]int{}
	for n := 1; ; n++ {
		line, err := br.ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("swebench: line %d: %w", n, err)
		}
		if len(bytes.TrimSpace(line)) > 0 {
			inst, perr := parse(line)
			if perr != nil {
				return nil, fmt.Errorf("swebench: line %d: %w", n, perr)
			}
			if first, dup := seen[inst.ID]; dup {
				return nil, fmt.Errorf("swebench: line %d: duplicate instance_id %q (first on line %d)", n, inst.ID, first)
			}
			seen[inst.ID] = n
			out = append(out, inst)
		}
		if err != nil {
			return out, nil
		}
	}
}

func parse(line []byte) (Instance, error) {
	var rec record
	if err := json.Unmarshal(line, &rec); err != nil {
		return Instance{}, err
	}
	var missing []string
	for _, f := range []struct{ name, v string }{
		{"instance_id", rec.ID}, {"repo", rec.Repo}, {"base_commit", rec.BaseCommit},
		{"problem_statement", rec.ProblemStatement}, {"test_patch", rec.TestPatch},
	} {
		if strings.TrimSpace(f.v) == "" {
			missing = append(missing, f.name)
		}
	}
	if len(rec.FailToPass) == 0 {
		missing = append(missing, "FAIL_TO_PASS")
	}
	if len(missing) > 0 {
		return Instance{}, fmt.Errorf("missing required field(s): %s", strings.Join(missing, ", "))
	}
	// Both end up in git arguments and URLs: refuse anything that could be read as an option or a path escape.
	if !repoRe.MatchString(rec.Repo) || strings.Contains(rec.Repo, "..") {
		return Instance{}, fmt.Errorf("repo %q is not of the form owner/name", rec.Repo)
	}
	if !commitRe.MatchString(rec.BaseCommit) {
		return Instance{}, fmt.Errorf("base_commit %q is not a commit hash", rec.BaseCommit)
	}
	version := strings.TrimSpace(string(rec.Version))
	if strings.HasPrefix(version, `"`) {
		var s string
		if err := json.Unmarshal(rec.Version, &s); err != nil {
			return Instance{}, fmt.Errorf("version: %w", err)
		}
		version = s
	} else if version == "null" {
		version = ""
	}
	return Instance{
		ID: rec.ID, Repo: rec.Repo, BaseCommit: rec.BaseCommit,
		ProblemStatement: rec.ProblemStatement, Patch: rec.Patch, TestPatch: rec.TestPatch,
		FailToPass: rec.FailToPass, PassToPass: rec.PassToPass,
		EnvironmentSetupCommit: rec.EnvironmentSetupCommit, Version: version, HintsText: rec.HintsText,
	}, nil
}

// Package bench runs coding tasks through the real harness (local sandbox,
// coding profile) against a model and reports pass/fail with the cost: steps,
// tool calls, tokens, prompt-cache hits, wall time and memory.
//
// A Task is a small repository (Files), a prompt, and a check that decides
// whether the job was done: Verify, a shell command run in the repository
// after the agent finished (exit 0 = pass), after Hidden files — the original
// tests — are written back so an agent cannot pass by weakening them; and/or
// Answer, regular expressions that must all match the final answer (questions
// about the code). Solution is a reference fix: the offline tests apply it to
// prove each task is solvable and fails before the fix.
//
// Offline (scripted model) the package is a regular test; against a live model
// use cmd/bench or `go test -tags live ./bench`.
package bench

import "time"

// Task is one benchmark job.
type Task struct {
	Name   string
	Prompt string
	// Files is the starting repository (path → content).
	Files map[string]string
	// Hidden files are written over the repository before Verify runs.
	Hidden map[string]string
	// Verify is a bash command run in the repository; exit 0 means the task is done. Empty: only Answer counts.
	Verify string
	// Answer regexps must all match the final answer.
	Answer []string
	// Solution files, applied over Files, make Verify (and Hidden) pass.
	Solution map[string]string
	// SolutionAnswer is a final answer that satisfies Answer.
	SolutionAnswer string
	// Needs lists the programs the task needs on PATH (go, python3, make).
	Needs []string
}

// Options tune a run.
type Options struct {
	// Timeout bounds the whole task (default 10 minutes).
	Timeout time.Duration
	// MaxSteps overrides AGENT_MAX_STEPS (0: keep).
	MaxSteps int
	// Keep leaves the working directory in place and records it in Result.Dir.
	Keep bool
	// NoCacheKey sends no prompt_cache_key (some routers route on it).
	NoCacheKey bool
}

// Result is what one run of one task cost and achieved.
type Result struct {
	Task  string `json:"task"`
	Model string `json:"model"`
	// Passed: the run finished and the task's checks held.
	Passed bool `json:"passed"`
	// Status is "finished", or the RUN_ERROR code / failure that ended the run.
	Status       string `json:"status"`
	VerifyOutput string `json:"verifyOutput,omitempty"`
	FinalAnswer  string `json:"finalAnswer,omitempty"`

	Steps      int            `json:"steps"`
	ToolCalls  int            `json:"toolCalls"`
	ToolErrors int            `json:"toolErrors"`
	Tools      map[string]int `json:"tools,omitempty"`

	InputTokens  int     `json:"inputTokens"`
	OutputTokens int     `json:"outputTokens"`
	CachedTokens int     `json:"cachedTokens"`
	CacheHitPct  float64 `json:"cacheHitPct"`

	WallSeconds float64 `json:"wallSeconds"`
	// HeapMiB and SysMiB are the Go heap in use and memory obtained from the OS at the end of the run;
	// MaxRSSMiB is the process peak so far (it only grows across the tasks of one process).
	HeapMiB   float64 `json:"heapMiB"`
	SysMiB    float64 `json:"sysMiB"`
	MaxRSSMiB float64 `json:"maxRSSMiB"`

	Dir string `json:"dir,omitempty"`
}

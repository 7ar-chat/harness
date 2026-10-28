// Package workspace keeps a thread's /workspace history: a
// local Git repository with an automatic checkpoint before every AI turn,
// per-file history, diffs and restores, persisted across sandbox expiry (the
// .git directory is archived into the blob store after each run and restored
// into every new sandbox of the thread), plus the workspace status / explicit
// restore of the files panel.
package workspace

import (
	"errors"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/GhaziBenDahmane/harness/sandbox"
)

// Checkpoint kinds (the commit subject prefix, like Work).
const (
	KindAutomatic = "automatic"
	KindAgent     = "agent"
)

// LabelBeforeTurn is the checkpoint taken before every run that uses the sandbox.
const LabelBeforeTurn = "Before AI turn"

// Validation errors (400 invalid_path / invalid_checkpoint), Work's messages.
var (
	ErrPathRequired = errors.New("Path is required.")
	ErrPathOutside  = errors.New("Path must stay within /workspace.")
	ErrPathGit      = errors.New("Path cannot target Git metadata.")
	ErrPathControl  = errors.New("Path cannot contain ASCII control characters.")
	ErrInvalidSHA   = errors.New("Invalid checkpoint id.")
	// Paths a file restore refuses (400 invalid_path), checked before any change.
	ErrRestoreDirectory = errors.New("Only files can be restored, not directories.")
	ErrRestoreIgnored   = errors.New("This path is excluded from the checkpoints and cannot be restored.")
	ErrRestoreUntracked = errors.New("This path is not part of the checkpoint or the workspace history.")
)

// Errors of the checkpoint operations.
var (
	// ErrNotReady means the thread has no live sandbox (409 workspace_not_ready).
	ErrNotReady = errors.New("Workspace is not ready.")
	// ErrUnknownCheckpoint means the sha is not a commit of the repository (404).
	ErrUnknownCheckpoint = errors.New("Unknown checkpoint.")
	// ErrNoRepository means /workspace has no checkpoint repository yet.
	ErrNoRepository = errors.New("No checkpoint repository.")
)

// GitError is a failed git script (502 workspace_git_failed).
type GitError struct{ Detail string }

func (e *GitError) Error() string { return "workspace checkpoint failed: " + e.Detail }

// excludePatterns are kept out of checkpoints (.git/info/exclude): Work's list
// plus the harness's scratch directory and the artifact snapshot's exclusions.
var excludePatterns = []string{
	".harness/tmp/",
	".harness/runtime/",
	".codex/.tmp/",
	".codex/log/",
	".claude/log/",
	"node_modules/",
	"/.work/",
	"__pycache__/",
	".venv/",
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 32 || r == 127 {
			return true
		}
	}
	return false
}

// NormalizePath validates a file path of the workspace: absolute under
// /workspace or relative to it, no "..", not inside .git, no control
// characters. It returns the path relative to /workspace (the git pathspec).
func NormalizePath(raw string) (string, error) {
	raw = strings.ReplaceAll(raw, "\\", "/")
	if hasControl(raw) || !utf8.ValidString(raw) {
		return "", ErrPathControl
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ErrPathRequired
	}
	if strings.HasPrefix(raw, "/") {
		if raw != sandbox.Workspace && !strings.HasPrefix(raw, sandbox.Workspace+"/") {
			return "", ErrPathOutside
		}
		raw = strings.TrimPrefix(raw, sandbox.Workspace)
	}
	var parts []string
	for _, part := range strings.Split(raw, "/") {
		switch part {
		case "", ".":
			continue
		case "..":
			return "", ErrPathOutside
		case ".git":
			return "", ErrPathGit
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return "", ErrPathRequired
	}
	return strings.Join(parts, "/"), nil
}

// AbsPath is the sandbox path of a path relative to /workspace.
func AbsPath(rel string) string { return path.Join(sandbox.Workspace, rel) }

// ValidSHA validates a checkpoint id: 7 to 64 hexadecimal digits.
func ValidSHA(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if len(s) < 7 || len(s) > 64 {
		return "", ErrInvalidSHA
	}
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return "", ErrInvalidSHA
		}
	}
	return s, nil
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Script markers (last stdout line).
const (
	markNoGit          = "__WC_NO_GIT__"
	markNoChanges      = "__WC_CHECKPOINT_NO_CHANGES__"
	markNoRepo         = "__WC_NO_REPOSITORY__"
	markUnknownSHA     = "__WC_UNKNOWN_CHECKPOINT__"
	markRestoreNoop    = "__WC_RESTORE_NO_CHANGES__"
	markRestoreOK      = "__WC_RESTORE_OK__"
	markRestoreDeleted = "__WC_RESTORE_DELETED__"
	markRestoreInvalid = "__WC_RESTORE_INVALID__"
)

// scriptHeader starts every script: strict mode, no hooks, no signing, no
// pager or external diff (the agent may have configured the repository).
const scriptHeader = `set -eu
command -v git >/dev/null 2>&1 || { printf '` + markNoGit + `\n'; exit 0; }
g() { git -c core.hooksPath=/dev/null -c commit.gpgsign=false -c core.pager=cat -c core.quotepath=false "$@"; }
`

func excludeLines() string {
	quoted := make([]string, 0, len(excludePatterns))
	for _, p := range excludePatterns {
		quoted = append(quoted, quote(p))
	}
	return `mkdir -p .git/info
touch .git/info/exclude
for pattern in ` + strings.Join(quoted, " ") + `; do
  grep -qxF -- "$pattern" .git/info/exclude || printf '%s\n' "$pattern" >> .git/info/exclude
done
`
}

// Markers of the managed block of .git/info/exclude listing the files too
// large to checkpoint.
const (
	largeBegin = "# >>> harness: files too large to checkpoint"
	largeEnd   = "# <<< harness: files too large to checkpoint"
)

// largeFileLines keep files larger than limit bytes out of the checkpoints
// (limit ≤ 0: no limit): they are listed, as anchored literal patterns, in a
// managed block of .git/info/exclude rebuilt before every commit, and a
// tracked one stops being tracked (its earlier, smaller versions stay in the
// history). Large uploads would otherwise blow the script timeout and the
// archive cap, and leave the history unsaved.
func largeFileLines(limit int64) string {
	if limit <= 0 {
		return ""
	}
	return `awk -v b=` + quote(largeBegin) + ` -v e=` + quote(largeEnd) + ` '$0==b{skip=1;next} $0==e{skip=0;next} !skip' .git/info/exclude > .git/info/exclude.wc
{
  printf '%s\n' ` + quote(largeBegin) + `
  find . -path ./.git -prune -o -type f -size +` + strconv.FormatInt(limit, 10) + `c -print | while IFS= read -r f; do
    f=${f#.}
    if g --literal-pathspecs ls-files --error-unmatch -- ".$f" >/dev/null 2>&1; then
      g --literal-pathspecs rm -q --cached -- ".$f" >/dev/null 2>&1 || true
    fi
    printf '%s\n' "$f" | sed -e 's/[][*?\\]/\\&/g' -e 's/ $/\\ /'
  done
  printf '%s\n' ` + quote(largeEnd) + `
} >> .git/info/exclude.wc
mv .git/info/exclude.wc .git/info/exclude
`
}

// identityLines set the local identity of the checkpoint repository.
const identityLines = `g config user.email harness@local
g config user.name 'Harness'
`

// checkpointScript initializes the repository when needed (Work's
// ensure_repository: an "Initial workspace checkpoint", empty when the
// workspace is) and commits every change as subject, only when there are changes.
func checkpointScript(subject string, maxFileBytes int64) string {
	return scriptHeader + `if [ ! -d .git ]; then g init -q; fi
` + identityLines + excludeLines() + largeFileLines(maxFileBytes) + `g add -A
if ! g rev-parse --verify -q HEAD >/dev/null; then
  g commit -q --no-verify --allow-empty -m 'automatic: Initial workspace checkpoint'
  g rev-parse HEAD
  exit 0
fi
if g diff --cached --quiet; then
  printf '` + markNoChanges + `\n'
  exit 0
fi
g commit -q --no-verify -m ` + quote(subject) + `
g rev-parse HEAD
`
}

// maxListed bounds the checkpoints listed (newest first).
const maxListed = 200

// listScript prints, per commit touching rel (every commit when rel is ""):
// sha NUL date NUL subject NUL changed files NUL… NUL (Work's format).
func listScript(rel string) string {
	pathArg, logCmd, treeCmd := "", "g log", "g diff-tree"
	if rel != "" {
		pathArg = " -- " + quote(rel)
		logCmd, treeCmd = "g --literal-pathspecs log", "g --literal-pathspecs diff-tree"
	}
	return scriptHeader + `if [ ! -d .git ]; then printf '` + markNoRepo + `\n'; exit 0; fi
if ! g rev-parse --verify -q HEAD >/dev/null; then exit 0; fi
` + logCmd + ` --max-count=` + strconv.Itoa(maxListed) + ` --format='%H'` + pathArg + ` | while IFS= read -r sha; do
  g log -1 -z --date=iso-strict --format='%H%x00%cI%x00%s' "$sha"
  ` + treeCmd + ` --root --no-commit-id --name-only -r -z --diff-filter=AMDR "$sha"` + pathArg + `
  printf '\0'
done
`
}

func commitCheck(sha string) string {
	return `if [ ! -d .git ]; then printf '` + markNoRepo + `\n'; exit 0; fi
g cat-file -e ` + quote(sha+"^{commit}") + ` 2>/dev/null || { printf '` + markUnknownSHA + `\n'; exit 0; }
`
}

// diffScript diffs rel between the checkpoint and the working tree.
func diffScript(sha, rel string) string {
	return scriptHeader + commitCheck(sha) +
		`g --literal-pathspecs diff --no-ext-diff --no-textconv --no-color ` + quote(sha) + ` -- ` + quote(rel) + "\n"
}

// restoreFileScript is Work's restore_file: a "Pre-restore" checkpoint of
// pending changes, then the file at sha (removed when it did not exist there),
// committed as "Restore <path> from <short sha>". The path is validated before
// anything changes: a directory (at sha or now), a path the checkpoints ignore
// and a path neither at sha, in HEAD nor in the workspace are refused
// (markRestoreInvalid <reason>); a removal that cannot be committed is undone.
func restoreFileScript(sha, rel string, maxFileBytes int64) string {
	short := sha
	if len(short) > 7 {
		short = short[:7]
	}
	q, qs := quote(rel), quote(sha)
	return scriptHeader + commitCheck(sha) + identityLines + excludeLines() + largeFileLines(maxFileBytes) + `invalid() { printf '` + markRestoreInvalid + ` %s\n' "$1"; exit 0; }
at_sha=$(g --literal-pathspecs ls-tree ` + qs + ` -- ` + q + ` | cut -f1 | awk '{print $2}')
case "$at_sha" in
  tree) invalid directory ;;
  commit) invalid submodule ;;
esac
if [ -d ` + q + ` ] && [ ! -L ` + q + ` ]; then invalid directory; fi
if [ -z "$at_sha" ]; then
  # Removal: only of a file the history knows (an ignored file would be deleted without a trace).
  if g check-ignore -q -- ` + q + `; then invalid ignored; fi
  if ! g cat-file -e HEAD:` + q + ` 2>/dev/null && [ ! -e ` + q + ` ] && [ ! -L ` + q + ` ]; then invalid untracked; fi
fi
g add -A
if ! g diff --cached --quiet; then
  g commit -q --no-verify -m ` + quote("automatic: Pre-restore "+rel) + `
fi
state=` + markRestoreOK + `
if [ -n "$at_sha" ]; then
  g --literal-pathspecs checkout ` + qs + ` -- ` + q + `
else
  state=` + markRestoreDeleted + `
  # Undo the removal when it cannot be committed (never leave the file deleted on error).
  trap 'st=$?; if [ "$st" -ne 0 ]; then g --literal-pathspecs reset -q HEAD -- ` + q + ` 2>/dev/null || true; g --literal-pathspecs checkout -q HEAD -- ` + q + ` 2>/dev/null || true; fi' EXIT
  g --literal-pathspecs rm -q -f --cached --ignore-unmatch -- ` + q + `
  rm -f -- ` + q + `
fi
if g diff --cached --quiet; then
  printf '` + markRestoreNoop + `\n'
  exit 0
fi
g commit -q --no-verify -m ` + quote("automatic: Restore "+rel+" from "+short) + `
printf '%s\n' "$state"
`
}

// Checkpoint is one commit of the workspace history.
type Checkpoint struct {
	SHA          string   `json:"sha"`
	ShortSHA     string   `json:"shortSha"`
	CreatedAt    string   `json:"createdAt"`
	Kind         string   `json:"kind"`
	Label        string   `json:"label"`
	ChangedFiles []string `json:"changedFiles"`
}

func kindAndLabel(subject string) (string, string) {
	if rest, ok := strings.CutPrefix(subject, KindAutomatic+": "); ok {
		return KindAutomatic, rest
	}
	return KindAgent, strings.TrimPrefix(subject, KindAgent+": ")
}

func validParsedSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	_, err := ValidSHA(s)
	return err == nil
}

// parseLog parses listScript's output (Work's parse_checkpoint_log); changed
// files are returned as absolute /workspace paths.
func parseLog(out string) []Checkpoint {
	fields := strings.Split(out, "\x00")
	var list []Checkpoint
	for i := 0; i < len(fields); {
		if fields[i] == "" || strings.TrimSpace(fields[i]) == "" {
			i++
			continue
		}
		if i+2 >= len(fields) {
			break
		}
		sha, created, subject := strings.TrimSpace(fields[i]), fields[i+1], fields[i+2]
		i += 3
		files := []string{}
		for i < len(fields) && fields[i] != "" {
			files = append(files, AbsPath(fields[i]))
			i++
		}
		if i < len(fields) && fields[i] == "" {
			i++
		}
		if !validParsedSHA(sha) {
			continue
		}
		kind, label := kindAndLabel(subject)
		list = append(list, Checkpoint{SHA: sha, ShortSHA: sha[:7], CreatedAt: created, Kind: kind, Label: label, ChangedFiles: files})
	}
	return list
}

// lastLine is the last non-empty stdout line.
func lastLine(out string) string {
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}

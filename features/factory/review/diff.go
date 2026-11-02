package review

import (
	"strconv"
	"strings"
)

// FileChange is one file of a unified diff.
type FileChange struct {
	// Path is the file after the change (before it for a deletion).
	Path string
	// OldPath is where a renamed file came from ("" otherwise).
	OldPath string
	Added   int
	Removed int
	Binary  bool
	Renamed bool
	New     bool
	Deleted bool
	// ModeChanged is set when the permission bits changed; OldMode and NewMode are git's octal strings.
	ModeChanged bool
	OldMode     string
	NewMode     string
	// Patch is the file's section of the diff, headers and hunks, verbatim.
	Patch string
	// AddedLines are the lines the change adds, with their line number in the new file.
	AddedLines []AddedLine
}

// AddedLine is one line a diff adds.
type AddedLine struct {
	Line int
	Text string
}

// ParseDiff parses a unified diff (git format, or plain ---/+++ pairs). It
// follows the hunk headers' line counts, so a removed line that looks like a
// file header ("--- a" deleted from a file) is not mistaken for one.
func ParseDiff(diff string) []FileChange {
	var (
		files            []FileChange
		cur              *FileChange
		start            int
		oldPath, newPath string
		headerPaths      [2]string // from "diff --git"
		hunks            int
		oldLeft, newLeft int
		newLine          int
	)
	flush := func(end int) {
		if cur == nil {
			return
		}
		cur.Patch = diff[start:end]
		switch {
		case cur.Renamed:
			// rename lines are authoritative
		case cur.Deleted && oldPath != "":
			cur.Path = oldPath
		case newPath != "":
			cur.Path = newPath
		case oldPath != "":
			cur.Path = oldPath
		default:
			cur.Path = headerPaths[1]
		}
		if cur.Path == "" {
			cur.Path = headerPaths[1]
		}
		files = append(files, *cur)
		cur = nil
	}
	begin := func(off int) {
		flush(off)
		cur = &FileChange{}
		start = off
		oldPath, newPath = "", ""
		headerPaths = [2]string{}
		hunks, oldLeft, newLeft = 0, 0, 0
	}

	for off := 0; off < len(diff); {
		end := strings.IndexByte(diff[off:], '\n')
		next := len(diff)
		line := diff[off:]
		if end >= 0 {
			line = diff[off : off+end]
			next = off + end + 1
		}
		line = strings.TrimSuffix(line, "\r")

		if cur != nil && (oldLeft > 0 || newLeft > 0) {
			c := byte(' ')
			if line != "" {
				c = line[0]
			}
			switch c {
			case '+':
				cur.Added++
				cur.AddedLines = append(cur.AddedLines, AddedLine{Line: newLine, Text: line[1:]})
				newLine++
				newLeft--
				off = next
				continue
			case '-':
				cur.Removed++
				oldLeft--
				off = next
				continue
			case ' ':
				oldLeft--
				newLeft--
				newLine++
				off = next
				continue
			case '\\':
				off = next
				continue
			}
			// Malformed counts: leave the hunk and read the line as a header.
			oldLeft, newLeft = 0, 0
		}

		switch {
		case strings.HasPrefix(line, "diff --git "):
			begin(off)
			headerPaths = splitGitHeader(strings.TrimPrefix(line, "diff --git "))
		case strings.HasPrefix(line, "--- ") && (cur == nil || hunks > 0):
			begin(off)
			oldPath = diffPath(line[4:], true)
			cur.New = oldPath == ""
		case cur == nil:
			// preamble before the first file
		case strings.HasPrefix(line, "--- "):
			oldPath = diffPath(line[4:], true)
			cur.New = cur.New || oldPath == ""
		case strings.HasPrefix(line, "+++ "):
			newPath = diffPath(line[4:], false)
			cur.Deleted = cur.Deleted || newPath == ""
		case strings.HasPrefix(line, "@@ "):
			hunks++
			oldLeft, newLeft, newLine = parseHunk(line)
		case strings.HasPrefix(line, "new file mode "):
			cur.New = true
		case strings.HasPrefix(line, "deleted file mode "):
			cur.Deleted = true
		case strings.HasPrefix(line, "old mode "):
			cur.ModeChanged = true
			cur.OldMode = strings.TrimSpace(strings.TrimPrefix(line, "old mode "))
		case strings.HasPrefix(line, "new mode "):
			cur.ModeChanged = true
			cur.NewMode = strings.TrimSpace(strings.TrimPrefix(line, "new mode "))
		case strings.HasPrefix(line, "rename from "):
			cur.Renamed = true
			cur.OldPath = unquoteGit(strings.TrimPrefix(line, "rename from "))
		case strings.HasPrefix(line, "rename to "):
			cur.Renamed = true
			cur.Path = unquoteGit(strings.TrimPrefix(line, "rename to "))
		case strings.HasPrefix(line, "Binary files "), strings.HasPrefix(line, "GIT binary patch"):
			cur.Binary = true
		}
		off = next
	}
	flush(len(diff))
	return files
}

// parseHunk reads "@@ -a[,b] +c[,d] @@": the old and new line counts and the
// first new line number. An omitted count is 1.
func parseHunk(line string) (oldN, newN, newStart int) {
	rest := strings.TrimPrefix(line, "@@ ")
	head, _, _ := strings.Cut(rest, " @@")
	o, n, ok := strings.Cut(head, " ")
	if !ok {
		return 0, 0, 0
	}
	_, oc := rangeOf(strings.TrimPrefix(o, "-"))
	ns, nc := rangeOf(strings.TrimPrefix(n, "+"))
	return oc, nc, ns
}

func rangeOf(s string) (start, count int) {
	a, b, hasCount := strings.Cut(s, ",")
	start, _ = strconv.Atoi(a)
	count = 1
	if hasCount {
		count, _ = strconv.Atoi(b)
	}
	return start, count
}

// diffPath reads the path of a ---/+++ line: "" for /dev/null, the git a/ or
// b/ prefix and a trailing timestamp removed.
func diffPath(s string, old bool) string {
	if !strings.HasPrefix(s, `"`) {
		s, _, _ = strings.Cut(s, "\t")
	}
	s = unquoteGit(s)
	if s == "/dev/null" {
		return ""
	}
	prefix := "b/"
	if old {
		prefix = "a/"
	}
	return strings.TrimPrefix(s, prefix)
}

// unquoteGit undoes git's C-style quoting of unusual path names.
func unquoteGit(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, `"`) {
		if u, err := strconv.Unquote(s); err == nil {
			return u
		}
		return strings.Trim(s, `"`)
	}
	return s
}

// splitGitHeader reads "a/x b/y" of a "diff --git" line. Names with spaces
// are ambiguous: the split where both halves name the same file wins, else
// the first. Renames carry their names on their own lines, which override it.
func splitGitHeader(rest string) [2]string {
	if strings.HasPrefix(rest, `"`) {
		// A quoted old name ends at its closing quote.
		for i := 1; i < len(rest); i++ {
			if rest[i] == '\\' {
				i++
				continue
			}
			if rest[i] == '"' {
				a := strings.TrimPrefix(unquoteGit(rest[:i+1]), "a/")
				b := strings.TrimPrefix(unquoteGit(rest[i+1:]), "b/")
				return [2]string{a, b}
			}
		}
	}
	var first [2]string
	for i := range len(rest) {
		if !strings.HasPrefix(rest[i:], " b/") && !strings.HasPrefix(rest[i:], ` "b/`) {
			continue
		}
		a := strings.TrimPrefix(unquoteGit(rest[:i]), "a/")
		b := strings.TrimPrefix(unquoteGit(rest[i+1:]), "b/")
		if a == b {
			return [2]string{a, b}
		}
		if first == ([2]string{}) {
			first = [2]string{a, b}
		}
	}
	return first
}

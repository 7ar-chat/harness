// Package skills is the skill registry: a skill is a folder with a SKILL.md
// (frontmatter name and description) and optional scripts and references.
// Sources are the built-in skills embedded in the binary, an optional extra
// fs.FS, and the user directory; package validation applies generic size and
// path limits, and the installer copies the skills a run needs into its
// sandbox under /skills/<name>/ (used by the load_skill tool).
package skills

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
	"gopkg.in/yaml.v3"
)

// Package limits: the frontmatter name and description are bounded in UTF-8
// bytes (256 B / 16 KiB); the name is a free display name, the identifier
// (RunOptions.Skills, /skills/<name>/) is its slug (Slug).
const (
	MaxEntries          = 500
	MaxDirectoryDepth   = 16
	MaxSkillMDBytes     = 1 << 20
	MaxFileBytes        = 16 << 20
	MaxPackageBytes     = 64 << 20
	MaxArchiveBytes     = 64 << 20
	MaxCompressRatio    = 100
	MaxNameBytes        = 256
	MaxDescriptionBytes = 16 << 10
	// MaxSlugChars bounds the identifier derived from the name.
	MaxSlugChars = 64
)

// InvalidError lists every validation issue of a package ("code: message").
type InvalidError struct {
	Details []string
	// Issues are the structured issues (Work's SkillPackageIssue).
	Issues []Issue
}

func (e *InvalidError) Error() string {
	return "invalid skill package: " + strings.Join(e.Details, "; ")
}

func invalid(details ...string) error {
	e := &InvalidError{Details: details}
	for _, d := range details {
		code, msg, _ := strings.Cut(d, ": ")
		e.Issues = append(e.Issues, Issue{Code: code, Message: msg, Severity: SeverityError})
	}
	return e
}

// invalidIssues builds the error of a list of issues.
func invalidIssues(issues []Issue) error {
	e := &InvalidError{Issues: issues}
	for _, i := range issues {
		d := i.Code + ": " + i.Message
		if i.Path != "" && !strings.Contains(i.Message, i.Path) {
			d += " (" + i.Path + ")"
		}
		e.Details = append(e.Details, d)
	}
	return e
}

// File is one package entry.
type File struct {
	Path string
	Data []byte
}

// Package is a validated skill.
type Package struct {
	Name        string
	Description string
	License     string
	// Requires are the skills this one's scripts use (frontmatter "requires":
	// a list of skill names), installed with it.
	Requires []string
	// Uses are the other skills its own files (not the markdown) name as
	// /skills/<name>/, e.g. a script importing another skill's module:
	// implicit dependencies, installed with it (only the known ones).
	Uses []string
	// Body is SKILL.md without its frontmatter.
	Body string
	// Files are sorted by path (SKILL.md included).
	Files []File
	Size  int64
	// Revision is the content digest (sha256 over sorted path + content).
	Revision string
}

// Paths returns the package-relative paths.
func (p *Package) Paths() []string {
	out := make([]string, len(p.Files))
	for i, f := range p.Files {
		out[i] = f.Path
	}
	return out
}

// Zip serializes the canonical package.
func (p *Package) Zip() ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range p.Files {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: f.Path, Method: zip.Deflate})
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(f.Data); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

var (
	reservedDevices = map[string]bool{"con": true, "prn": true, "aux": true, "nul": true, "conin$": true, "conout$": true}
	numberedDevice  = regexp.MustCompile(`(?i)^(?:com|lpt)[1-9¹²³]$`)
	windowsDrive    = regexp.MustCompile(`^[A-Za-z]:`)
	nativeExts      = map[string]bool{".dll": true, ".dylib": true, ".elf": true, ".exe": true, ".o": true, ".so": true}
	nativeMagics    = [][]byte{[]byte("MZ"), []byte("\x7fELF"), {0xfe, 0xed, 0xfa, 0xce}, {0xce, 0xfa, 0xed, 0xfe},
		{0xfe, 0xed, 0xfa, 0xcf}, {0xcf, 0xfa, 0xed, 0xfe}, {0xca, 0xfe, 0xba, 0xbe}, {0xca, 0xfe, 0xba, 0xbf},
		{0xbe, 0xba, 0xfe, 0xca}, {0xbf, 0xba, 0xfe, 0xca}}
)

func unsafeComponent(c string) bool {
	base := strings.ToLower(strings.TrimRight(strings.SplitN(c, ".", 2)[0], " "))
	if strings.Contains(c, ":") || strings.HasSuffix(c, ".") || strings.HasSuffix(c, " ") {
		return true
	}
	for _, r := range c {
		if unicode.IsControl(r) {
			return true
		}
	}
	return reservedDevices[base] || numberedDevice.MatchString(base)
}

// normalizePath mirrors normalize_skill_package_path (NFC, POSIX, no traversal).
func normalizePath(p string) (string, bool) {
	p = strings.TrimSpace(p)
	p = norm.NFC.String(strings.ReplaceAll(p, "\\", "/"))
	if p == "" || strings.ContainsRune(p, 0) || strings.HasPrefix(p, "/") || windowsDrive.MatchString(p) {
		return "", false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." || unsafeComponent(part) {
			return "", false
		}
	}
	return p, true
}

func native(p string, data []byte) bool {
	if nativeExts[strings.ToLower(path.Ext(p))] {
		return true
	}
	for _, m := range nativeMagics {
		if bytes.HasPrefix(data, m) {
			return true
		}
	}
	return false
}

// Issue severities.
const (
	SeverityError   = "error"
	SeverityWarning = "warning"
)

// Issue is one validation finding (Work's SkillPackageIssue).
type Issue struct {
	Code     string
	Message  string
	Severity string
	Path     string
}

// Entry describes one file of a validated package.
type Entry struct {
	Path      string
	MediaType string
	SizeBytes int64
}

// Metadata is the SKILL.md frontmatter name and description.
type Metadata struct {
	Name        string
	Description string
}

// Validation is the result of a package validation (Work's
// SkillPackageValidationResult): Package is set only when Valid.
type Validation struct {
	Valid    bool
	Revision string
	Metadata *Metadata
	Entries  []Entry
	Issues   []Issue
	Package  *Package
}

// Upload is one extra file of a manually authored package.
type Upload struct {
	Path      string
	Data      []byte
	MediaType string
}

func issue(code, msg, path string) Issue {
	return Issue{Code: code, Message: msg, Severity: SeverityError, Path: path}
}

// ParseZip reads and validates an uploaded zip package.
func ParseZip(data []byte) (*Package, error) {
	files, iss := readZip(data)
	if iss != nil {
		return nil, invalidIssues([]Issue{*iss})
	}
	pkg, issues := build(reroot(files))
	if len(issues) > 0 {
		return nil, invalidIssues(issues)
	}
	return pkg, nil
}

// ValidateZip is the validate endpoint view of ParseZip (Work's read_zip_skill_package).
func ValidateZip(data []byte) Validation {
	files, iss := readZip(data)
	if iss != nil {
		return Validation{Issues: []Issue{*iss}}
	}
	return validation(reroot(files))
}

// ValidateManual builds and validates a package from the SKILL.md editor text
// and extra uploaded files (Work's read_manual_skill_package).
func ValidateManual(skillMD string, uploads []Upload) Validation {
	if !utf8.ValidString(skillMD) {
		return Validation{Issues: []Issue{issue("invalid_utf8", "SKILL.md must contain valid UTF-8 text.", "SKILL.md")}}
	}
	files := []File{{Path: "SKILL.md", Data: []byte(skillMD)}}
	var readIssues []Issue
	for _, u := range uploads {
		p, ok := normalizePath(u.Path)
		if !ok {
			readIssues = append(readIssues, issue("unsafe_path", "Package paths must be safe and package-relative.", u.Path))
			continue
		}
		files = append(files, File{Path: p, Data: u.Data})
	}
	v := validation(files)
	if len(readIssues) > 0 {
		v.Valid, v.Package = false, nil
		v.Issues = append(readIssues, v.Issues...)
	}
	return v
}

// ValidateFiles validates already collected package files (generated packages).
func ValidateFiles(files []File) Validation { return validation(files) }

func validation(files []File) Validation {
	pkg, issues := build(files)
	v := Validation{Issues: issues, Revision: revision(files)}
	for _, f := range files {
		v.Entries = append(v.Entries, Entry{Path: f.Path, MediaType: MediaType(f.Path), SizeBytes: int64(len(f.Data))})
	}
	if len(files) == 0 {
		v.Revision = ""
	}
	if pkg != nil {
		v.Valid, v.Package = true, pkg
		v.Metadata = &Metadata{Name: pkg.Name, Description: pkg.Description}
	} else if meta, _, metaIssues := parseRoot(files); meta != nil && len(metaIssues) == 0 {
		v.Metadata = &Metadata{Name: meta.Name, Description: meta.Description}
	}
	return v
}

// MediaType guesses the media type of a package path (SKILL.md is text/markdown).
func MediaType(p string) string {
	if p == "SKILL.md" {
		return "text/markdown"
	}
	if t := mime.TypeByExtension(path.Ext(p)); t != "" {
		return t
	}
	return "application/octet-stream"
}

func readZip(data []byte) ([]File, *Issue) {
	fail := func(code, msg, p string) ([]File, *Issue) {
		i := issue(code, msg, p)
		return nil, &i
	}
	if len(data) > MaxArchiveBytes {
		return fail("archive_too_large", "The ZIP archive exceeds the 64 MiB limit.", "")
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fail("invalid_zip", "Upload a valid ZIP archive.", "")
	}
	if len(zr.File) > MaxEntries {
		return fail("too_many_entries", "The package contains too many entries.", "")
	}
	var files []File
	var expanded int64
	for _, f := range zr.File {
		mode := f.Mode()
		switch {
		case mode&fs.ModeSymlink != 0:
			return fail("symbolic_link", "Links and special filesystem entries are not supported.", f.Name)
		case mode&(fs.ModeNamedPipe|fs.ModeDevice|fs.ModeCharDevice|fs.ModeSocket) != 0:
			return fail("special_file", "Links and special filesystem entries are not supported.", f.Name)
		case hardLink(f.Extra):
			return fail("hard_link", "Links and special filesystem entries are not supported.", f.Name)
		}
		dir := f.FileInfo().IsDir() || strings.HasSuffix(f.Name, "/")
		p, ok := normalizePath(strings.TrimSuffix(f.Name, "/"))
		if !ok {
			return fail("unsafe_path", "Package paths must be safe and package-relative.", f.Name)
		}
		if dir {
			continue
		}
		if f.Flags&0x1 != 0 {
			return fail("encrypted_entry", "Encrypted ZIP entries are not supported.", p)
		}
		limit := int64(MaxFileBytes)
		code := "file_too_large"
		if path.Base(p) == "SKILL.md" && strings.Count(p, "/") <= 1 {
			limit, code = MaxSkillMDBytes, "skill_md_too_large"
		}
		rc, err := f.Open()
		if err != nil {
			return fail("invalid_zip", "Upload a valid ZIP archive.", p)
		}
		content, err := io.ReadAll(io.LimitReader(rc, limit+1))
		rc.Close()
		if err != nil {
			return fail("invalid_zip", "Upload a valid ZIP archive.", p)
		}
		if int64(len(content)) > limit {
			return fail(code, "The package file exceeds its size limit.", p)
		}
		expanded += int64(len(content))
		if expanded > MaxPackageBytes {
			return fail("package_too_large", "The expanded package exceeds the 64 MiB limit.", "")
		}
		if expanded > int64(len(data))*MaxCompressRatio {
			return fail("decompression_ratio_exceeded", "The ZIP decompression ratio is too high.", "")
		}
		files = append(files, File{Path: p, Data: content})
	}
	return files, nil
}

// pkwareUnixExtra is the PKWARE Unix extra field: atime, mtime (4 bytes
// each), uid, gid (2 bytes each), then the link target of a hard or symbolic link.
const pkwareUnixExtra = 0x000d

// hardLink parses the extra field as typed records (tag, size, data) and
// reports a PKWARE Unix record carrying a link target. Other records (NTFS or
// extended timestamps, Info-ZIP Unix ids…) are skipped by their size, so their
// bytes are never mistaken for a link; parsing stops at a malformed record.
func hardLink(extra []byte) bool {
	for len(extra) >= 4 {
		tag := binary.LittleEndian.Uint16(extra[0:2])
		size := int(binary.LittleEndian.Uint16(extra[2:4]))
		extra = extra[4:]
		if size > len(extra) {
			return false
		}
		if tag == pkwareUnixExtra && size > 12 {
			return true
		}
		extra = extra[size:]
	}
	return false
}

// reroot strips a single top-level folder when SKILL.md is only inside it
// (a zipped skill folder).
func reroot(files []File) []File {
	top := ""
	for _, f := range files {
		if f.Path == "SKILL.md" {
			return files
		}
		first, _, nested := strings.Cut(f.Path, "/")
		if !nested || (top != "" && first != top) {
			return files
		}
		top = first
	}
	if top == "" {
		return files
	}
	hasSkill := false
	for _, f := range files {
		if f.Path == top+"/SKILL.md" {
			hasSkill = true
		}
	}
	if !hasSkill {
		return files
	}
	out := make([]File, len(files))
	for i, f := range files {
		out[i] = File{Path: strings.TrimPrefix(f.Path, top+"/"), Data: f.Data}
	}
	return out
}

// LoadFS reads a skill directory (built-in skills).
func LoadFS(fsys fs.FS) (*Package, error) {
	var files []File
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") && p != "." || d.Name() == "__pycache__" {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") || strings.HasSuffix(d.Name(), ".pyc") {
			return nil
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		files = append(files, File{Path: p, Data: data})
		return nil
	})
	if err != nil {
		return nil, err
	}
	pkg, issues := build(files)
	if len(issues) > 0 {
		return nil, invalidIssues(issues)
	}
	return pkg, nil
}

// revision is the package digest: sha256 over the sorted (path, content) pairs.
func revision(files []File) string {
	sorted := append([]File(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	digest := sha256.New()
	for _, f := range sorted {
		digest.Write([]byte(f.Path))
		digest.Write([]byte{0})
		digest.Write(f.Data)
		digest.Write([]byte{0})
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// build validates the package structure, limits and metadata (Work's validate_skill_package).
func build(files []File) (*Package, []Issue) {
	var issues []Issue
	add := func(code, msg, p string) { issues = append(issues, issue(code, msg, p)) }
	if len(files) > MaxEntries {
		add("too_many_entries", "The package contains too many entries.", "")
	}
	seen := map[string]bool{}
	var total int64
	for i := range files {
		f := &files[i]
		if _, ok := normalizePath(f.Path); !ok {
			add("unsafe_path", "Package paths must be safe and package-relative.", f.Path)
		}
		key := strings.ToLower(norm.NFC.String(f.Path))
		if seen[key] {
			add("duplicate_path", "Package entry paths must be unique after normalization.", f.Path)
		}
		seen[key] = true
		if strings.Count(f.Path, "/") > MaxDirectoryDepth {
			add("max_depth_exceeded", "The package contains too many directory levels.", f.Path)
		}
		size := int64(len(f.Data))
		total += size
		if f.Path == "SKILL.md" {
			if size > MaxSkillMDBytes {
				add("skill_md_too_large", "SKILL.md exceeds the 1 MiB limit.", "SKILL.md")
			}
		} else if size > MaxFileBytes {
			add("file_too_large", "A package file exceeds the 16 MiB limit.", f.Path)
		}
		if native(f.Path, f.Data) {
			add("native_executable", "Native executables and shared libraries are not supported.", f.Path)
		}
	}
	if total > MaxPackageBytes {
		add("package_too_large", "The expanded package exceeds the 64 MiB limit.", "")
	}
	root := hasRoot(files)
	if !root {
		add("missing_skill_md", "The package must contain SKILL.md at its root.", "SKILL.md")
	}
	for _, f := range files {
		if f.Path == "SKILL.md" || !strings.EqualFold(path.Base(f.Path), "SKILL.md") {
			continue
		}
		switch {
		case root && strings.Contains(f.Path, "/"):
			add("multiple_skill_md", "The package must contain exactly one root SKILL.md.", f.Path)
		case !root:
			add("misplaced_skill_md", "SKILL.md must be at the package root.", f.Path)
		}
	}
	meta, body, metaIssues := parseRoot(files)
	issues = append(issues, metaIssues...)
	if len(issues) > 0 || meta == nil {
		return nil, issues
	}
	sorted := append([]File(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	return &Package{Name: meta.Name, Description: meta.Description, License: meta.License, Requires: meta.Requires,
		Uses: usesOf(Slug(meta.Name), sorted), Body: body, Files: sorted,
		Size: total, Revision: revision(sorted)}, nil
}

func hasRoot(files []File) bool {
	for _, f := range files {
		if f.Path == "SKILL.md" {
			return true
		}
	}
	return false
}

// parseRoot parses the root SKILL.md (nil metadata when absent or too large).
func parseRoot(files []File) (*frontmatter, string, []Issue) {
	for _, f := range files {
		if f.Path == "SKILL.md" {
			if len(f.Data) > MaxSkillMDBytes {
				return nil, "", nil
			}
			fm, body, issues := parseSkillMD(f.Data)
			if len(issues) > 0 {
				return nil, "", issues
			}
			return &fm, body, nil
		}
	}
	return nil, "", nil
}

type frontmatter struct {
	Name        string
	Description string
	License     string
	Requires    []string
}

// parseSkillMD validates the YAML frontmatter and returns the body after it.
func parseSkillMD(data []byte) (frontmatter, string, []Issue) {
	fail := func(code, msg string) (frontmatter, string, []Issue) {
		return frontmatter{}, "", []Issue{issue(code, msg, "SKILL.md")}
	}
	if !utf8.Valid(data) {
		return fail("invalid_utf8", "SKILL.md must contain valid UTF-8 text.")
	}
	text := strings.ReplaceAll(strings.TrimPrefix(string(data), "\ufeff"), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return fail("invalid_frontmatter", "SKILL.md must begin with YAML frontmatter.")
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return fail("invalid_frontmatter", "SKILL.md frontmatter is not closed.")
	}
	var raw map[string]any
	if err := yaml.Unmarshal([]byte(quotePlainScalars(lines[1:end])), &raw); err != nil {
		return fail("invalid_frontmatter", "SKILL.md frontmatter must be valid YAML.")
	}
	if raw == nil {
		return fail("invalid_frontmatter", "SKILL.md frontmatter must be a YAML mapping.")
	}
	var issues []Issue
	field := func(key string, maxBytes int, code string) string {
		v, ok := raw[key]
		if !ok {
			issues = append(issues, issue(code, fmt.Sprintf("SKILL.md frontmatter requires a non-empty %s.", key), "SKILL.md"))
			return ""
		}
		s, ok := v.(string)
		if !ok {
			issues = append(issues, issue("invalid_metadata", fmt.Sprintf("SKILL.md frontmatter %s must be a string.", key), "SKILL.md"))
			return ""
		}
		s = strings.TrimSpace(s)
		if s == "" {
			issues = append(issues, issue(code, fmt.Sprintf("SKILL.md frontmatter requires a non-empty %s.", key), "SKILL.md"))
			return ""
		}
		if len(s) > maxBytes {
			issues = append(issues, issue("metadata_too_large", fmt.Sprintf("SKILL.md %s exceeds its size limit.", key), "SKILL.md"))
		}
		return s
	}
	fm := frontmatter{Name: field("name", MaxNameBytes, "missing_name"), Description: field("description", MaxDescriptionBytes, "missing_description")}
	if fm.Name != "" && Slug(fm.Name) == "" {
		issues = append(issues, issue("invalid_name", "The skill name must contain at least one letter or digit.", "SKILL.md"))
	}
	if l, ok := raw["license"].(string); ok {
		fm.License = strings.TrimSpace(l)
	}
	fm.Requires = requiresOf(raw["requires"])
	body := strings.TrimLeft(strings.Join(lines[end+1:], "\n"), "\n")
	return fm, body, issues
}

// usesOf returns the skills the package's non-markdown files name as
// /skills/<name>/ (SkillPaths), itself excluded, in order. Markdown names
// commands the model runs (installed when it runs them), not dependencies.
func usesOf(self string, files []File) []string {
	var out []string
	for _, f := range files {
		if strings.EqualFold(path.Ext(f.Path), ".md") || !utf8.Valid(f.Data) {
			continue
		}
		names, _ := SkillPaths(string(f.Data))
		for _, n := range names {
			if n != self && !slices.Contains(out, n) {
				out = append(out, n)
			}
		}
	}
	return out
}

// requiresOf reads the optional "requires" frontmatter field: a skill name or
// a list of them, as slugs, deduplicated (anything else is ignored).
func requiresOf(v any) []string {
	var names []string
	switch t := v.(type) {
	case string:
		names = strings.Split(t, ",")
	case []any:
		for _, e := range t {
			if s, ok := e.(string); ok {
				names = append(names, s)
			}
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, n := range names {
		if s := Slug(strings.TrimSpace(n)); s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// plainKey is a top-level "key: value" line of the frontmatter.
var plainKey = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_-]*):[ \t]+(\S.*)$`)

// quotePlainScalars returns the frontmatter YAML with every top-level plain
// (unquoted) scalar value that YAML would misread quoted as written: " #"
// starts a comment and ": " a nested mapping, so "scanned for #REF! errors"
// would silently lose its end (A-14). A plain value continued on indented
// lines is folded with spaces first, as YAML does. Quoted, block (| >),
// flow ([ {), anchored, aliased or tagged values are left to YAML.
func quotePlainScalars(lines []string) string {
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		m := plainKey.FindStringSubmatch(lines[i])
		if m == nil || strings.ContainsRune(`"'|>[{&*!%@`+"`", rune(m[2][0])) {
			out = append(out, lines[i])
			continue
		}
		value := strings.TrimRight(m[2], " \t")
		j := i + 1
		for ; j < len(lines) && (strings.HasPrefix(lines[j], " ") || strings.HasPrefix(lines[j], "\t")) && strings.TrimSpace(lines[j]) != ""; j++ {
			value += " " + strings.TrimSpace(lines[j])
		}
		if !strings.Contains(value, " #") && !strings.Contains(value, "\t#") && !strings.Contains(value, ": ") {
			out = append(out, lines[i:j]...)
			i = j - 1
			continue
		}
		quoted, _ := json.Marshal(value) // a JSON string is a valid YAML double-quoted scalar
		out = append(out, m[1]+": "+string(quoted))
		i = j - 1
	}
	return strings.Join(out, "\n")
}

// Slug is the identifier of a skill name: lowercase ASCII letters and digits
// separated by single hyphens (accents removed), at most MaxSlugChars; "" when
// the name has no letter or digit. A kebab-case name is its own slug.
func Slug(name string) string {
	decomposed := norm.NFKD.String(strings.ToLower(name))
	var b strings.Builder
	dash := false
	for _, r := range decomposed {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		case unicode.Is(unicode.Mn, r):
			// combining accent: dropped
		default:
			dash = true
		}
	}
	s := b.String()
	if len(s) > MaxSlugChars {
		s = strings.TrimRight(s[:MaxSlugChars], "-")
	}
	return s
}

// Digest is the hex sha256 of data.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

package skills_test

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"io/fs"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/7ar-chat/harness/features/skills"
)

// skillMD is a minimal valid SKILL.md.
func skillMD(name, description string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\n---\n\nBody of " + name + ".\n"
}

// zipEntry is one entry of a test archive; header customizes it (mode, flags, extra).
type zipEntry struct {
	name   string
	data   []byte
	header func(*zip.FileHeader)
}

func buildZip(t *testing.T, entries ...zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if e.header != nil {
			e.header(h)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(e.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func codes(issues []skills.Issue) []string {
	out := make([]string, len(issues))
	for i, is := range issues {
		out[i] = is.Code
	}
	return out
}

// requireCode fails unless the issues contain code (reporting path when given).
func requireCode(t *testing.T, issues []skills.Issue, code, path string) {
	t.Helper()
	for _, is := range issues {
		if is.Code == code && (path == "" || is.Path == path) {
			if is.Severity != skills.SeverityError {
				t.Errorf("issue %s severity %q, want error", code, is.Severity)
			}
			return
		}
	}
	t.Fatalf("no %s issue (path %q) in %+v", code, path, issues)
}

func TestParseZipReadsAZippedSkillFolderAndStripsTheTopLevelFolder(t *testing.T) {
	// A skill folder zipped as a whole (my-skill/SKILL.md, my-skill/scripts/x.py) is accepted and its files are rooted at SKILL.md, sorted, with directory entries ignored.
	// Arrange
	data := buildZip(t,
		zipEntry{name: "my-skill/", header: func(h *zip.FileHeader) { h.SetMode(fs.ModeDir | 0o755) }},
		zipEntry{name: "my-skill/scripts/x.py", data: []byte("print(1)\n")},
		zipEntry{name: "my-skill/SKILL.md", data: []byte(skillMD("my-skill", "Does things."))},
	)

	// Act
	pkg, err := skills.ParseZip(data)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if got := pkg.Paths(); !slices.Equal(got, []string{"SKILL.md", "scripts/x.py"}) {
		t.Fatalf("paths %v", got)
	}
	if pkg.Name != "my-skill" || pkg.Description != "Does things." || pkg.Body != "Body of my-skill.\n" {
		t.Fatalf("package %+v", pkg)
	}
}

func TestParseZipDoesNotStripAFolderWhenSkillMDIsMissingFromIt(t *testing.T) {
	// A single top-level folder without its own SKILL.md is not a zipped skill: the package is reported as missing its root SKILL.md.
	// Arrange
	data := buildZip(t, zipEntry{name: "docs/readme.txt", data: []byte("x")})

	// Act
	_, err := skills.ParseZip(data)

	// Assert
	var inv *skills.InvalidError
	if !errors.As(err, &inv) {
		t.Fatalf("err %v", err)
	}
	requireCode(t, inv.Issues, "missing_skill_md", "SKILL.md")
}

func TestParseZipKeepsTwoTopLevelFoldersAsIs(t *testing.T) {
	// Two top-level folders are never merged by the re-rooting: SKILL.md stays missing from the root.
	// Arrange
	data := buildZip(t,
		zipEntry{name: "a/SKILL.md", data: []byte(skillMD("a", "A."))},
		zipEntry{name: "b/other.txt", data: []byte("x")},
	)

	// Act
	v := skills.ValidateZip(data)

	// Assert
	if v.Valid {
		t.Fatalf("valid: %+v", v)
	}
	requireCode(t, v.Issues, "missing_skill_md", "")
	requireCode(t, v.Issues, "misplaced_skill_md", "a/SKILL.md")
}

func TestZipSerializesAPackageThatParsesBackIdentically(t *testing.T) {
	// A package's Zip is canonical: parsing it again yields the same revision, files and metadata.
	// Arrange
	v := skills.ValidateManual(skillMD("round-trip", "Round trip."), []skills.Upload{{Path: "scripts/run.py", Data: []byte("print('x')\n")}})
	if !v.Valid {
		t.Fatalf("setup: %+v", v.Issues)
	}

	// Act
	data, err := v.Package.Zip()
	if err != nil {
		t.Fatal(err)
	}
	back, err := skills.ParseZip(data)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if back.Revision != v.Package.Revision || back.Name != "round-trip" || !slices.Equal(back.Paths(), []string{"SKILL.md", "scripts/run.py"}) {
		t.Fatalf("round trip: %+v vs %+v", back, v.Package)
	}
	if string(back.Files[1].Data) != "print('x')\n" {
		t.Fatalf("content %q", back.Files[1].Data)
	}
}

func TestReadZipRejectsLinksSpecialFilesAndEncryptedEntries(t *testing.T) {
	// Symbolic links, devices/pipes, hard links (PKWARE Unix extra with a target) and encrypted entries are refused with the code of their kind and the offending path.
	pkware := func(target string) []byte {
		body := append(make([]byte, 12), target...) // atime, mtime, uid, gid, then the link target
		extra := make([]byte, 4, 4+len(body))
		binary.LittleEndian.PutUint16(extra[0:], 0x000d)
		binary.LittleEndian.PutUint16(extra[2:], uint16(len(body)))
		return append(extra, body...)
	}
	cases := []struct {
		name  string
		entry zipEntry
		code  string
	}{
		{"symlink", zipEntry{name: "SKILL.md", data: []byte("/etc/passwd"), header: func(h *zip.FileHeader) { h.SetMode(fs.ModeSymlink | 0o777) }}, "symbolic_link"},
		{"named pipe", zipEntry{name: "SKILL.md", header: func(h *zip.FileHeader) { h.SetMode(fs.ModeNamedPipe | 0o644) }}, "special_file"},
		{"device", zipEntry{name: "SKILL.md", header: func(h *zip.FileHeader) { h.SetMode(fs.ModeDevice | 0o644) }}, "special_file"},
		{"hard link", zipEntry{name: "SKILL.md", data: []byte("x"), header: func(h *zip.FileHeader) { h.Extra = pkware("/etc/passwd") }}, "hard_link"},
		{"encrypted", zipEntry{name: "SKILL.md", data: []byte("x"), header: func(h *zip.FileHeader) { h.Flags |= 0x1 }}, "encrypted_entry"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Arrange
			data := buildZip(t, c.entry)

			// Act
			v := skills.ValidateZip(data)

			// Assert
			if v.Valid || len(v.Issues) != 1 {
				t.Fatalf("validation %+v", v)
			}
			requireCode(t, v.Issues, c.code, "SKILL.md")
		})
	}
}

func TestReadZipIgnoresHarmlessExtraFieldsAndMalformedOnes(t *testing.T) {
	// Extended timestamp and NTFS-like records are skipped by size; a truncated record stops the scan; none is a hard link, so the skill loads.
	// Arrange
	record := func(tag uint16, size int, body ...byte) []byte {
		out := make([]byte, 4)
		binary.LittleEndian.PutUint16(out[0:], tag)
		binary.LittleEndian.PutUint16(out[2:], uint16(size))
		return append(out, body...)
	}
	extras := map[string][]byte{
		"timestamps":       record(0x5455, 5, 1, 0, 0, 0, 0),
		"pkware no target": record(0x000d, 12, make([]byte, 12)...),
		"truncated":        record(0x000d, 40, make([]byte, 5)...),
		"short":            {1, 2, 3},
	}
	for name, extra := range extras {
		t.Run(name, func(t *testing.T) {
			data := buildZip(t, zipEntry{name: "SKILL.md", data: []byte(skillMD("ok", "Fine.")), header: func(h *zip.FileHeader) { h.Extra = extra }})

			// Act
			v := skills.ValidateZip(data)

			// Assert
			if !v.Valid {
				t.Fatalf("issues %+v", v.Issues)
			}
		})
	}
}

func TestReadZipRejectsHostileArchives(t *testing.T) {
	// Not a zip, too many entries, traversal paths, oversized members and decompression bombs fail with a specific code before anything is built.
	big := func(n int) []byte { return make([]byte, n) }
	many := make([]zipEntry, skills.MaxEntries+1)
	for i := range many {
		many[i] = zipEntry{name: "f" + string(rune('a'+i%26)) + string(rune('a'+i/26%26)) + string(rune('a'+i/676)) + ".txt"}
	}
	cases := []struct {
		name string
		data []byte
		code string
		path string
	}{
		{"not a zip", []byte("this is not a zip archive"), "invalid_zip", ""},
		{"too many entries", buildZip(t, many...), "too_many_entries", ""},
		{"parent traversal", buildZip(t, zipEntry{name: "../evil.txt", data: []byte("x")}), "unsafe_path", "../evil.txt"},
		{"absolute path", buildZip(t, zipEntry{name: "/etc/passwd", data: []byte("x")}), "unsafe_path", "/etc/passwd"},
		{"windows device", buildZip(t, zipEntry{name: "SKILL.md"}, zipEntry{name: "scripts/NUL.txt"}), "unsafe_path", "scripts/NUL.txt"},
		{"oversized file", buildZip(t, zipEntry{name: "data.bin", data: big(skills.MaxFileBytes + 1)}), "file_too_large", "data.bin"},
		{"oversized SKILL.md", buildZip(t, zipEntry{name: "SKILL.md", data: big(skills.MaxSkillMDBytes + 1)}), "skill_md_too_large", "SKILL.md"},
		{"ratio bomb", buildZip(t, zipEntry{name: "SKILL.md", data: []byte(skillMD("bomb", "Zeros."))}, zipEntry{name: "pad.txt", data: big(8 << 20)}), "decompression_ratio_exceeded", ""},
		{"archive over the limit", big(skills.MaxArchiveBytes + 1), "archive_too_large", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Act
			v := skills.ValidateZip(c.data)
			_, err := skills.ParseZip(c.data)

			// Assert
			if v.Valid || v.Package != nil || len(v.Issues) != 1 {
				t.Fatalf("validation %+v", v)
			}
			requireCode(t, v.Issues, c.code, c.path)
			var inv *skills.InvalidError
			if !errors.As(err, &inv) || !strings.HasPrefix(inv.Error(), "invalid skill package: "+c.code+": ") {
				t.Fatalf("ParseZip error %v", err)
			}
		})
	}
}

func TestReadZipRejectsAnExpandedPackageOverTheLimit(t *testing.T) {
	// Members that each fit the file limit but together expand past 64 MiB, with a compression ratio under the bomb threshold, fail as package_too_large.
	// Arrange
	rnd := rand.New(rand.NewPCG(1, 2))
	chunk := make([]byte, skills.MaxFileBytes)
	for i := 0; i < len(chunk); i += 1100 {
		for j := i; j < min(i+100, len(chunk)); j++ {
			chunk[j] = byte(rnd.IntN(256)) // 100 random bytes per 1100: compresses about 10:1, cheaply
		}
	}
	entries := []zipEntry{{name: "SKILL.md", data: []byte(skillMD("huge", "Huge."))}}
	for _, n := range []string{"a", "b", "c", "d"} {
		entries = append(entries, zipEntry{name: n + ".bin", data: chunk})
	}
	data := buildZip(t, entries...)

	// Act
	v := skills.ValidateZip(data)

	// Assert
	if v.Valid || len(v.Issues) != 1 {
		t.Fatalf("validation %+v", v.Issues)
	}
	requireCode(t, v.Issues, "package_too_large", "")
}

func TestValidateFilesReportsEveryStructuralIssueAtOnce(t *testing.T) {
	// A package gets all its structural problems listed together, each with its path: duplicates after case folding, depth, native executables (by extension and by magic), nested and misplaced SKILL.md.
	// Arrange
	deep := strings.Repeat("d/", skills.MaxDirectoryDepth+1) + "f.txt"
	files := []skills.File{
		{Path: "SKILL.md", Data: []byte(skillMD("messy", "Messy."))},
		{Path: "Readme.md", Data: []byte("a")},
		{Path: "README.md", Data: []byte("b")},
		{Path: deep, Data: []byte("x")},
		{Path: "lib/helper.so", Data: []byte("text")},
		{Path: "bin/tool", Data: []byte("\x7fELF....")},
		{Path: "sub/skill.md", Data: []byte("nested")},
		{Path: "../escape.txt", Data: []byte("x")},
	}

	// Act
	v := skills.ValidateFiles(files)

	// Assert
	if v.Valid || v.Package != nil {
		t.Fatalf("valid: %+v", v)
	}
	requireCode(t, v.Issues, "duplicate_path", "README.md")
	requireCode(t, v.Issues, "max_depth_exceeded", deep)
	requireCode(t, v.Issues, "native_executable", "lib/helper.so")
	requireCode(t, v.Issues, "native_executable", "bin/tool")
	requireCode(t, v.Issues, "multiple_skill_md", "sub/skill.md")
	requireCode(t, v.Issues, "unsafe_path", "../escape.txt")
	if v.Metadata == nil || v.Metadata.Name != "messy" {
		t.Fatalf("the metadata of a readable SKILL.md is reported even when the package is invalid: %+v", v.Metadata)
	}
	if len(v.Entries) != len(files) || v.Revision == "" {
		t.Fatalf("entries %d revision %q", len(v.Entries), v.Revision)
	}
}

func TestValidateFilesLimitsEntriesAndSizes(t *testing.T) {
	// More than MaxEntries files, a file over 16 MiB, a SKILL.md over 1 MiB and a package over 64 MiB are each reported.
	// Arrange
	var many []skills.File
	for i := range skills.MaxEntries + 1 {
		many = append(many, skills.File{Path: "dir" + string(rune('a'+i%26)) + "/" + string(rune('a'+i/26%26)) + string(rune('a'+i/676)), Data: nil})
	}
	chunk := make([]byte, skills.MaxFileBytes)
	huge := []skills.File{{Path: "SKILL.md", Data: []byte(skillMD("huge", "Huge."))}, {Path: "toobig.bin", Data: make([]byte, skills.MaxFileBytes+1)}}
	for _, n := range []string{"a", "b", "c", "d"} {
		huge = append(huge, skills.File{Path: n + ".bin", Data: chunk})
	}

	// Act
	manyV := skills.ValidateFiles(many)
	hugeV := skills.ValidateFiles(huge)
	bigMD := skills.ValidateFiles([]skills.File{{Path: "SKILL.md", Data: make([]byte, skills.MaxSkillMDBytes+1)}})

	// Assert
	requireCode(t, manyV.Issues, "too_many_entries", "")
	requireCode(t, hugeV.Issues, "file_too_large", "toobig.bin")
	requireCode(t, hugeV.Issues, "package_too_large", "")
	requireCode(t, bigMD.Issues, "skill_md_too_large", "SKILL.md")
	if bigMD.Metadata != nil {
		t.Fatalf("an oversized SKILL.md is not parsed: %+v", bigMD.Metadata)
	}
}

func TestValidateFilesOfNothingReportsAMissingSkillMDWithoutRevision(t *testing.T) {
	// An empty package is missing SKILL.md and has no revision or entries.
	// Act
	v := skills.ValidateFiles(nil)

	// Assert
	if v.Valid || v.Revision != "" || len(v.Entries) != 0 || v.Metadata != nil {
		t.Fatalf("validation %+v", v)
	}
	requireCode(t, v.Issues, "missing_skill_md", "SKILL.md")
}

func TestValidateFilesReportsAMisplacedSkillMDWhenTheRootOneIsMissing(t *testing.T) {
	// A SKILL.md only inside a folder (not zipped as a skill) is misplaced; the root one is still required.
	// Act
	v := skills.ValidateFiles([]skills.File{{Path: "inner/SKILL.md", Data: []byte(skillMD("inner", "Inner."))}})

	// Assert
	requireCode(t, v.Issues, "missing_skill_md", "SKILL.md")
	requireCode(t, v.Issues, "misplaced_skill_md", "inner/SKILL.md")
}

func TestValidateManualJoinsUploadsAndReportsUnsafeOnes(t *testing.T) {
	// Manual authoring: uploads are normalized (backslashes, NFC); an unsafe upload makes the package invalid and is listed first; invalid UTF-8 in the editor text is rejected outright.
	// Arrange
	md := skillMD("manual", "Manual skill.")

	// Act
	ok := skills.ValidateManual(md, []skills.Upload{{Path: `scripts\run.py`, Data: []byte("1")}, {Path: "refs/cafe\u0301.md", Data: []byte("2")}})
	bad := skills.ValidateManual(md, []skills.Upload{{Path: "../x", Data: []byte("1")}, {Path: "C:/evil", Data: nil}, {Path: "fine.txt", Data: []byte("1")}})
	badUTF8 := skills.ValidateManual("---\nname: x\n---\n\xff", nil)

	// Assert
	if !ok.Valid || !slices.Equal(ok.Package.Paths(), []string{"SKILL.md", "refs/caf\u00e9.md", "scripts/run.py"}) {
		t.Fatalf("manual %+v", ok)
	}
	if ok.Metadata == nil || ok.Metadata.Name != "manual" || ok.Package.Size == 0 {
		t.Fatalf("metadata %+v size %d", ok.Metadata, ok.Package.Size)
	}
	if bad.Valid || bad.Package != nil || !slices.Equal(codes(bad.Issues), []string{"unsafe_path", "unsafe_path"}) || bad.Issues[0].Path != "../x" {
		t.Fatalf("unsafe uploads: %+v", bad)
	}
	if bad.Metadata == nil || bad.Metadata.Name != "manual" {
		t.Fatalf("metadata of the valid SKILL.md still reported: %+v", bad.Metadata)
	}
	if badUTF8.Valid || len(badUTF8.Issues) != 1 {
		t.Fatalf("invalid utf-8: %+v", badUTF8)
	}
	requireCode(t, badUTF8.Issues, "invalid_utf8", "SKILL.md")
}

func TestNormalizedPathRules(t *testing.T) {
	// Package paths are NFC, POSIX, relative, without traversal, drive letters, reserved Windows names, control characters, trailing dots/spaces or colons.
	accepted := []string{"scripts/run.py", "a/b/c.txt", "name with space.md", "..hidden/ok", "COM10.txt", "LPT0.txt"}
	rejected := []string{"", "  ", "/abs", "a//b", "a/./b", "a/../b", "./a", "C:/x", "c:x", "con", "NUL.txt", "dir/aux", "com1", "lpt\u00b2.log", "file.", "dir /x", "a:b",
		"tab\there", "nul\x00byte", "conin$", "dir/COM1.tar.gz"}
	for _, p := range accepted {
		v := skills.ValidateManual(skillMD("n", "d"), []skills.Upload{{Path: p, Data: []byte("x")}})
		if !v.Valid {
			t.Errorf("%q rejected: %+v", p, v.Issues)
		}
	}
	for _, p := range rejected {
		v := skills.ValidateManual(skillMD("n", "d"), []skills.Upload{{Path: p, Data: []byte("x")}})
		if v.Valid {
			t.Errorf("%q accepted", p)
		}
	}
}

func TestNativeExecutablesAreRefusedByMagicNumberToo(t *testing.T) {
	// A renamed binary is detected by its header (PE, ELF, Mach-O, fat Mach-O), not only by extension.
	for _, magic := range []string{"MZ\x90\x00", "\x7fELF\x02", "\xfe\xed\xfa\xce", "\xce\xfa\xed\xfe", "\xfe\xed\xfa\xcf", "\xcf\xfa\xed\xfe", "\xca\xfe\xba\xbe", "\xbe\xba\xfe\xca"} {
		// Act
		v := skills.ValidateManual(skillMD("n", "d"), []skills.Upload{{Path: "tools/data.txt", Data: []byte(magic + "rest")}})

		// Assert
		if v.Valid {
			t.Errorf("magic %q accepted", magic)
			continue
		}
		requireCode(t, v.Issues, "native_executable", "tools/data.txt")
	}
	for _, ext := range []string{"x.DLL", "x.dylib", "x.exe", "x.o", "x.elf"} {
		if v := skills.ValidateManual(skillMD("n", "d"), []skills.Upload{{Path: ext, Data: []byte("text")}}); v.Valid {
			t.Errorf("extension of %s accepted", ext)
		}
	}
}

func TestSkillMDFrontmatterIsValidated(t *testing.T) {
	// Each malformed SKILL.md frontmatter is rejected with its own code and the SKILL.md path; metadata is reported only when the frontmatter is sound.
	cases := []struct {
		name string
		md   string
		code string
	}{
		{"no frontmatter", "# Title\n", "invalid_frontmatter"},
		{"empty file", "", "invalid_frontmatter"},
		{"unclosed", "---\nname: a\ndescription: b\n", "invalid_frontmatter"},
		{"invalid yaml", "---\nname: [unclosed\n---\n", "invalid_frontmatter"},
		{"empty mapping", "---\n---\nBody", "invalid_frontmatter"},
		{"missing name", "---\ndescription: d\n---\n", "missing_name"},
		{"missing description", "---\nname: n\n---\n", "missing_description"},
		{"blank name", "---\nname: \"  \"\ndescription: d\n---\n", "missing_name"},
		{"non-string description", "---\nname: n\ndescription: 12\n---\n", "invalid_metadata"},
		{"name without letters", "---\nname: \"!!!\"\ndescription: d\n---\n", "invalid_name"},
		{"name too long", "---\nname: " + strings.Repeat("a", skills.MaxNameBytes+1) + "\ndescription: d\n---\n", "metadata_too_large"},
		{"description too long", "---\nname: n\ndescription: " + strings.Repeat("a", skills.MaxDescriptionBytes+1) + "\n---\n", "metadata_too_large"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Act
			v := skills.ValidateManual(c.md, nil)

			// Assert
			if v.Valid || v.Package != nil || v.Metadata != nil {
				t.Fatalf("validation %+v", v)
			}
			requireCode(t, v.Issues, c.code, "SKILL.md")
		})
	}
}

func TestSkillMDAcceptsBOMCRLFAndLicenseAndRequires(t *testing.T) {
	// A BOM and CRLF line endings are tolerated; license is trimmed; requires accepts a comma separated string or a list, as deduplicated slugs, ignoring non-strings.
	// Arrange
	crlf := "\ufeff---\r\nname: Fancy Name\r\ndescription: Does X\r\nlicense: \"  MIT  \"\r\nrequires: [Docx, \"Shared Kit\", docx, 3, \"!!\"]\r\n---\r\n\r\nBody\r\n"
	str := "---\nname: s\ndescription: d\nrequires: \"pdf, XLSX ,pdf\"\n---\n"
	odd := "---\nname: s\ndescription: d\nrequires: {a: b}\n---\n"

	// Act
	a := skills.ValidateManual(crlf, nil)
	b := skills.ValidateManual(str, nil)
	c := skills.ValidateManual(odd, nil)

	// Assert
	if !a.Valid || a.Package.License != "MIT" || a.Package.Name != "Fancy Name" || a.Package.Body != "Body\n" {
		t.Fatalf("crlf: %+v", a)
	}
	if !slices.Equal(a.Package.Requires, []string{"docx", "shared-kit"}) {
		t.Fatalf("requires %v", a.Package.Requires)
	}
	if !b.Valid || !slices.Equal(b.Package.Requires, []string{"pdf", "xlsx"}) {
		t.Fatalf("string requires: %+v", b.Package)
	}
	if !c.Valid || len(c.Package.Requires) != 0 {
		t.Fatalf("a mapping is ignored: %+v", c.Package)
	}
}

func TestPlainFrontmatterScalarsKeepTheirColonsAndFoldedLines(t *testing.T) {
	// A plain description with ": " or a folded continuation is read as the text written (not a nested mapping), while quoted and block values are left to YAML.
	// Arrange
	colon := "---\nname: c\ndescription: Use when: the user asks for X\n---\n"
	folded := "---\nname: f\ndescription: Starts here\n  and goes on # not a comment\n---\n"
	quoted := "---\nname: q\ndescription: \"Quoted: stays\"\n---\n"
	block := "---\nname: b\ndescription: |\n  line one\n  line two\n---\n"
	tab := "---\nname: t\ndescription: a\t# hash\n---\n"

	// Act
	got := map[string]skills.Validation{}
	for k, md := range map[string]string{"colon": colon, "folded": folded, "quoted": quoted, "block": block, "tab": tab} {
		got[k] = skills.ValidateManual(md, nil)
	}

	// Assert
	want := map[string]string{
		"colon":  "Use when: the user asks for X",
		"folded": "Starts here and goes on # not a comment",
		"quoted": "Quoted: stays",
		"block":  "line one\nline two",
		"tab":    "a\t# hash",
	}
	for k, w := range want {
		if !got[k].Valid || got[k].Metadata.Description != w {
			t.Errorf("%s: valid=%v description %q, want %q", k, got[k].Valid, got[k].Metadata, w)
		}
	}
}

func TestPackageUsesNamesTheSkillsItsNonMarkdownFilesReference(t *testing.T) {
	// Uses lists the other skills the package's scripts name as /skills/<name>/ (not itself, not the markdown, not binary files), in order, once.
	// Arrange
	files := []skills.File{
		{Path: "SKILL.md", Data: []byte(skillMD("mine", "Mine.") + "\nSee /skills/docs-only/ in the markdown.\n")},
		{Path: "scripts/a.py", Data: []byte("import sys\nsys.path.insert(0, '/skills/shared-kit/scripts')\nopen('/skills/mine/x')\n")},
		{Path: "scripts/b.py", Data: []byte("p = '/skills/pdf/scripts' ; q = '/skills/shared-kit/'\n")},
		{Path: "data.bin", Data: []byte("\xff\xfe /skills/binary/ \x00")},
		{Path: "README.MD", Data: []byte("/skills/upper-md/")},
	}

	// Act
	v := skills.ValidateFiles(files)

	// Assert
	if !v.Valid {
		t.Fatal(v.Issues)
	}
	if !slices.Equal(v.Package.Uses, []string{"shared-kit", "pdf"}) {
		t.Fatalf("uses %v", v.Package.Uses)
	}
}

func TestPackageRevisionIsStableAcrossFileOrderAndChangesWithContent(t *testing.T) {
	// The revision digests sorted (path, content) pairs: file order does not matter, a one byte change does.
	// Arrange
	a := skills.File{Path: "SKILL.md", Data: []byte(skillMD("rev", "Rev."))}
	b := skills.File{Path: "b.txt", Data: []byte("1")}
	c := skills.File{Path: "a.txt", Data: []byte("2")}

	// Act
	one := skills.ValidateFiles([]skills.File{a, b, c})
	two := skills.ValidateFiles([]skills.File{c, b, a})
	changed := skills.ValidateFiles([]skills.File{a, {Path: "b.txt", Data: []byte("3")}, c})

	// Assert
	if one.Revision != two.Revision || one.Package.Revision != one.Revision {
		t.Fatalf("revisions %s %s", one.Revision, two.Revision)
	}
	if one.Revision == changed.Revision {
		t.Fatal("content change not reflected in the revision")
	}
	if got := one.Package.Paths(); !slices.Equal(got, []string{"SKILL.md", "a.txt", "b.txt"}) {
		t.Fatalf("files not sorted: %v", got)
	}
}

func TestLoadFSSkipsHiddenAndCompiledFiles(t *testing.T) {
	// Reading a skill directory leaves out dot files and folders, __pycache__ and .pyc files, as a built-in skill folder may contain them.
	// Arrange
	fsys := fstest.MapFS{
		"SKILL.md":                  {Data: []byte(skillMD("tidy", "Tidy."))},
		"scripts/run.py":            {Data: []byte("1")},
		"scripts/run.pyc":           {Data: []byte("x")},
		"scripts/__pycache__/a.pyc": {Data: []byte("x")},
		".git/config":               {Data: []byte("x")},
		".DS_Store":                 {Data: []byte("x")},
	}

	// Act
	pkg, err := skills.LoadFS(fsys)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if got := pkg.Paths(); !slices.Equal(got, []string{"SKILL.md", "scripts/run.py"}) {
		t.Fatalf("paths %v", got)
	}
}

type failingFS struct{ fstest.MapFS }

func (f failingFS) ReadFile(name string) ([]byte, error) {
	if name == "scripts/broken.py" {
		return nil, errors.New("disk error")
	}
	return f.MapFS.ReadFile(name)
}

func (f failingFS) Open(name string) (fs.File, error) {
	if name == "scripts/broken.py" {
		return nil, errors.New("disk error")
	}
	return f.MapFS.Open(name)
}

func TestLoadFSFailsOnAnUnreadableFileAndAnInvalidPackage(t *testing.T) {
	// A read error aborts the load with that error; a readable but invalid skill is an InvalidError listing its issues.
	// Arrange
	broken := failingFS{fstest.MapFS{"SKILL.md": {Data: []byte(skillMD("x", "X."))}, "scripts/broken.py": {Data: []byte("1")}}}
	invalid := fstest.MapFS{"SKILL.md": {Data: []byte("no frontmatter")}}

	// Act
	_, readErr := skills.LoadFS(broken)
	_, invErr := skills.LoadFS(invalid)

	// Assert
	if readErr == nil || !strings.Contains(readErr.Error(), "disk error") {
		t.Fatalf("read error %v", readErr)
	}
	var inv *skills.InvalidError
	if !errors.As(invErr, &inv) || len(inv.Issues) != 1 || inv.Issues[0].Code != "invalid_frontmatter" || len(inv.Details) != 1 {
		t.Fatalf("invalid error %v", invErr)
	}
}

func TestInvalidErrorDetailsNameThePathUnlessTheMessageAlreadyDoes(t *testing.T) {
	// The flattened error text is "code: message (path)" for each issue; the path is left out when the message already names it, and for issues without one.
	// Arrange
	named := buildZip(t, zipEntry{name: "SKILL.md", data: []byte("nope")})
	unsafe := buildZip(t, zipEntry{name: "../x", data: []byte("1")})

	// Act
	_, errNamed := skills.ParseZip(named)
	_, errUnsafe := skills.ParseZip(unsafe)
	_, errNotZip := skills.ParseZip([]byte("junk"))

	// Assert
	want := map[string]struct {
		err    error
		detail string
	}{
		"message names the path": {errNamed, "invalid_frontmatter: SKILL.md must begin with YAML frontmatter."},
		"path appended":          {errUnsafe, "unsafe_path: Package paths must be safe and package-relative. (../x)"},
		"no path":                {errNotZip, "invalid_zip: Upload a valid ZIP archive."},
	}
	for name, w := range want {
		var inv *skills.InvalidError
		if !errors.As(w.err, &inv) || len(inv.Details) != 1 || inv.Details[0] != w.detail {
			t.Errorf("%s: err %v, want detail %q", name, w.err, w.detail)
			continue
		}
		if inv.Error() != "invalid skill package: "+w.detail {
			t.Errorf("%s: Error() = %q", name, inv.Error())
		}
	}
}

func TestSlug(t *testing.T) {
	// The identifier of a name: lowercase ASCII letters and digits joined by single hyphens, accents removed, at most 64 characters, "" without any letter or digit.
	cases := map[string]string{
		"docx":                        "docx",
		"My Cool_Skill!":              "my-cool-skill",
		"  --Leading and trailing-- ": "leading-and-trailing",
		"Café Déjà Vu":                "cafe-deja-vu",
		"a    b":                      "a-b",
		"日本語":                         "",
		"!!!":                         "",
		"":                            "",
		"v2.0 Beta":                   "v2-0-beta",
		strings.Repeat("ab ", 40):     strings.TrimRight(strings.Repeat("ab-", 40)[:skills.MaxSlugChars], "-"),
	}
	for in, want := range cases {
		if got := skills.Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMediaTypeGuessesFromTheExtension(t *testing.T) {
	// SKILL.md is markdown whatever the extension table says, known extensions get their type and unknown ones are octet-stream.
	cases := map[string]string{
		"SKILL.md":         "text/markdown",
		"scripts/page.png": "image/png",
		"x.unknownext":     "application/octet-stream",
		"noext":            "application/octet-stream",
	}
	for p, want := range cases {
		if got := skills.MediaType(p); got != want {
			t.Errorf("MediaType(%q) = %q, want %q", p, got, want)
		}
	}
}

func TestDigestIsTheHexSHA256(t *testing.T) {
	// Digest is the standard hex sha256 (known vector for "abc").
	if got, want := skills.Digest([]byte("abc")), "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"; got != want {
		t.Fatalf("digest %s", got)
	}
}

func TestBuiltinSkillsLoadAsValidPackages(t *testing.T) {
	// Every shipped skill folder validates as a package (guards the embedded content against the package rules).
	// Arrange
	root := os.DirFS("skillsfs/builtin")
	entries, err := fs.ReadDir(root, ".")
	if err != nil {
		t.Fatal(err)
	}

	// Act / Assert
	checked := 0
	for _, e := range entries {
		if _, err := fs.Stat(root, e.Name()+"/SKILL.md"); err != nil {
			continue
		}
		sub, _ := fs.Sub(root, e.Name())
		pkg, err := skills.LoadFS(sub)
		if err != nil || skills.Slug(pkg.Name) != e.Name() {
			t.Errorf("%s: %v", e.Name(), err)
		}
		checked++
	}
	if checked < 5 {
		t.Fatalf("only %d built-in skills checked", checked)
	}
}

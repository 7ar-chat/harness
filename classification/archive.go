package classification

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// ArchiveLimits are the safety limits of an archive scan.
type ArchiveLimits struct {
	MaxEntries    int   // per archive
	MaxFileBytes  int64 // per entry
	MaxTotalBytes int64 // expanded, nested archives included
	MaxRatio      int64 // expanded bytes / archive bytes
	MaxDepth      int   // nested archives
}

// DefaultArchiveLimits are the skill package limits (ai/cowork/skill_packages.py).
var DefaultArchiveLimits = ArchiveLimits{MaxEntries: 500, MaxFileBytes: 16 << 20, MaxTotalBytes: 64 << 20, MaxRatio: 100, MaxDepth: 3}

// ErrArchiveUnreadable means an archive could not be checked within the limits
// (invalid, encrypted entries, too many entries, too large, too deep, or a
// format that cannot be scanned such as 7z or RAR).
var ErrArchiveUnreadable = errors.New("the archive cannot be checked")

type archiveKind int

const (
	notArchive archiveKind = iota
	zipArchive
	tarArchive
	gzipArchive
	bzip2Archive
	unscannableArchive
)

// unscannable are archive formats the scan cannot open: they are refused (C-20).
var unscannable = []struct {
	name  string
	magic []byte
}{
	{"7z", []byte("7z\xbc\xaf\x27\x1c")},
	{"RAR", []byte("Rar!\x1a\x07")},
	{"xz", []byte("\xfd7zXZ\x00")},
	{"zstd", []byte{0x28, 0xb5, 0x2f, 0xfd}},
	{"lzip", []byte("LZIP")},
	{"cab", []byte("MSCF\x00\x00\x00\x00")},
}

// kindOf detects an archive from its bytes (never from its name): zip by its
// end-of-central-directory record (a junk prefix is allowed, like unzip),
// tar by its ustar header, gzip and bzip2 by their magic. OOXML packages and
// OpenDocument files are documents, not archives (their label is checked).
func kindOf(data []byte) (archiveKind, string) {
	switch {
	case bytes.HasPrefix(data, []byte{0x1f, 0x8b}):
		return gzipArchive, "gzip"
	case len(data) >= 10 && bytes.HasPrefix(data, []byte("BZh")) && data[3] >= '1' && data[3] <= '9' &&
		(bytes.Equal(data[4:10], []byte("1AY&SY")) || bytes.Equal(data[4:10], []byte("\x17rE8P\x90"))):
		return bzip2Archive, "bzip2"
	case len(data) >= 262 && bytes.Equal(data[257:262], []byte("ustar")):
		return tarArchive, "tar"
	}
	for _, u := range unscannable {
		if bytes.HasPrefix(data, u.magic) {
			return unscannableArchive, u.name
		}
	}
	if zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data))); err == nil {
		if isDocumentContainer(zr) {
			return notArchive, ""
		}
		return zipArchive, "zip"
	} else if bytes.HasPrefix(data, []byte("PK\x03\x04")) || bytes.HasPrefix(data, []byte("PK\x05\x06")) {
		return zipArchive, "zip" // a broken zip: the scan reports it unreadable
	}
	return notArchive, ""
}

// IsArchive reports bytes the upload scan must open: zip (also with a prefix),
// tar, gzip, bzip2 — and 7z, RAR, xz, zstd, lzip and cab, which it refuses.
// OOXML (any variant) and OpenDocument files are not archives.
func IsArchive(data []byte) bool {
	k, _ := kindOf(data)
	return k != notArchive
}

// ScanArchive returns the path ("inner.zip/dir/file.pdf" for nested archives)
// of the first entry DocumentDisallowed refuses, "" when none.
func ScanArchive(data []byte, lim ArchiveLimits) (string, error) {
	budget := lim.MaxTotalBytes
	if r := int64(len(data)) * lim.MaxRatio; lim.MaxRatio > 0 && r < budget {
		budget = r
	}
	return scanAny(data, "", lim, 0, &budget)
}

func scanAny(data []byte, prefix string, lim ArchiveLimits, depth int, budget *int64) (string, error) {
	switch kind, name := kindOf(data); kind {
	case zipArchive:
		return scanZip(data, prefix, lim, depth, budget)
	case tarArchive:
		return scanTar(bytes.NewReader(data), prefix, lim, depth, budget)
	case gzipArchive:
		gz, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return "", fmt.Errorf("%w: %s is not a valid gzip stream", ErrArchiveUnreadable, orRoot(prefix))
		}
		defer gz.Close()
		member := gz.Name
		return scanStream(gz, prefix, member, lim, depth, budget)
	case bzip2Archive:
		return scanStream(bzip2.NewReader(bytes.NewReader(data)), prefix, "", lim, depth, budget)
	case unscannableArchive:
		return "", fmt.Errorf("%w: %s is a %s archive, which cannot be checked", ErrArchiveUnreadable, orRoot(prefix), name)
	}
	return "", nil
}

// scanStream checks the content of a compressed stream (gzip, bzip2): a tar
// is scanned entry by entry, any other content as one file.
func scanStream(r io.Reader, prefix, member string, lim ArchiveLimits, depth int, budget *int64) (string, error) {
	content, err := readBounded(r, prefix+member, lim, budget)
	if err != nil {
		return "", err
	}
	if k, _ := kindOf(content); k == tarArchive {
		return scanTar(bytes.NewReader(content), prefix, lim, depth, budget)
	}
	if member == "" {
		member = "content"
	}
	return checkEntry(prefix+path.Base("/"+member), content, lim, depth, budget)
}

// readBounded reads an entry within the per-entry and total limits.
func readBounded(r io.Reader, name string, lim ArchiveLimits, budget *int64) ([]byte, error) {
	content, err := io.ReadAll(io.LimitReader(r, min(lim.MaxFileBytes, *budget)+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %s cannot be read", ErrArchiveUnreadable, orRoot(name))
	}
	if int64(len(content)) > lim.MaxFileBytes || int64(len(content)) > *budget {
		return nil, fmt.Errorf("%w: %s exceeds the size limits", ErrArchiveUnreadable, orRoot(name))
	}
	*budget -= int64(len(content))
	return content, nil
}

// checkEntry applies the document rule to one entry and recurses into nested archives.
func checkEntry(name string, content []byte, lim ArchiveLimits, depth int, budget *int64) (string, error) {
	if DocumentDisallowed(name, content) {
		return name, nil
	}
	if IsArchive(content) {
		if depth+1 > lim.MaxDepth {
			return "", fmt.Errorf("%w: %s nests archives too deeply", ErrArchiveUnreadable, name)
		}
		return scanAny(content, name+"/", lim, depth+1, budget)
	}
	return "", nil
}

func entryName(prefix, raw string) string {
	return prefix + strings.TrimPrefix(path.Clean("/"+strings.ReplaceAll(raw, "\\", "/")), "/")
}

func scanZip(data []byte, prefix string, lim ArchiveLimits, depth int, budget *int64) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("%w: %s is not a valid zip", ErrArchiveUnreadable, orRoot(prefix))
	}
	if len(zr.File) > lim.MaxEntries {
		return "", fmt.Errorf("%w: %s has more than %d entries", ErrArchiveUnreadable, orRoot(prefix), lim.MaxEntries)
	}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || strings.HasSuffix(f.Name, "/") {
			continue
		}
		name := entryName(prefix, f.Name)
		if f.Flags&0x1 != 0 {
			return "", fmt.Errorf("%w: %s is encrypted", ErrArchiveUnreadable, name)
		}
		rc, err := f.Open()
		if err != nil {
			return "", fmt.Errorf("%w: %s cannot be read", ErrArchiveUnreadable, name)
		}
		content, err := readBounded(rc, name, lim, budget)
		rc.Close()
		if err != nil {
			return "", err
		}
		if found, err := checkEntry(name, content, lim, depth, budget); found != "" || err != nil {
			return found, err
		}
	}
	return "", nil
}

func scanTar(r io.Reader, prefix string, lim ArchiveLimits, depth int, budget *int64) (string, error) {
	tr := tar.NewReader(r)
	for n := 0; ; n++ {
		h, err := tr.Next()
		if err == io.EOF {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("%w: %s is not a valid tar", ErrArchiveUnreadable, orRoot(prefix))
		}
		if n >= lim.MaxEntries {
			return "", fmt.Errorf("%w: %s has more than %d entries", ErrArchiveUnreadable, orRoot(prefix), lim.MaxEntries)
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != 0 { // 0: pre-POSIX regular file
			continue // directories, links, devices carry no content
		}
		name := entryName(prefix, h.Name)
		content, err := readBounded(tr, name, lim, budget)
		if err != nil {
			return "", err
		}
		if found, err := checkEntry(name, content, lim, depth, budget); found != "" || err != nil {
			return found, err
		}
	}
}

func orRoot(prefix string) string {
	if prefix == "" {
		return "the archive"
	}
	return strings.TrimSuffix(prefix, "/")
}

// NeedsInspection reports, from the first bytes of a file (≥ 1 KiB when
// available) and its last bytes (≥ 64 KiB + 22 when available), whether its
// confidentiality must be checked on the whole content: a PDF header within
// the first KiB, an OLE2, zip, gzip, bzip2, tar or unscannable-archive
// signature, or a zip end-of-central-directory record (a zip with a prefix).
func NeedsInspection(head, tail []byte) bool {
	if isPDFContent(head) || bytes.HasPrefix(head, oleHeader) || bytes.HasPrefix(head, []byte("PK")) {
		return true
	}
	if k, _ := kindOf(head); k != notArchive {
		return true
	}
	return bytes.Contains(tail, []byte("PK\x05\x06"))
}

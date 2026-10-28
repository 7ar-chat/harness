package classification

import (
	"archive/zip"
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
	"github.com/richardlehane/mscfb"
)

// Port of studio_django_sdk.security.confidentiality (the reader Studio Work
// applies to uploads), made fail-closed (roadmap 0.4):
//   - the container is chosen from the bytes, never from the file name
//     (C-05: .docm/.xlsm/.pptm/.dotx/.xltx/.potx, ODF, renamed files);
//   - every label is collected and the most restrictive one decides (C-06);
//   - PDF labels are read from the header lines, the document information
//     dictionary and the XMP metadata (C-04).

var (
	rmsMarkers = []string{"encryptioninfo", "encryptedpackage", "rightsmanagement"}
	oleStreams = []string{"DRTransform", "Metadata", "DRMEncryptedTransform"}
	nameTag    = regexp.MustCompile(`(?s)<NAME>(.*?)</NAME>`)
	pdfLabel   = regexp.MustCompile(`.*/MSIP_Label_.*_Name\(C.*`)
	msipName   = regexp.MustCompile(`^MSIP_Label_.+_Name$`)
	// pdfRawLabel is a label literal anywhere in the file (uncompressed Info dictionary).
	pdfRawLabel = regexp.MustCompile(`/MSIP_Label_[A-Za-z0-9_-]+?_Name\s*\(((?:\\.|[^\\)]){0,256})\)`)
	// xmpLabel is a label in XMP metadata (element or attribute form).
	xmpLabel = regexp.MustCompile(`MSIP_Label_[A-Za-z0-9-]+_Name\s*(?:>|=\s*["'])([^<"']{0,256})`)
)

// ErrLabelUnreadable means a document's label part exists but cannot be read
// within the limits; the document is then treated as disallowed (fail closed).
var ErrLabelUnreadable = errors.New("the document's confidentiality label cannot be read")

// maxLabelPartBytes bounds a label-carrying part (custom.xml, meta.xml, XMP).
const maxLabelPartBytes = 4 << 20

// ConfidentialityLabel returns the most restrictive MSIP label of a document
// ("" when none), defaulting to "C3-Amundi-Only" for RMS-protected Office files
// without a readable label. The container is detected from the content; the
// filename is ignored (kept for callers). An unreadable label part yields
// "C3-Amundi-Only" (fail closed).
func ConfidentialityLabel(_ string, content []byte) string {
	labels, err := ConfidentialityLabels(content)
	if err != nil {
		return "C3-Amundi-Only"
	}
	return mostRestrictive(labels)
}

// ConfidentialityLabels returns every MSIP label name found in a PDF, OOXML
// (any variant), OpenDocument or OLE2 document, sorted, deduplicated.
func ConfidentialityLabels(content []byte) ([]string, error) {
	var labels []string
	var err error
	switch {
	case isPDFContent(content):
		labels = pdfLabels(content)
	case bytes.HasPrefix(content, oleHeader):
		if l := oleLabel(content); l != "" {
			labels = append(labels, l)
		}
		if len(labels) == 0 && rmsProtected(content) {
			labels = append(labels, "C3-Amundi-Only")
		}
	default:
		if zr, zerr := zip.NewReader(bytes.NewReader(content), int64(len(content))); zerr == nil && isDocumentContainer(zr) {
			labels, err = zipDocumentLabels(zr)
			if err == nil && len(labels) == 0 && rmsProtected(content) {
				labels = append(labels, "C3-Amundi-Only")
			}
		}
	}
	return dedupe(labels), err
}

// DocumentDisallowed reports a document whose confidentiality label Studio
// refuses (C4, C3-Amundi-Only), whatever its name: any of its labels is
// disallowed, or its label part cannot be read (fail closed).
func DocumentDisallowed(_ string, data []byte) bool {
	labels, err := ConfidentialityLabels(data)
	if err != nil {
		return true
	}
	for _, l := range labels {
		if ConfidentialityDisallowed(l) {
			return true
		}
	}
	return false
}

// ErrSourceNotAllowed refuses a derived file (render, PDF conversion, extracted
// text) of a document whose label is disallowed: the derivation drops the label
// and delivery would stamp the default one (harness v3 "Review round 1").
var ErrSourceNotAllowed = errors.New("the source document's confidentiality label (C4 or C3-Amundi-Only) does not allow derived files")

// SourceAllowed reports whether files may be derived from a document (page
// images, PDF conversions, extracted text): false when DocumentDisallowed
// (any disallowed label, or an unreadable label part). Content without a
// document container (text, images) is allowed.
func SourceAllowed(content []byte) bool {
	return !DocumentDisallowed("", content)
}

// ConfidentialityDisallowed reports labels Studio refuses (C4, C3-Amundi-Only).
func ConfidentialityDisallowed(label string) bool {
	n := strings.ToLower(strings.TrimSpace(label))
	if n == "" {
		return false
	}
	return strings.Contains(n, "c4") || strings.Contains(n, "c3-amundi-only") ||
		(strings.Contains(n, "c3") && strings.Contains(n, "cannot leave amundi"))
}

// restrictiveness orders labels: disallowed > C3 > C2 > C1 > other.
func restrictiveness(label string) int {
	n := strings.ToLower(label)
	switch {
	case ConfidentialityDisallowed(label):
		return 4
	case strings.Contains(n, "c3"):
		return 3
	case strings.Contains(n, "c2"):
		return 2
	case strings.Contains(n, "c1"):
		return 1
	}
	return 0
}

func mostRestrictive(labels []string) string {
	best := ""
	for _, l := range labels {
		if best == "" || restrictiveness(l) > restrictiveness(best) {
			best = l
		}
	}
	return best
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range in {
		l = strings.TrimSpace(strings.ToValidUTF8(l, "�"))
		if l != "" && !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out
}

// isPDFContent: readers accept the header within the first 1024 bytes.
func isPDFContent(content []byte) bool {
	head := content[:min(len(content), 1024)]
	return bytes.Contains(head, []byte("%PDF-"))
}

// isDocumentContainer reports an OOXML package (any variant: [Content_Types].xml)
// or an OpenDocument file (mimetype entry), as opposed to a user zip.
func isDocumentContainer(zr *zip.Reader) bool {
	for i, f := range zr.File {
		if f.Name == contentTypesPart {
			return true
		}
		if i == 0 && f.Name == "mimetype" && f.UncompressedSize64 < 256 {
			if data, err := readEntryBounded(f, 256); err == nil && bytes.HasPrefix(data, []byte("application/vnd.oasis.opendocument")) {
				return true
			}
		}
	}
	return false
}

// zipDocumentLabels reads the MSIP label names of an OOXML package
// (docProps/custom.xml, or the part the package relationships name) and of an
// OpenDocument file (meta.xml user-defined properties), without validating the
// rest of the package.
func zipDocumentLabels(zr *zip.Reader) ([]string, error) {
	parts := map[string]bool{strings.ToLower(customPropertiesPart): true, "meta.xml": true}
	for _, f := range zr.File {
		if f.Name != packageRelationships {
			continue
		}
		data, err := readEntryBounded(f, maxLabelPartBytes)
		if err != nil {
			return nil, ErrLabelUnreadable
		}
		root, err := parseTree(data)
		if err != nil {
			return nil, ErrLabelUnreadable
		}
		for _, r := range root.Children {
			if typ, _ := attr(r, "Type"); typ == customRelationship {
				target, _ := attr(r, "Target")
				parts[strings.ToLower(strings.TrimPrefix(target, "/"))] = true
			}
		}
	}
	var labels []string
	for _, f := range zr.File {
		if !parts[strings.ToLower(f.Name)] {
			continue
		}
		data, err := readEntryBounded(f, maxLabelPartBytes)
		if err != nil {
			return nil, ErrLabelUnreadable
		}
		found, err := xmlPropertyLabels(data)
		if err != nil {
			return nil, ErrLabelUnreadable
		}
		labels = append(labels, found...)
	}
	return labels, nil
}

// xmlPropertyLabels returns the values of the properties named
// MSIP_Label_*_Name: OOXML <property name="…"><vt:lpwstr>v</…></property> and
// ODF <meta:user-defined meta:name="…">v</meta:user-defined>.
func xmlPropertyLabels(data []byte) ([]string, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	var labels []string
	depth, capture := 0, -1
	var text strings.Builder
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return labels, nil
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if capture < 0 && (t.Name.Local == "property" || t.Name.Local == "user-defined") {
				for _, a := range t.Attr {
					if a.Name.Local == "name" && msipName.MatchString(a.Value) {
						capture = depth
						text.Reset()
					}
				}
			}
		case xml.CharData:
			if capture > 0 && text.Len() < 1024 {
				text.Write(t)
			}
		case xml.EndElement:
			if depth == capture {
				labels = append(labels, text.String())
				capture = -1
			}
			depth--
		}
	}
}

func oleLabel(content []byte) string {
	if !bytes.HasPrefix(content, oleHeader) {
		return ""
	}
	doc, err := mscfb.New(bytes.NewReader(content))
	if err != nil {
		return ""
	}
	for entry, err := doc.Next(); err == nil; entry, err = doc.Next() {
		full := strings.Join(append(append([]string(nil), entry.Path...), entry.Name), "/")
		match := false
		for _, s := range oleStreams {
			if strings.Contains(full, s) {
				match = true
				break
			}
		}
		if !match {
			continue
		}
		data, rerr := io.ReadAll(io.LimitReader(entry, 16<<20))
		if rerr != nil {
			return ""
		}
		start := bytes.Index(data, []byte("<?xml"))
		if start < 0 {
			return ""
		}
		if m := nameTag.FindSubmatch(data[start:]); m != nil {
			return strings.ToValidUTF8(string(m[1]), "�")
		}
		return ""
	}
	return ""
}

func rmsProtected(content []byte) bool {
	if zr, err := zip.NewReader(bytes.NewReader(content), int64(len(content))); err == nil {
		for _, f := range zr.File {
			name := strings.ToLower(f.Name)
			for _, m := range rmsMarkers {
				if strings.Contains(name, m) {
					return true
				}
			}
		}
	}
	if !bytes.HasPrefix(content, oleHeader) {
		return false
	}
	doc, err := mscfb.New(bytes.NewReader(content))
	if err != nil {
		return false
	}
	for entry, err := doc.Next(); err == nil; entry, err = doc.Next() {
		full := strings.ToLower(strings.Join(append(append([]string(nil), entry.Path...), entry.Name), "/"))
		for _, m := range rmsMarkers {
			if strings.Contains(full, m) {
				return true
			}
		}
	}
	return false
}

// pdfLabels collects the labels of a PDF: the header lines (the Python
// reader), label literals anywhere in the file, XMP metadata, and — when the
// file parses — the document information dictionary and the catalog's
// metadata stream (compressed or in object streams).
func pdfLabels(content []byte) []string {
	var labels []string
	if l := pdfConfidentialityLabel(content); l != "" {
		labels = append(labels, l)
	}
	for _, m := range pdfRawLabel.FindAllSubmatch(content, 64) {
		labels = append(labels, unescapePDFLiteral(m[1]))
	}
	for _, m := range xmpLabel.FindAllSubmatch(content, 64) {
		labels = append(labels, string(m[1]))
	}
	ctx, err := api.ReadContext(context.Background(), bytes.NewReader(content), pdfConfig())
	if err != nil {
		return labels
	}
	if d, err := infoDict(ctx); err == nil {
		for k, v := range d {
			if msipName.MatchString(k) {
				labels = append(labels, pdfText(ctx, v))
			}
		}
	}
	if root, err := ctx.Catalog(); err == nil {
		if ref, ok := root["Metadata"].(types.IndirectRef); ok {
			if sd, _, err := ctx.DereferenceStreamDict(ref); err == nil && sd != nil {
				for _, m := range xmpLabel.FindAllSubmatch(boundedStream(sd), 64) {
					labels = append(labels, string(m[1]))
				}
			}
		}
	}
	return labels
}

// boundedStream returns the content of an unfiltered or Flate-encoded stream,
// decoding at most maxLabelPartBytes (a compressed metadata bomb is cut short).
func boundedStream(sd *types.StreamDict) []byte {
	var r io.Reader
	switch f := sd.Dict["Filter"].(type) {
	case nil:
		r = bytes.NewReader(sd.Raw)
	case types.Name:
		if f != "FlateDecode" {
			return nil
		}
		zr, err := zlib.NewReader(bytes.NewReader(sd.Raw))
		if err != nil {
			return nil
		}
		defer zr.Close()
		r = zr
	default:
		return nil
	}
	data, _ := io.ReadAll(io.LimitReader(r, maxLabelPartBytes))
	return data
}

// unescapePDFLiteral decodes the escapes of a PDF literal string (enough for label names).
func unescapePDFLiteral(b []byte) string {
	s, err := types.StringLiteralToString(types.StringLiteral(b))
	if err != nil {
		return string(b)
	}
	return s
}

// pdfConfidentialityLabel scans the first 15 lines (universal newlines, like
// Python text mode) for "/MSIP_Label_*_Name(C…".
func pdfConfidentialityLabel(content []byte) string {
	r := bufio.NewReader(bytes.NewReader(content))
	for i := 0; i < 15; i++ {
		line, err := readUniversalLine(r)
		if line == "" && err != nil {
			break
		}
		if pdfLabel.MatchString(line) {
			parts := strings.Split(strings.ReplaceAll(line, "Name(C", "#C"), "#")
			if len(parts) > 1 {
				label, _, _ := strings.Cut(parts[1], ")")
				return label
			}
		}
		if err != nil {
			break
		}
	}
	return ""
}

// readUniversalLine reads up to \n, \r or \r\n (Python universal newlines).
func readUniversalLine(r *bufio.Reader) (string, error) {
	var b strings.Builder
	for b.Len() < 1<<20 {
		c, err := r.ReadByte()
		if err != nil {
			return b.String(), err
		}
		switch c {
		case '\n':
			return b.String(), nil
		case '\r':
			if next, err := r.Peek(1); err == nil && next[0] == '\n' {
				_, _ = r.ReadByte()
			}
			return b.String(), nil
		}
		b.WriteByte(c)
	}
	return b.String(), nil
}

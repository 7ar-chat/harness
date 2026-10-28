package classification

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

const (
	contentTypesPart     = "[Content_Types].xml"
	packageRelationships = "_rels/.rels"
	customPropertiesPart = "docProps/custom.xml"
	customContentType    = "application/vnd.openxmlformats-officedocument.custom-properties+xml"
	customRelationship   = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/custom-properties"
	customPropertyFMTID  = "{D5CDD505-2E9C-101B-9397-08002B2CF9AE}"
	maxZipEntries        = 5000
	maxUncompressedBytes = 100 << 20
	// A package expanding to more than maxExpansionRatio times its size (once
	// past expansionFloor) is a decompression bomb, refused before any entry is
	// read (C-08); real Office files stay far below (text XML ≈ 5–30×).
	maxExpansionRatio     = 200
	expansionFloor        = 8 << 20
	maxContentTypesBytes  = 1 << 20
	nsContentTypes        = "http://schemas.openxmlformats.org/package/2006/content-types"
	nsRelationships       = "http://schemas.openxmlformats.org/package/2006/relationships"
	nsCustomProperties    = "http://schemas.openxmlformats.org/officeDocument/2006/custom-properties"
	nsDocPropsVTypes      = "http://schemas.openxmlformats.org/officeDocument/2006/docPropsVTypes"
	customPartNameInTypes = "/docProps/custom.xml"
)

var errZipFormat = errors.New("zip entry unreadable")

// checkExpansion refuses a package whose declared expanded size (enforced by
// the zip reader) exceeds the expansion ratio.
func checkExpansion(total uint64, size int) error {
	if total > expansionFloor && total > uint64(size)*maxExpansionRatio {
		return errf(ErrInvalidDocument, "OOXML package expands too much (decompression bomb)")
	}
	return nil
}

func isZipFormatError(err error) bool { return errors.Is(err, errZipFormat) }

// readEntryBounded reads a ZIP entry without allocating more than maximum bytes.
func readEntryBounded(f *zip.File, maximum int64) ([]byte, error) {
	if f.UncompressedSize64 > uint64(maximum) {
		return nil, errf(ErrInvalidDocument, "OOXML entry is too large")
	}
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errZipFormat, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, maximum+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errZipFormat, err)
	}
	if int64(len(data)) > maximum {
		return nil, errf(ErrInvalidDocument, "OOXML entry is too large")
	}
	return data, nil
}

type pkgEntry struct {
	name  string
	dir   bool
	data  []byte
	mtime zip.FileHeader
}

type ooxmlPackage struct {
	entries []*pkgEntry
	index   map[string]*pkgEntry
}

func (p *ooxmlPackage) get(name string) ([]byte, bool) {
	e, ok := p.index[name]
	if !ok {
		return nil, false
	}
	return e.data, true
}

func (p *ooxmlPackage) set(name string, data []byte) {
	if e, ok := p.index[name]; ok {
		e.data = data
		return
	}
	e := &pkgEntry{name: name, data: data}
	p.entries = append(p.entries, e)
	p.index[name] = e
}

// canonicalName mirrors _canonical_name: rejects traversal and non-canonical names.
func canonicalName(f *zip.File) (string, error) {
	name := f.Name
	if strings.Contains(name, "\\") || strings.HasPrefix(name, "/") || hasWindowsDrive(name) {
		return "", errf(ErrInvalidDocument, "OOXML package contains traversal entry")
	}
	var parts []string
	for _, p := range strings.Split(name, "/") {
		if p == ".." {
			return "", errf(ErrInvalidDocument, "OOXML package contains traversal entry")
		}
		if p == "" || p == "." {
			continue
		}
		parts = append(parts, p)
	}
	if len(parts) == 0 {
		return "", errf(ErrInvalidDocument, "OOXML package contains invalid entry name")
	}
	canonical := strings.Join(parts, "/")
	if f.FileInfo().IsDir() {
		canonical += "/"
	}
	return canonical, nil
}

var windowsDrive = regexp.MustCompile(`^[A-Za-z]:`)

func hasWindowsDrive(name string) bool { return windowsDrive.MatchString(name) }

// readPackage mirrors _read_package: a bounded, validated view of every entry.
func readPackage(content []byte) (*ooxmlPackage, error) {
	zr, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
	if err != nil {
		return nil, errf(ErrInvalidDocument, "invalid OOXML ZIP package")
	}
	if len(zr.File) > maxZipEntries {
		return nil, errf(ErrInvalidDocument, "OOXML package has too many entries")
	}
	pkg := &ooxmlPackage{index: map[string]*pkgEntry{}}
	collisions := map[string]bool{}
	var total uint64
	for _, f := range zr.File {
		canonical, err := canonicalName(f)
		if err != nil {
			return nil, err
		}
		key := strings.TrimSuffix(canonical, "/")
		if collisions[key] {
			return nil, errf(ErrInvalidDocument, "OOXML package contains duplicate entry")
		}
		collisions[key] = true
		if f.Name != canonical {
			return nil, errf(ErrInvalidDocument, "OOXML package contains invalid entry name")
		}
		if f.Flags&0x1 != 0 {
			return nil, errf(ErrInvalidDocument, "encrypted OOXML package")
		}
		if strings.HasPrefix(strings.ToLower(canonical), "_xmlsignatures/") {
			return nil, errf(ErrSignedDocument, "signed OOXML package")
		}
		total += f.UncompressedSize64
		if total > maxUncompressedBytes {
			return nil, errf(ErrInvalidDocument, "OOXML package is too large")
		}
	}
	if err := checkExpansion(total, len(content)); err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		canonical, _ := canonicalName(f)
		data, err := readEntryBounded(f, maxUncompressedBytes)
		if err != nil {
			if isZipFormatError(err) {
				return nil, errf(ErrInvalidDocument, "invalid OOXML ZIP package")
			}
			return nil, err
		}
		dir := f.FileInfo().IsDir()
		if dir && len(data) > 0 {
			return nil, errf(ErrInvalidDocument, "OOXML package contains non-empty directory")
		}
		e := &pkgEntry{name: canonical, dir: dir, data: data, mtime: f.FileHeader}
		pkg.entries = append(pkg.entries, e)
		pkg.index[canonical] = e
	}
	ct, ok := pkg.get(contentTypesPart)
	if !ok {
		return nil, errf(ErrInvalidDocument, "OOXML package is missing [Content_Types].xml")
	}
	rels, ok := pkg.get(packageRelationships)
	if !ok {
		return nil, errf(ErrInvalidDocument, "OOXML package is missing package relationships")
	}
	if !wellFormed(ct) || !wellFormed(rels) {
		return nil, errf(ErrInvalidDocument, "invalid OOXML package metadata")
	}
	return pkg, nil
}

func wellFormed(data []byte) bool {
	dec := xml.NewDecoder(bytes.NewReader(data))
	sawRoot := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return sawRoot
		}
		if err != nil {
			return false
		}
		if _, ok := tok.(xml.StartElement); ok {
			sawRoot = true
		}
	}
}

// orderedProps keeps insertion order like a Python dict.
type orderedProps struct {
	keys   []string
	values map[string]string
}

func newOrderedProps() *orderedProps { return &orderedProps{values: map[string]string{}} }

func (o *orderedProps) has(k string) bool { _, ok := o.values[k]; return ok }

func (o *orderedProps) put(k, v string) {
	if !o.has(k) {
		o.keys = append(o.keys, k)
	}
	o.values[k] = v
}

// xmlNode is a minimal generic element tree (namespace-resolved names).
type xmlNode struct {
	Name     xml.Name
	Attr     []xml.Attr
	Children []*xmlNode
	Text     *string
}

func parseTree(data []byte) (*xmlNode, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	var stack []*xmlNode
	var root *xmlNode
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			n := &xmlNode{Name: t.Name, Attr: t.Attr}
			if len(stack) > 0 {
				parent := stack[len(stack)-1]
				parent.Children = append(parent.Children, n)
			} else if root == nil {
				root = n
			}
			stack = append(stack, n)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 {
				cur := stack[len(stack)-1]
				// ElementTree .text is the text before the first child only.
				if len(cur.Children) == 0 {
					s := string(t)
					if cur.Text != nil {
						s = *cur.Text + s
					}
					cur.Text = &s
				}
			}
		}
	}
	if root == nil {
		return nil, errors.New("no root element")
	}
	return root, nil
}

func attr(n *xmlNode, local string) (string, bool) {
	for _, a := range n.Attr {
		if a.Name.Space == "" && a.Name.Local == local {
			return a.Value, true
		}
	}
	return "", false
}

// customProperties mirrors _custom_properties: every property as text.
func customProperties(pkg *ooxmlPackage) (*orderedProps, error) {
	out := newOrderedProps()
	raw, ok := pkg.get(customPropertiesPart)
	if !ok {
		return out, nil
	}
	root, err := parseTree(raw)
	if err != nil {
		return nil, errf(ErrInvalidDocument, "invalid OOXML custom properties")
	}
	if root.Name.Space != nsCustomProperties || root.Name.Local != "Properties" {
		return nil, errf(ErrInvalidDocument, "invalid OOXML custom properties root")
	}
	for _, p := range root.Children {
		if p.Name.Space != nsCustomProperties || p.Name.Local != "property" {
			continue
		}
		name, _ := attr(p, "name")
		if name == "" || out.has(name) {
			return nil, errf(ErrInvalidDocument, "duplicate or missing OOXML custom property")
		}
		if len(p.Children) != 1 || p.Children[0].Text == nil {
			return nil, errf(ErrInvalidDocument, "invalid OOXML custom property value")
		}
		out.put(name, *p.Children[0].Text)
	}
	return out, nil
}

// ReadCustomProperties returns the OOXML custom properties (docProps/custom.xml) as text.
func ReadCustomProperties(content []byte) (map[string]string, error) {
	pkg, err := readPackage(content)
	if err != nil {
		return nil, err
	}
	props, err := customProperties(pkg)
	if err != nil {
		return nil, err
	}
	return props.values, nil
}

func escapeXML(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func writeCustomXML(props *orderedProps) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n")
	b.WriteString(`<Properties xmlns="` + nsCustomProperties + `" xmlns:vt="` + nsDocPropsVTypes + `">`)
	for i, k := range props.keys {
		fmt.Fprintf(&b, `<property fmtid="%s" pid="%d" name="%s"><vt:lpwstr>%s</vt:lpwstr></property>`,
			customPropertyFMTID, i+2, escapeXML(k), escapeXML(props.values[k]))
	}
	b.WriteString(`</Properties>`)
	return []byte(b.String())
}

// rawElement locates a top-level child element of the root in the raw bytes.
type rawElement struct {
	start, end int64 // byte offsets [start, end)
	prefix     string
	local      string
	attrs      []xml.Attr // raw (prefixed) attributes
}

// scanChildren returns the root's raw prefix, the offset of its closing tag
// and its direct children (with raw offsets), so edits keep every other byte.
func scanChildren(data []byte) (rootPrefix string, rootLocal string, closeAt int64, children []rawElement, err error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	depth := 0
	var cur *rawElement
	for {
		before := dec.InputOffset()
		tok, terr := dec.RawToken()
		if terr == io.EOF {
			break
		}
		if terr != nil {
			return "", "", 0, nil, terr
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth == 1 {
				rootPrefix, rootLocal = t.Name.Space, t.Name.Local
			}
			if depth == 2 {
				cur = &rawElement{start: before, prefix: t.Name.Space, local: t.Name.Local, attrs: t.Attr}
			}
		case xml.EndElement:
			if depth == 2 && cur != nil {
				cur.end = dec.InputOffset()
				children = append(children, *cur)
				cur = nil
			}
			if depth == 1 {
				closeAt = before
				if dec.InputOffset() == before { // self-closing root: no closing tag
					closeAt = -1
				}
			}
			depth--
		}
	}
	return rootPrefix, rootLocal, closeAt, children, nil
}

func rawAttr(attrs []xml.Attr, local string) (string, bool) {
	for _, a := range attrs {
		if a.Name.Space == "" && a.Name.Local == local {
			return a.Value, true
		}
	}
	return "", false
}

func renderElement(prefix, local string, attrs []xml.Attr) string {
	name := local
	if prefix != "" {
		name = prefix + ":" + local
	}
	var b strings.Builder
	b.WriteString("<" + name)
	for _, a := range attrs {
		n := a.Name.Local
		if a.Name.Space != "" {
			n = a.Name.Space + ":" + n
		}
		var v bytes.Buffer
		_ = xml.EscapeText(&v, []byte(a.Value))
		b.WriteString(" " + n + `="` + strings.ReplaceAll(v.String(), `"`, "&quot;") + `"`)
	}
	b.WriteString("/>")
	return b.String()
}

func setAttr(attrs []xml.Attr, local, value string) []xml.Attr {
	out := append([]xml.Attr(nil), attrs...)
	for i, a := range out {
		if a.Name.Space == "" && a.Name.Local == local {
			out[i].Value = value
			return out
		}
	}
	return append(out, xml.Attr{Name: xml.Name{Local: local}, Value: value})
}

// upsertChild replaces the first child matching match (keeping its attributes,
// setting key=value) or inserts a new element before the root's closing tag.
func upsertChild(data []byte, wantNS, wantRoot, childLocal string, match func(rawElement) bool,
	replace func(rawElement) []xml.Attr, insert func([]rawElement) []xml.Attr, what string) ([]byte, error) {
	root, err := parseTree(data)
	if err != nil {
		return nil, errf(ErrInvalidDocument, "invalid OOXML %s", what)
	}
	if root.Name.Space != wantNS || root.Name.Local != wantRoot {
		return nil, errf(ErrInvalidDocument, "invalid OOXML %s root", what)
	}
	prefix, rootLocal, closeAt, children, err := scanChildren(data)
	if err != nil {
		return nil, errf(ErrInvalidDocument, "invalid OOXML %s", what)
	}
	for _, c := range children {
		if c.local == childLocal && c.prefix == prefix && match(c) {
			el := renderElement(c.prefix, c.local, replace(c))
			out := append([]byte(nil), data[:c.start]...)
			out = append(out, el...)
			return append(out, data[c.end:]...), nil
		}
	}
	el := renderElement(prefix, childLocal, insert(children))
	if closeAt < 0 {
		// <Types .../> → <Types ...>el</Types>
		trimmed := bytes.TrimRight(data, " \t\r\n")
		idx := bytes.LastIndex(trimmed, []byte("/>"))
		name := rootLocal
		if prefix != "" {
			name = prefix + ":" + rootLocal
		}
		out := append([]byte(nil), trimmed[:idx]...)
		out = append(out, '>')
		out = append(out, el...)
		out = append(out, []byte("</"+name+">")...)
		return append(out, data[len(trimmed):]...), nil
	}
	out := append([]byte(nil), data[:closeAt]...)
	out = append(out, el...)
	return append(out, data[closeAt:]...), nil
}

func updateContentTypes(raw []byte) ([]byte, error) {
	return upsertChild(raw, nsContentTypes, "Types", "Override",
		func(c rawElement) bool { v, _ := rawAttr(c.attrs, "PartName"); return v == customPartNameInTypes },
		func(c rawElement) []xml.Attr { return setAttr(c.attrs, "ContentType", customContentType) },
		func([]rawElement) []xml.Attr {
			return []xml.Attr{{Name: xml.Name{Local: "PartName"}, Value: customPartNameInTypes}, {Name: xml.Name{Local: "ContentType"}, Value: customContentType}}
		}, "content types")
}

func updateRelationships(raw []byte) ([]byte, error) {
	return upsertChild(raw, nsRelationships, "Relationships", "Relationship",
		func(c rawElement) bool { v, _ := rawAttr(c.attrs, "Type"); return v == customRelationship },
		func(c rawElement) []xml.Attr { return setAttr(c.attrs, "Target", "docProps/custom.xml") },
		func(children []rawElement) []xml.Attr {
			ids := map[string]bool{}
			for _, c := range children {
				if c.local == "Relationship" {
					id, _ := rawAttr(c.attrs, "Id")
					ids[id] = true
				}
			}
			n := 1
			for ids[fmt.Sprintf("rId%d", n)] {
				n++
			}
			return []xml.Attr{
				{Name: xml.Name{Local: "Id"}, Value: fmt.Sprintf("rId%d", n)},
				{Name: xml.Name{Local: "Type"}, Value: customRelationship},
				{Name: xml.Name{Local: "Target"}, Value: "docProps/custom.xml"},
			}
		}, "package relationships")
}

// writeCustomProperties mirrors write_custom_properties: merges the new
// properties into docProps/custom.xml, registers the part in the content types
// and package relationships, keeps every other entry. Existing properties the
// providers own (drop: any MSIP label, GVData) are replaced, so a document
// that already carries a label — Studio's own included — ends up with exactly
// the new one (C-04, C-07). pkg is the package already read by inspect.
func writeCustomProperties(pkg *ooxmlPackage, keys []string, props map[string]string, drop func(string) bool) ([]byte, error) {
	existing, err := customProperties(pkg)
	if err != nil {
		return nil, err
	}
	merged := newOrderedProps()
	for _, k := range existing.keys {
		if drop == nil || !drop(k) {
			merged.put(k, existing.values[k])
		}
	}
	for _, k := range keys {
		if strings.TrimSpace(k) == "" {
			return nil, errf(ErrInvalidDocument, "custom properties must be non-empty string pairs")
		}
		merged.put(k, props[k])
	}
	ct, _ := pkg.get(contentTypesPart)
	newCT, err := updateContentTypes(ct)
	if err != nil {
		return nil, err
	}
	rels, _ := pkg.get(packageRelationships)
	newRels, err := updateRelationships(rels)
	if err != nil {
		return nil, err
	}
	pkg.set(customPropertiesPart, writeCustomXML(merged))
	pkg.set(contentTypesPart, newCT)
	pkg.set(packageRelationships, newRels)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range pkg.entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		h.Modified = e.mtime.Modified
		if e.dir {
			h.Method = zip.Store
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			return nil, errf(ErrInvalidDocument, "writing OOXML package failed")
		}
		if _, err := w.Write(e.data); err != nil {
			return nil, errf(ErrInvalidDocument, "writing OOXML package failed")
		}
	}
	if err := zw.Close(); err != nil {
		return nil, errf(ErrInvalidDocument, "writing OOXML package failed")
	}
	return buf.Bytes(), nil // verify re-reads the output and checks every property
}

// partDigests fingerprints every entry except the ones classification rewrites.
func partDigests(pkg *ooxmlPackage) map[string][32]byte {
	out := map[string][32]byte{}
	for _, e := range pkg.entries {
		switch e.name {
		case contentTypesPart, packageRelationships, customPropertiesPart:
			continue
		}
		out[e.name] = sha256.Sum256(e.data)
	}
	return out
}

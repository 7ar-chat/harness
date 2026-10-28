package classification

import (
	"bytes"
	"encoding/xml"
	"io"
	"strings"
)

// adapter writes and verifies metadata for one format (adapters/*.py).
// inspect returns the source structure, which writeMetadata reuses (the input
// is parsed once); drop selects existing properties to remove (provider-owned).
type adapter interface {
	inspect(content []byte) (any, error)
	writeMetadata(source any, keys []string, props map[string]string, drop func(string) bool) ([]byte, error)
	verify(content []byte, keys []string, props map[string]string, source any) error
}

// ooxmlAdapter covers DOCX, XLSX and PPTX. Instead of re-opening the file with
// python-docx / openpyxl / python-pptx, inspect checks the main part exists and
// has at least one section / worksheet / slide (like the Python adapters), and
// verify proves the visible structure is unchanged by requiring every package
// part other than the three rewritten metadata parts to be byte-identical.
type ooxmlAdapter struct{ format Format }

type ooxmlStructure struct {
	parts map[string][32]byte
	pkg   *ooxmlPackage
}

// mainPart returns the PartName of the main document override.
func mainPart(pkg *ooxmlPackage, f Format) string {
	ct, _ := pkg.get(contentTypesPart)
	root, err := parseTree(ct)
	if err != nil {
		return ""
	}
	for _, c := range root.Children {
		if c.Name.Local != "Override" {
			continue
		}
		t, _ := attr(c, "ContentType")
		if ooxmlMainContentTypes[t] == f {
			p, _ := attr(c, "PartName")
			return strings.TrimPrefix(p, "/")
		}
	}
	return ""
}

// countElements counts elements with the given local name.
func countElements(data []byte, local string) int {
	dec := xml.NewDecoder(bytes.NewReader(data))
	n := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return n
		}
		if err != nil {
			return -1
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == local {
			n++
		}
	}
}

func (a ooxmlAdapter) check(content []byte) (*ooxmlPackage, error) {
	pkg, err := readPackage(content)
	if err != nil {
		return nil, err
	}
	name := mainPart(pkg, a.format)
	data, ok := pkg.get(name)
	if name == "" || !ok {
		return nil, errf(ErrInvalidDocument, "invalid %s", strings.ToUpper(string(a.format)))
	}
	switch a.format {
	case DOCX:
		if countElements(data, "sectPr") < 1 {
			return nil, errf(ErrInvalidDocument, "DOCX has no sections")
		}
	case XLSX:
		if countElements(data, "sheet") < 1 {
			return nil, errf(ErrInvalidDocument, "XLSX has no worksheets")
		}
	case PPTX:
		if countElements(data, "sldId") < 1 {
			return nil, errf(ErrInvalidDocument, "PPTX has no slides")
		}
	}
	return pkg, nil
}

func (a ooxmlAdapter) inspect(content []byte) (any, error) {
	pkg, err := a.check(content)
	if err != nil {
		return nil, err
	}
	return ooxmlStructure{parts: partDigests(pkg), pkg: pkg}, nil
}

func (a ooxmlAdapter) writeMetadata(source any, keys []string, props map[string]string, drop func(string) bool) ([]byte, error) {
	return writeCustomProperties(source.(ooxmlStructure).pkg, keys, props, drop)
}

func (a ooxmlAdapter) verify(content []byte, keys []string, props map[string]string, source any) error {
	pkg, err := a.check(content)
	if err != nil {
		return err
	}
	src := source.(ooxmlStructure)
	got := partDigests(pkg)
	if len(got) != len(src.parts) {
		return errf(ErrVerificationFailed, "%s structure changed", strings.ToUpper(string(a.format)))
	}
	for name, digest := range src.parts {
		if got[name] != digest {
			return errf(ErrVerificationFailed, "%s visible content changed", strings.ToUpper(string(a.format)))
		}
	}
	written, err := customProperties(pkg)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if v, ok := written.values[k]; !ok || v != props[k] {
			return errf(ErrVerificationFailed, "%s metadata changed: %s", strings.ToUpper(string(a.format)), k)
		}
	}
	return nil
}

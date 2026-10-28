package classification

import (
	"bytes"
	"context"
	"crypto/sha256"
	"math"
	"sort"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

// pdfGeometryTolerance mirrors PDF_GEOMETRY_TOLERANCE (six-decimal serialization).
const pdfGeometryTolerance = 0.5e-6

type pdfStructure struct {
	ctx       *model.Context // the parsed input, reused by writeMetadata
	pages     int
	media     [][4]float64
	crop      [][4]float64
	rotations []int
	digests   [][32]byte
}

func pdfConfig() *model.Configuration {
	conf := model.NewDefaultConfiguration()
	conf.ValidationMode = model.ValidationRelaxed
	return conf
}

func readPDF(content []byte) (*model.Context, error) {
	ctx, err := api.ReadContext(context.Background(), bytes.NewReader(content), pdfConfig())
	if err != nil {
		return nil, errf(ErrInvalidDocument, "invalid PDF")
	}
	if ctx.Encrypt != nil || ctx.E != nil {
		return nil, errf(ErrInvalidDocument, "encrypted PDF is not supported")
	}
	// Like pypdf, only a parseable structure is required (no strict validation).
	if err := ctx.EnsurePageCount(); err != nil {
		return nil, errf(ErrInvalidDocument, "invalid PDF")
	}
	if containsSignature(ctx) {
		return nil, errf(ErrInvalidDocument, "signed PDF is not supported")
	}
	if ctx.PageCount < 1 {
		return nil, errf(ErrInvalidDocument, "PDF has no pages")
	}
	return ctx, nil
}

// containsSignature walks the object graph from the catalog looking for
// AcroForm signature markers (/FT /Sig or a /ByteRange entry), like
// PdfAdapter._contains_signature.
func containsSignature(ctx *model.Context) bool {
	root, err := ctx.Catalog()
	if err != nil {
		return false
	}
	seen := map[int]bool{}
	var walk func(o types.Object, depth int) bool
	walk = func(o types.Object, depth int) bool {
		if depth > 64 {
			return false
		}
		if ref, ok := o.(types.IndirectRef); ok {
			n := ref.ObjectNumber.Value()
			if seen[n] {
				return false
			}
			seen[n] = true
			d, err := ctx.Dereference(ref)
			if err != nil {
				return false
			}
			o = d
		}
		switch v := o.(type) {
		case types.Dict:
			if ft, ok := v["FT"].(types.Name); ok && ft == "Sig" {
				return true
			}
			if _, ok := v["ByteRange"]; ok {
				return true
			}
			for _, child := range v {
				if walk(child, depth+1) {
					return true
				}
			}
		case types.StreamDict:
			return walk(v.Dict, depth+1)
		case types.Array:
			for _, child := range v {
				if walk(child, depth+1) {
					return true
				}
			}
		}
		return false
	}
	return walk(root, 0)
}

func box(r *types.Rectangle) [4]float64 {
	if r == nil {
		return [4]float64{}
	}
	return [4]float64{r.LL.X, r.LL.Y, r.UR.X, r.UR.Y}
}

func inspectPDF(ctx *model.Context) (pdfStructure, error) {
	s := pdfStructure{pages: ctx.PageCount}
	for i := 1; i <= ctx.PageCount; i++ {
		d, _, inh, err := ctx.PageDict(context.Background(), i, false)
		if err != nil || d == nil || inh == nil {
			return s, errf(ErrInvalidDocument, "invalid PDF")
		}
		media := inh.MediaBox
		crop := inh.CropBox
		if crop == nil {
			crop = media
		}
		s.media = append(s.media, box(media))
		s.crop = append(s.crop, box(crop))
		s.rotations = append(s.rotations, ((inh.Rotate%360)+360)%360)
		data, err := ctx.PageContent(d, i)
		if err != nil && err != model.ErrNoContent {
			return s, errf(ErrInvalidDocument, "invalid PDF")
		}
		s.digests = append(s.digests, sha256.Sum256(data))
	}
	return s, nil
}

func boxesEqual(a, b [][4]float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		for j := 0; j < 4; j++ {
			if math.IsInf(a[i][j], 0) || math.IsNaN(a[i][j]) || math.Abs(a[i][j]-b[i][j]) > pdfGeometryTolerance {
				return false
			}
		}
	}
	return true
}

// pdfText renders an Info dict value like pypdf's str(): text for strings, "/Name" for names.
func pdfText(ctx *model.Context, o types.Object) string {
	if ref, ok := o.(types.IndirectRef); ok {
		d, err := ctx.Dereference(ref)
		if err != nil {
			return ""
		}
		o = d
	}
	switch v := o.(type) {
	case types.StringLiteral:
		s, err := types.StringLiteralToString(v)
		if err != nil {
			return v.Value()
		}
		return s
	case types.HexLiteral:
		s, err := types.HexLiteralToString(v)
		if err != nil {
			return v.Value()
		}
		return s
	case types.Name:
		return "/" + string(v)
	case nil:
		return ""
	}
	return o.String()
}

func infoDict(ctx *model.Context) (types.Dict, error) {
	if ctx.Info == nil {
		return nil, nil
	}
	return ctx.DereferenceDict(*ctx.Info)
}

// PDFInfo returns the document information dictionary (keys with their "/" prefix).
func PDFInfo(content []byte) (map[string]string, error) {
	ctx, err := readPDF(content)
	if err != nil {
		return nil, err
	}
	d, err := infoDict(ctx)
	if err != nil {
		return nil, errf(ErrInvalidDocument, "invalid PDF")
	}
	out := map[string]string{}
	for k, v := range d {
		out["/"+k] = pdfText(ctx, v)
	}
	return out, nil
}

type pdfAdapter struct{}

func (pdfAdapter) inspect(content []byte) (any, error) {
	ctx, err := readPDF(content)
	if err != nil {
		return nil, err
	}
	s, err := inspectPDF(ctx)
	if err != nil {
		return nil, err
	}
	s.ctx = ctx
	return s, nil
}

// writeMetadata merges the properties into the existing document information
// (keeping every other existing entry, like pypdf add_metadata; provider-owned
// entries — earlier labels — are replaced, never doubled: C-04) and rewrites the file.
func (pdfAdapter) writeMetadata(source any, keys []string, props map[string]string, drop func(string) bool) ([]byte, error) {
	ctx := source.(pdfStructure).ctx
	var err error
	ctx.PreserveInfoDict = true
	var d types.Dict
	if ctx.Info != nil {
		if d, err = ctx.DereferenceDict(*ctx.Info); err != nil {
			return nil, errf(ErrInvalidDocument, "invalid PDF")
		}
	}
	for k := range d {
		if drop != nil && drop(k) {
			delete(d, k)
		}
	}
	if d == nil {
		d = types.NewDict()
		ir, err := ctx.IndRefForNewObject(d)
		if err != nil {
			return nil, errf(ErrInvalidDocument, "invalid PDF")
		}
		ctx.Info = ir
	}
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)
	for _, k := range sorted {
		s, err := types.EscapedUTF16String(props[k])
		if err != nil {
			return nil, errf(ErrInvalidDocument, "PDF metadata encoding failed for %s", k)
		}
		d[k] = types.StringLiteral(*s)
	}
	var buf bytes.Buffer
	if err := api.WriteContext(context.Background(), ctx, &buf); err != nil {
		return nil, errf(ErrInvalidDocument, "writing PDF failed")
	}
	return buf.Bytes(), nil // verify re-reads the output and checks every property
}

func (pdfAdapter) verify(content []byte, keys []string, props map[string]string, source any) error {
	ctx, err := readPDF(content)
	if err != nil {
		return err
	}
	got, err := inspectPDF(ctx)
	if err != nil {
		return err
	}
	src := source.(pdfStructure)
	switch {
	case got.pages != src.pages:
		return errf(ErrVerificationFailed, "PDF page count changed")
	case !boxesEqual(got.media, src.media):
		return errf(ErrVerificationFailed, "PDF media geometry changed")
	case !boxesEqual(got.crop, src.crop):
		return errf(ErrVerificationFailed, "PDF crop geometry changed")
	}
	for i := range got.rotations {
		if got.rotations[i] != src.rotations[i] {
			return errf(ErrVerificationFailed, "PDF page rotation changed")
		}
		if got.digests[i] != src.digests[i] {
			return errf(ErrVerificationFailed, "PDF visible content changed")
		}
	}
	d, err := infoDict(ctx)
	if err != nil {
		return errf(ErrVerificationFailed, "PDF metadata changed")
	}
	for _, k := range keys {
		if pdfText(ctx, d[k]) != props[k] {
			return errf(ErrVerificationFailed, "PDF metadata changed: %s", k)
		}
	}
	return nil
}

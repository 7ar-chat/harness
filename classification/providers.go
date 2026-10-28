package classification

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Provider produces and verifies one family of label properties.
type Provider interface {
	Name() string
	// Properties returns the provider's properties in insertion order.
	Properties(ctx Context, settings map[string]string) ([]string, map[string]string, error)
	Verify(props map[string]string, ctx Context, settings map[string]string) error
}

func setting(settings map[string]string, provider, key string, aliases ...string) (string, error) {
	for _, k := range append([]string{key}, aliases...) {
		if v := strings.TrimSpace(settings[k]); v != "" {
			return v, nil
		}
	}
	return "", errf(ErrProvider, "missing %s setting: %s", provider, key)
}

func settingDefault(settings map[string]string, key, def string) string {
	if v := strings.TrimSpace(settings[key]); v != "" {
		return v
	}
	return def
}

func utcStamp(ctx Context) string { return ctx.Timestamp.UTC().Format("2006-01-02T15:04:05Z") }

// MIP is the Microsoft Information Protection label provider (providers/mip.py).
type MIP struct{}

func (MIP) Name() string { return "mip" }

func (MIP) expected(ctx Context, s map[string]string) ([]string, map[string]string, error) {
	labelID, err := setting(s, "MIP", "label_id")
	if err != nil {
		return nil, nil, err
	}
	labelName, err := setting(s, "MIP", "label_name", "name")
	if err != nil {
		return nil, nil, err
	}
	siteID, err := setting(s, "MIP", "site_id")
	if err != nil {
		return nil, nil, err
	}
	prefix := "MSIP_Label_" + labelID + "_"
	keys := []string{"Enabled", "SetDate", "Method", "Name", "SiteId", "ActionId", "ContentBits", "Tag"}
	vals := map[string]string{
		"Enabled":     strings.ToLower(settingDefault(s, "enabled", "true")),
		"SetDate":     utcStamp(ctx),
		"Method":      settingDefault(s, "method", "Privileged"),
		"Name":        labelName,
		"SiteId":      siteID,
		"ActionId":    ctx.ActionID.String(),
		"ContentBits": "0", // no visual header, footer or watermark is written
		"Tag":         settingDefault(s, "tag", "10, 0, 1, 1"),
	}
	out := map[string]string{}
	ordered := make([]string, 0, len(keys))
	for _, k := range keys {
		ordered = append(ordered, prefix+k)
		out[prefix+k] = vals[k]
	}
	return ordered, out, nil
}

func (m MIP) Properties(ctx Context, s map[string]string) ([]string, map[string]string, error) {
	return m.expected(ctx, s)
}

func (m MIP) Verify(props map[string]string, ctx Context, s map[string]string) error {
	_, expected, err := m.expected(ctx, s)
	if err != nil {
		return err
	}
	for k := range props {
		if strings.HasPrefix(k, "MSIP_Label_") {
			if _, ok := expected[k]; !ok {
				return errf(ErrProvider, "unexpected MIP property: %s", k)
			}
		}
	}
	for k, v := range expected {
		if got, ok := props[k]; !ok || got != v {
			return errf(ErrProvider, "missing or invalid MIP property: %s", k)
		}
	}
	return nil
}

// GVData is the GVData semantic metadata provider (providers/gvdata.py).
type GVData struct{}

const (
	gvChunkSize   = 200
	gvEndSentinel = "(end)"
)

var gvWrittenKeys = []string{"Classification", "ClassificationTagSetId", "ComplianceTagSetId", "Distribution",
	"DistributionTagSetId", "FileId", "gvdocid", "TagDateTime", "UserId"}

var gvDataKey = regexp.MustCompile(`^GVData(?:([0-9]+))?$`)

func (GVData) Name() string { return "gvdata" }

func (GVData) metadata(ctx Context, s map[string]string) ([]string, map[string]string, error) {
	get := func(key string, aliases ...string) (string, error) { return setting(s, "GVData", key, aliases...) }
	classification, err := get("classification")
	if err != nil {
		return nil, nil, err
	}
	classTag, err := get("classification_tag_set_id", "classification_tag_id")
	if err != nil {
		return nil, nil, err
	}
	compliance, err := get("compliance_tag_set_id")
	if err != nil {
		return nil, nil, err
	}
	distribution, err := get("distribution")
	if err != nil {
		return nil, nil, err
	}
	distTag, err := get("distribution_tag_set_id", "distribution_tag_id")
	if err != nil {
		return nil, nil, err
	}
	vals := map[string]string{
		"Classification":         classification,
		"ClassificationTagSetId": classTag,
		"ComplianceTagSetId":     compliance,
		"Distribution":           distribution,
		"DistributionTagSetId":   distTag,
		"FileId":                 ctx.DocumentID.String(),
		"gvdocid":                ctx.DocumentID.String(),
		"TagDateTime":            utcStamp(ctx),
		"UserId":                 ctx.Actor,
	}
	return append([]string(nil), gvWrittenKeys...), vals, nil
}

type kv struct{ k, v string }

func (GVData) payload(ctx Context, s map[string]string, count int) ([]kv, error) {
	get := func(key string, aliases ...string) (string, error) { return setting(s, "GVData", key, aliases...) }
	classification, err := get("classification")
	if err != nil {
		return nil, err
	}
	distribution, err := get("distribution")
	if err != nil {
		return nil, err
	}
	classID, err := get("classification_tag_set_id", "classification_tag_id")
	if err != nil {
		return nil, err
	}
	distID, err := get("distribution_tag_set_id", "distribution_tag_id")
	if err != nil {
		return nil, err
	}
	return []kv{
		{"tagset_" + strings.ReplaceAll(classID, "-", "_") + "_classification", classification},
		{"tagset_" + strings.ReplaceAll(distID, "-", "_") + "_distribution", distribution},
		{"OS", "Windows"},
		{"docID", ctx.DocumentID.String()},
		{"docState", "{}"},
		{"lineID", ctx.LineID.String()},
		{"parentLineIDs", "[]"},
		{"writtenKeys", pyJSONList(gvWrittenKeys)},
		{"noOfGvDataEntries", strconv.Itoa(count)},
	}, nil
}

func (g GVData) encodedPayload(ctx Context, s map[string]string) (string, int, error) {
	count := 1
	for i := 0; i < 100; i++ {
		p, err := g.payload(ctx, s, count)
		if err != nil {
			return "", 0, err
		}
		encoded := base64.StdEncoding.EncodeToString([]byte(pyJSONObject(p)))
		next := (len(encoded) + gvChunkSize - 1) / gvChunkSize
		if next == count {
			return encoded, count, nil
		}
		count = next
	}
	return "", 0, errf(ErrProvider, "GVData chunk count did not stabilize")
}

func (g GVData) Properties(ctx Context, s map[string]string) ([]string, map[string]string, error) {
	encoded, count, err := g.encodedPayload(ctx, s)
	if err != nil {
		return nil, nil, err
	}
	var chunks []string
	for i := 0; i < len(encoded); i += gvChunkSize {
		chunks = append(chunks, encoded[i:min(i+gvChunkSize, len(encoded))])
	}
	if len(chunks) != count {
		return nil, nil, errf(ErrProvider, "GVData chunk count changed while serializing")
	}
	keys, vals, err := g.metadata(ctx, s)
	if err != nil {
		return nil, nil, err
	}
	put := func(k, v string) {
		if _, ok := vals[k]; !ok {
			keys = append(keys, k)
		}
		vals[k] = v
	}
	// Same (odd) naming as gvdata.py: GVData, GVData0, GVData1, … then the sentinel.
	put("GVData", chunks[0])
	for i, c := range chunks[1:] {
		put(fmt.Sprintf("GVData%d", i), c)
	}
	put(fmt.Sprintf("GVData%d", len(chunks)-1), gvEndSentinel)
	return keys, vals, nil
}

func (g GVData) Verify(props map[string]string, ctx Context, s map[string]string) error {
	_, expected, err := g.metadata(ctx, s)
	if err != nil {
		return err
	}
	for k, v := range expected {
		if got, ok := props[k]; !ok || got != v {
			return errf(ErrProvider, "missing or invalid GVData property: %s", k)
		}
	}
	for k := range props {
		if strings.HasPrefix(k, "GVData") && !gvDataKey.MatchString(k) {
			return errf(ErrProvider, "unexpected GVData chunk: %s", k)
		}
	}
	var chunks []string
	sentinelKey := ""
	for index := 0; ; index++ {
		key := "GVData"
		if index > 0 {
			key = fmt.Sprintf("GVData%d", index-1)
		}
		v, ok := props[key]
		if !ok {
			return errf(ErrProvider, "missing GVData sentinel")
		}
		if v == gvEndSentinel {
			if len(chunks) == 0 {
				return errf(ErrProvider, "GVData sentinel must follow data chunks")
			}
			sentinelKey = key
			break
		}
		if len(v) > gvChunkSize {
			return errf(ErrProvider, "GVData chunk exceeds %d characters", gvChunkSize)
		}
		chunks = append(chunks, v)
	}
	for i, c := range chunks[:len(chunks)-1] {
		if len(c) != gvChunkSize {
			return errf(ErrProvider, "GVData chunk %d must be exactly %d characters", i, gvChunkSize)
		}
	}
	for k := range props {
		if !gvDataKey.MatchString(k) || k == sentinelKey {
			continue
		}
		suffix := strings.TrimPrefix(k, "GVData")
		idx := -1
		if suffix != "" {
			n, err := strconv.Atoi(suffix)
			if err != nil {
				return errf(ErrProvider, "unexpected GVData chunk: %s", k)
			}
			idx = n
		}
		if idx >= len(chunks) {
			return errf(ErrProvider, "unexpected GVData chunk after sentinel: %s", k)
		}
		want := "GVData"
		if idx >= 0 {
			want = fmt.Sprintf("GVData%d", idx)
		}
		if k != want {
			return errf(ErrProvider, "unexpected GVData chunk: %s", k)
		}
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(strings.Join(chunks, ""))
	if err != nil {
		return errf(ErrProvider, "invalid GVData Base64 or JSON payload")
	}
	want, err := g.payload(ctx, s, len(chunks))
	if err != nil {
		return err
	}
	if string(decoded) != pyJSONObject(want) {
		return errf(ErrProvider, "invalid GVData payload")
	}
	return nil
}

// pyJSONString encodes like Python json.dumps (ensure_ascii=True).
func pyJSONString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r < 0x20 || (r >= 0x7f && r <= 0xffff):
				if r < 0x7f {
					fmt.Fprintf(&b, `\u%04x`, r)
				} else if r == 0x7f {
					b.WriteRune(r)
				} else {
					fmt.Fprintf(&b, `\u%04x`, r)
				}
			case r > 0xffff:
				r1, r2 := utf16.EncodeRune(r)
				fmt.Fprintf(&b, `\u%04x\u%04x`, r1, r2)
			default:
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// pyJSONObject encodes an ordered string map like json.dumps(..., separators=(",", ":")).
func pyJSONObject(pairs []kv) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(pyJSONString(p.k) + ":" + pyJSONString(p.v))
	}
	b.WriteByte('}')
	return b.String()
}

func pyJSONList(items []string) string {
	parts := make([]string, len(items))
	for i, s := range items {
		parts[i] = pyJSONString(s)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

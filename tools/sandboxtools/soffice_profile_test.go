package sandboxtools_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GhaziBenDahmane/harness/tools"
)

// fakeSoffice puts a soffice on PATH that logs its arguments (one line per
// conversion) and writes a (non-empty, invalid) <outdir>/<name>.<format> like LibreOffice.
// The conversions run from python3 scripts: the test is skipped without it.
func fakeSoffice(t *testing.T) (env map[string]string, log string) {
	t.Helper()
	requirePython3(t)
	bin := t.TempDir()
	log = filepath.Join(bin, "soffice.log")
	script := `#!/usr/bin/env bash
echo "$*" >> "` + log + `"
fmt=pdf; out=.; src=""
while [ $# -gt 0 ]; do
  case "$1" in
    --convert-to) fmt="${2%%:*}"; shift 2 ;;
    --outdir) out="$2"; shift 2 ;;
    -*) shift ;;
    *) src="$1"; shift ;;
  esac
done
b=$(basename "$src"); echo "%PDF-fake" > "$out/${b%.*}.$fmt"
`
	if err := os.WriteFile(filepath.Join(bin, "soffice"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return map[string]string{"PATH": bin + ":" + os.Getenv("PATH")}, log
}

// profiles are the LibreOffice user profiles the logged conversions used.
func profiles(t *testing.T, log string) []string {
	t.Helper()
	data, _ := os.ReadFile(log)
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		profile := ""
		for _, f := range strings.Fields(line) {
			if v, ok := strings.CutPrefix(f, "-env:UserInstallation="); ok {
				profile = v
			}
		}
		if !strings.Contains(line, "--norestore") {
			profile = ""
		}
		out = append(out, profile)
	}
	return out
}

func TestEachDocumentConversionUsesItsOwnLibreOfficeProfile(t *testing.T) {
	// view_file and read_file of legacy documents run LibreOffice with a fresh user profile per conversion (--norestore), so concurrent conversions never share the default profile.
	// Arrange
	env, log := fakeSoffice(t)
	f, _ := newHostFixture(t, env)
	ctx := tools.WithCall(context.Background(), "call", &recorder{vision: true})
	if _, err := f.call(ctx, "execute_command", `{"command":"printf x > a.docx; printf x > b.docx; printf x > c.doc"}`); err != nil {
		t.Fatal(err)
	}

	// Act
	_, _ = f.call(ctx, "view_file", `{"path":"a.docx"}`)
	_, _ = f.call(ctx, "view_file", `{"path":"b.docx"}`)
	_, _ = f.call(ctx, "read_file", `{"path":"c.doc"}`)

	// Assert
	got := profiles(t, log)
	if len(got) != 3 {
		t.Fatalf("conversions %q", got)
	}
	seen := map[string]bool{}
	for _, p := range got {
		if !strings.HasPrefix(p, "file:///") || seen[p] {
			t.Fatalf("profiles %q", got)
		}
		seen[p] = true
	}
}

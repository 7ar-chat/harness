package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestReadImageRefusesWhatIsNotASmallRegularImageFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644)
	os.Mkdir(filepath.Join(dir, "dir.png"), 0o755)
	big := filepath.Join(dir, "big.png")
	f, _ := os.Create(big)
	f.Truncate(maxImageBytes + 1)
	f.Close()
	cases := map[string]string{
		"extension": filepath.Join(dir, "notes.txt"),
		"missing":   filepath.Join(dir, "none.png"),
		"directory": filepath.Join(dir, "dir.png"),
		"too big":   big,
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := readImage(p); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	a, err := readImage(pngFile(t, dir, "ok.PNG"))
	if err != nil || a.MIME != "image/png" || a.Name != "ok.PNG" {
		t.Fatalf("%+v %v", a, err)
	}
}

func TestCleanPastedPathUndoesTerminalQuotingAndFileURLs(t *testing.T) {
	cases := map[string]string{
		`  /a/b.png  `:            "/a/b.png",
		`'/a/b c.png'`:            "/a/b c.png",
		`"/a/b.png"`:              "/a/b.png",
		`/a/my\ shot.png`:         "/a/my shot.png",
		`file:///a/my%20shot.png`: "/a/my shot.png",
		`'/a/b.png`:               "'/a/b.png", // unbalanced quote stays
		`"`:                       `"`,
	}
	for in, want := range cases {
		if got := cleanPastedPath(in); got != want {
			t.Errorf("cleanPastedPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestImageCommandResolvesRelativePathsInTheWorkspaceAndReportsFailures(t *testing.T) {
	be := &optsBackend{}
	m, root := newWS(t, be, nil)
	pngFile(t, root, "shot.png")

	typeText(m, "/image shot.png")
	press(m, tea.KeyEnter)
	attached := len(m.inp.atts)
	typeText(m, "/image missing.png")
	press(m, tea.KeyEnter)
	typeText(m, "/image readme.txt")
	press(m, tea.KeyEnter)

	tr := m.Transcript()
	if attached != 1 || len(m.inp.atts) != 1 {
		t.Fatalf("attachments %d / %d", attached, len(m.inp.atts))
	}
	if !strings.Contains(tr, "image: ") || !strings.Contains(tr, "readme.txt is not a png, jpg, gif or webp file") {
		t.Fatalf("transcript:\n%s", tr)
	}
}

func TestImageOnABackendWithoutAttachmentsIsRefused(t *testing.T) {
	m, root := newWS(t, &fakeBackend{}, nil)

	typeText(m, "/image "+pngFile(t, root, "a.png"))
	press(m, tea.KeyEnter)

	if len(m.inp.atts) != 0 || !strings.Contains(m.Transcript(), "this backend cannot take attachments") {
		t.Fatalf("atts %d:\n%s", len(m.inp.atts), m.Transcript())
	}
}

func TestPastedImagePathThatCannotBeReadIsNotTypedIntoTheInput(t *testing.T) {
	m, root := newWS(t, &optsBackend{}, nil)

	update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(filepath.Join(root, "gone.png")), Paste: true})

	if m.in.Value() != "" || len(m.inp.atts) != 0 || !strings.Contains(m.Transcript(), "image: ") {
		t.Fatalf("input %q atts %d:\n%s", m.in.Value(), len(m.inp.atts), m.Transcript())
	}
}

func TestLongPastesAreSummarisedAndSentInFull(t *testing.T) {
	// Arrange
	be := &fakeBackend{}
	m, _ := newWS(t, be, nil)
	first := strings.Repeat("line\n", 14) + "end"
	second := strings.Repeat("other\n", 14) + "end"

	// Act
	update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(first), Paste: true})
	typeText(m, " and ")
	update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(second), Paste: true})
	shown := m.in.Value()
	press(m, tea.KeyEnter)

	// Assert
	if shown != "[pasted 15 lines] and [pasted 15 lines #2]" {
		t.Fatalf("input %q", shown)
	}
	if got := lastSend(be); got != first+" and "+second {
		t.Fatalf("sent %q", got)
	}
	if len(m.inp.pastes) != 0 {
		t.Fatalf("pastes kept after the send: %v", m.inp.pastes)
	}
}

func TestShortPasteGoesIntoTheInputAsTyped(t *testing.T) {
	m, _ := newWS(t, &fakeBackend{}, nil)

	update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("two\nlines"), Paste: true})

	if m.in.Value() != "two\nlines" || len(m.inp.pastes) != 0 {
		t.Fatalf("input %q pastes %v", m.in.Value(), m.inp.pastes)
	}
}

func TestClipboardImageResultIsQueuedOrReportedMissing(t *testing.T) {
	be := &optsBackend{}
	m, _ := newWS(t, be, nil)

	update(m, clipImageMsg{data: []byte("\x89PNGdata")})
	queued := m.inp.atts
	update(m, clipImageMsg{})
	update(m, clipImageMsg{err: os.ErrNotExist, data: []byte("x")})

	if len(queued) != 1 || queued[0].a.MIME != "image/png" || queued[0].a.Name != "clipboard-120000.png" {
		t.Fatalf("queued %+v", queued)
	}
	if len(m.inp.atts) != 1 || strings.Count(m.Transcript(), "no image on the clipboard") != 2 {
		t.Fatalf("atts %d:\n%s", len(m.inp.atts), m.Transcript())
	}
}

func TestClipboardImageOnABackendWithoutAttachmentsIsSilentlyDropped(t *testing.T) {
	m, _ := newWS(t, &fakeBackend{}, nil)

	update(m, clipImageMsg{data: []byte("png")})

	if len(m.inp.atts) != 0 || len(m.blocks) != 0 {
		t.Fatalf("atts %d blocks %d", len(m.inp.atts), len(m.blocks))
	}
}

package tui

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/GhaziBenDahmane/harness/hosts/client"
)

// Limits of pasted content.
const (
	maxImageBytes   = 10 << 20
	pasteSummaryLen = 1000 // chars
	pasteSummaryLns = 10   // lines
)

var imageExts = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp",
}

// attachment is an image queued for the next message.
type attachment struct{ a client.Attachment }

// clipImageMsg is the result of reading an image from the system clipboard.
type clipImageMsg struct {
	data []byte
	err  error
}

func init() {
	register(command{name: "image", args: "[path]", help: "attach an image (no path: the clipboard image, when the system can give one)", run: (*Model).cmdImage})
}

func (m *Model) cmdImage(arg string) tea.Cmd {
	if arg == "" {
		return func() tea.Msg { d, err := clipboardImage(); return clipImageMsg{d, err} }
	}
	m.attachImage(arg)
	return nil
}

// attachImage reads the image file at the pasted path and queues it. It is the
// user's own file on their machine, so it may lie outside the workspace.
func (m *Model) attachImage(p string) bool {
	if _, ok := m.be.(RunOptionsBackend); !ok {
		m.add(block{kind: blockNotice, text: "this backend cannot take attachments"})
		return false
	}
	path := cleanPastedPath(p)
	if !filepath.IsAbs(path) && m.inp.workspace != "" {
		path = filepath.Join(m.inp.workspace, path)
	}
	a, err := readImage(path)
	if err != nil {
		m.add(block{kind: blockNotice, text: "image: " + err.Error()})
		return false
	}
	m.inp.atts = append(m.inp.atts, attachment{a})
	return true
}

// readImage loads an image file as an attachment.
func readImage(path string) (client.Attachment, error) {
	mime, ok := imageExts[strings.ToLower(filepath.Ext(path))]
	if !ok {
		return client.Attachment{}, fmt.Errorf("%s is not a png, jpg, gif or webp file", filepath.Base(path))
	}
	st, err := os.Stat(path)
	if err != nil {
		return client.Attachment{}, err
	}
	if !st.Mode().IsRegular() || st.Size() > maxImageBytes {
		return client.Attachment{}, fmt.Errorf("%s is not a regular file under %d MB", filepath.Base(path), maxImageBytes>>20)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return client.Attachment{}, err
	}
	return client.Attachment{Name: filepath.Base(path), MIME: mime, Data: data}, nil
}

// cleanPastedPath undoes what terminals do to a dropped path: quotes,
// backslash-escaped spaces and file:// URLs.
func cleanPastedPath(p string) string {
	p = strings.TrimSpace(p)
	if len(p) >= 2 && (p[0] == '\'' || p[0] == '"') && p[len(p)-1] == p[0] {
		p = p[1 : len(p)-1]
	}
	if strings.HasPrefix(p, "file://") {
		if u, err := url.Parse(p); err == nil {
			p = u.Path
		}
	}
	return strings.ReplaceAll(p, `\ `, " ")
}

// looksLikeImagePath reports whether pasted text is one path to an image file.
func looksLikeImagePath(s string) bool {
	s = cleanPastedPath(s)
	if s == "" || strings.ContainsAny(s, "\n\r") {
		return false
	}
	_, ok := imageExts[strings.ToLower(filepath.Ext(s))]
	return ok
}

// paste handles a bracketed paste. An image path becomes an attachment; a long
// text is summarised in the input ("[pasted 40 lines]") and sent in full; an
// empty paste (a clipboard image, which terminals cannot paste) asks the
// system for one. Anything else goes to the input unchanged.
func (m *Model) paste(text string) (tea.Cmd, bool) {
	switch {
	case text == "":
		return func() tea.Msg { d, err := clipboardImage(); return clipImageMsg{d, err} }, true
	case looksLikeImagePath(text):
		if m.attachImage(text) {
			return nil, true
		}
		return nil, true
	}
	lines := strings.Count(text, "\n") + 1
	if len([]rune(text)) > pasteSummaryLen || lines > pasteSummaryLns {
		label := fmt.Sprintf("[pasted %d lines]", lines)
		for i := 2; m.inp.pastes[label] != ""; i++ {
			label = fmt.Sprintf("[pasted %d lines #%d]", lines, i)
		}
		m.inp.pastes[label] = text
		m.in.InsertString(label)
		return nil, true
	}
	return nil, false
}

// clipImageDone queues the clipboard image, or tells there is none.
func (m *Model) clipImageDone(r clipImageMsg) {
	if _, ok := m.be.(RunOptionsBackend); !ok {
		return
	}
	if r.err != nil || len(r.data) == 0 {
		m.add(block{kind: blockNotice, text: "no image on the clipboard (macOS needs pngpaste or osascript)"})
		return
	}
	name := fmt.Sprintf("clipboard-%s.png", m.now().Format("150405"))
	m.inp.atts = append(m.inp.atts, attachment{client.Attachment{Name: name, MIME: "image/png", Data: r.data}})
}

// clipboardImage reads a PNG from the macOS clipboard with pngpaste, or with
// osascript; best effort: any failure is an error the caller turns into a notice.
func clipboardImage() ([]byte, error) {
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("clipboard images are read on macOS only")
	}
	if p, err := exec.LookPath("pngpaste"); err == nil {
		if out, err := exec.Command(p, "-").Output(); err == nil && len(out) > 0 {
			return out, nil
		}
	}
	f, err := os.CreateTemp("", "har-clip-*.png")
	if err != nil {
		return nil, err
	}
	f.Close()
	defer os.Remove(f.Name())
	script := []string{
		`set png to (the clipboard as «class PNGf»)`,
		fmt.Sprintf(`set fh to open for access POSIX file %q with write permission`, f.Name()),
		`set eof fh to 0`, `write png to fh`, `close access fh`,
	}
	args := []string{}
	for _, l := range script {
		args = append(args, "-e", l)
	}
	if err := exec.Command("osascript", args...).Run(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(f.Name())
	if err == nil && http.DetectContentType(data) != "image/png" {
		err = fmt.Errorf("not a png")
	}
	return data, err
}

// chipsView lists the queued images above the input.
func (m *Model) chipsView(w int) string {
	if len(m.inp.atts) == 0 {
		return ""
	}
	var chips []string
	for _, a := range m.inp.atts {
		chips = append(chips, "[image: "+a.a.Name+"]")
	}
	return styleDim.Render(clip(strings.Join(chips, " ")+"  (backspace removes)", w))
}

// compose builds what is sent for the text the user entered: the pasted texts
// in full, the files @mentioned (read from the workspace), the outputs of the
// !commands run since the last message. It queues the images on the backend
// and adds the "Called Read" notices to the transcript.
func (m *Model) compose(text string) string {
	payload := text
	for label, full := range m.inp.pastes {
		payload = strings.ReplaceAll(payload, label, full)
	}
	m.inp.pastes = map[string]string{}
	if m.inp.workspace != "" {
		blocks, notices := mentionBlocks(readMentions(m.inp.workspace, text))
		for _, n := range notices {
			m.add(block{kind: blockNotice, text: n})
		}
		payload += blocks
	}
	if len(m.inp.shellCtx) > 0 {
		payload = strings.Join(m.inp.shellCtx, "\n\n") + "\n\n" + payload
		m.inp.shellCtx = nil
	}
	if len(m.inp.atts) > 0 {
		if rb, ok := m.be.(RunOptionsBackend); ok {
			as := make([]client.Attachment, len(m.inp.atts))
			for i, a := range m.inp.atts {
				as[i] = a.a
			}
			rb.Attach(as...)
		}
		m.inp.atts = nil
	}
	return payload
}

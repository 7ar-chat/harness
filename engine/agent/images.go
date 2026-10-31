package agent

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/GhaziBenDahmane/harness/ai"
	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/tools"
)

// Harness v3 §3: images are sent once, at most maxRequestImages /
// maxRequestImageBytes per request, then replaced by a text stub (A-13).
const (
	maxRequestImages     = MaxRequestImages
	maxRequestImageBytes = 6 << 20
	imagePathKey         = "work.path"
	// notShownStub / shownStub end the stubs of images not sent (per-request
	// cap) and of images sent in an earlier step.
	notShownStub = "not shown (at most 6 images and 6 MiB per request): view it again if you need it"
	shownStub    = "shown earlier"
)

// MaxRequestImages is the most images one model request carries (attachments
// included: artifacts.Images attaches at most this many).
const MaxRequestImages = 6

// ImageAttachmentKey tags an attachment's image part (Extra) with the
// attachment's artifact id, so its stub names the right file.
const ImageAttachmentKey = "work.attachment"

// addToolImages appends the images the step's tools queued (takeToolImages)
// as one user message after their results (tool messages cannot carry images);
// capImages and imagesSent apply the per-request cap and send-once.
func (st *runState) addToolImages(imgs []tools.Image) {
	if len(imgs) == 0 {
		return
	}
	paths := make([]string, 0, len(imgs))
	for _, img := range imgs {
		paths = append(paths, img.Path)
	}
	parts := []ai.Part{{Type: ai.PartText,
		Text: "[Images attached by the tool calls above, shown once: " + strings.Join(paths, ", ") + "]"}}
	for _, img := range imgs {
		b64 := base64.StdEncoding.EncodeToString(img.Data)
		parts = append(parts, ai.Part{Type: ai.PartImage,
			Image: &ai.Image{Base64Data: b64, MIMEType: img.MIMEType},
			Extra: map[string]any{imagePathKey: img.Path}})
	}
	st.history = append(st.history, &ai.Message{Role: ai.RoleUser, Parts: parts})
}

// imagePaths names the image parts of a message, in order: the tools' paths,
// else the path of the attachment the part was made from (by artifact id).
func imagePaths(m *ai.Message) []string {
	atts := map[string]string{}
	if list, ok := m.Extra[store.ExtraAttachments].([]store.Attachment); ok {
		for _, a := range list {
			atts[a.ArtifactID] = a.Path
		}
	}
	var out []string
	for _, p := range m.Parts {
		if p.Type != ai.PartImage {
			continue
		}
		path, _ := p.Extra[imagePathKey].(string)
		if path == "" {
			id, _ := p.Extra[ImageAttachmentKey].(string)
			path = atts[id]
		}
		out = append(out, path)
	}
	return out
}

func imageStub(path, why string) ai.Part {
	if path == "" {
		path = "attachment"
	}
	return ai.Part{Type: ai.PartText, Text: fmt.Sprintf("[image %s %s]", path, why)}
}

func imageBytes(p ai.Part) int {
	if p.Image == nil {
		return 0
	}
	return len(p.Image.Base64Data) * 3 / 4
}

// stubImages returns m with its image parts replaced (keep reports the parts to keep).
func stubImages(m *ai.Message, why string, keep func(i int, p ai.Part) bool) *ai.Message {
	paths := imagePaths(m)
	parts := make([]ai.Part, 0, len(m.Parts))
	k, changed := 0, false
	for _, p := range m.Parts {
		if p.Type != ai.PartImage {
			parts = append(parts, p)
			continue
		}
		if keep != nil && keep(k, p) {
			parts = append(parts, p)
		} else {
			parts, changed = append(parts, imageStub(paths[k], why)), true
		}
		k++
	}
	if !changed {
		return m
	}
	cp := *m
	cp.Parts = parts
	return &cp
}

// capImages keeps at most maxRequestImages images and maxRequestImageBytes of
// them in a request, the newest first; the others become stubs (copies).
func capImages(msgs []*ai.Message) []*ai.Message {
	count, size := 0, 0
	var out []*ai.Message
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if compactionImages(m) == 0 {
			continue
		}
		// Within a message, the last images are the newest.
		n := compactionImages(m)
		keepFrom := make([]bool, n)
		parts := m.Parts
		k := n - 1
		for j := len(parts) - 1; j >= 0; j-- {
			p := parts[j]
			if p.Type != ai.PartImage {
				continue
			}
			b := imageBytes(p)
			if count < maxRequestImages && size+b <= maxRequestImageBytes {
				keepFrom[k] = true
				count, size = count+1, size+b
			}
			k--
		}
		capped := stubImages(m, notShownStub, func(i int, _ ai.Part) bool { return keepFrom[i] })
		if capped != m {
			if out == nil {
				out = append([]*ai.Message(nil), msgs...)
			}
			out[i] = capped
		}
	}
	if out == nil {
		return msgs
	}
	return out
}

func compactionImages(m *ai.Message) int {
	n := 0
	for _, p := range m.Parts {
		if p.Type == ai.PartImage {
			n++
		}
	}
	return n
}

// imagesSent stubs every image of the history once a model call was made
// with request msgs: an image is sent in one step only. Images the request
// carried become "shown earlier", those the per-request cap left out "not
// shown" (with the same text as in that request); views whose pages were all
// sent now count as views of their document.
func (st *runState) imagesSent(msgs []*ai.Message) {
	sent := map[string]bool{}
	for _, m := range msgs {
		paths := imagePaths(m)
		k := 0
		for _, p := range m.Parts {
			if p.Type == ai.PartImage {
				if k < len(paths) && paths[k] != "" {
					sent[paths[k]] = true
				}
				k++
			}
		}
	}
	for i, m := range st.history {
		if compactionImages(m) == 0 {
			continue
		}
		paths := imagePaths(m)
		shown := stubImages(m, shownStub, func(k int, _ ai.Part) bool {
			return k < len(paths) && paths[k] != "" && !sent[paths[k]]
		})
		st.history[i] = stubImages(shown, notShownStub, nil)
	}
	st.v3.verify.viewsSent(sent)
}

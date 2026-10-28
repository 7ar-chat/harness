package agent

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/store"
	"github.com/GhaziBenDahmane/harness/tools"
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
	parts := []schema.MessageInputPart{{Type: schema.ChatMessagePartTypeText,
		Text: "[Images attached by the tool calls above, shown once: " + strings.Join(paths, ", ") + "]"}}
	for _, img := range imgs {
		b64 := base64.StdEncoding.EncodeToString(img.Data)
		parts = append(parts, schema.MessageInputPart{Type: schema.ChatMessagePartTypeImageURL,
			Image: &schema.MessageInputImage{MessagePartCommon: schema.MessagePartCommon{Base64Data: &b64, MIMEType: img.MIMEType}},
			Extra: map[string]any{imagePathKey: img.Path}})
	}
	st.history = append(st.history, &schema.Message{Role: schema.User, UserInputMultiContent: parts})
}

// imagePaths names the image parts of a message, in order: the tools' paths,
// else the path of the attachment the part was made from (by artifact id).
func imagePaths(m *schema.Message) []string {
	atts := map[string]string{}
	if list, ok := m.Extra[store.ExtraAttachments].([]store.Attachment); ok {
		for _, a := range list {
			atts[a.ArtifactID] = a.Path
		}
	}
	var out []string
	for _, p := range m.UserInputMultiContent {
		if p.Type != schema.ChatMessagePartTypeImageURL {
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

func imageStub(path, why string) schema.MessageInputPart {
	if path == "" {
		path = "attachment"
	}
	return schema.MessageInputPart{Type: schema.ChatMessagePartTypeText, Text: fmt.Sprintf("[image %s %s]", path, why)}
}

func imageBytes(p schema.MessageInputPart) int {
	if p.Image == nil || p.Image.Base64Data == nil {
		return 0
	}
	return len(*p.Image.Base64Data) * 3 / 4
}

// stubImages returns m with its image parts replaced (keep reports the parts to keep).
func stubImages(m *schema.Message, why string, keep func(i int, p schema.MessageInputPart) bool) *schema.Message {
	paths := imagePaths(m)
	parts := make([]schema.MessageInputPart, 0, len(m.UserInputMultiContent))
	k, changed := 0, false
	for _, p := range m.UserInputMultiContent {
		if p.Type != schema.ChatMessagePartTypeImageURL {
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
	cp.UserInputMultiContent = parts
	return &cp
}

// capImages keeps at most maxRequestImages images and maxRequestImageBytes of
// them in a request, the newest first; the others become stubs (copies).
func capImages(msgs []*schema.Message) []*schema.Message {
	count, size := 0, 0
	var out []*schema.Message
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if compactionImages(m) == 0 {
			continue
		}
		// Within a message, the last images are the newest.
		n := compactionImages(m)
		keepFrom := make([]bool, n)
		parts := m.UserInputMultiContent
		k := n - 1
		for j := len(parts) - 1; j >= 0; j-- {
			p := parts[j]
			if p.Type != schema.ChatMessagePartTypeImageURL {
				continue
			}
			b := imageBytes(p)
			if count < maxRequestImages && size+b <= maxRequestImageBytes {
				keepFrom[k] = true
				count, size = count+1, size+b
			}
			k--
		}
		capped := stubImages(m, notShownStub, func(i int, _ schema.MessageInputPart) bool { return keepFrom[i] })
		if capped != m {
			if out == nil {
				out = append([]*schema.Message(nil), msgs...)
			}
			out[i] = capped
		}
	}
	if out == nil {
		return msgs
	}
	return out
}

func compactionImages(m *schema.Message) int {
	n := 0
	for _, p := range m.UserInputMultiContent {
		if p.Type == schema.ChatMessagePartTypeImageURL {
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
func (st *runState) imagesSent(msgs []*schema.Message) {
	sent := map[string]bool{}
	for _, m := range msgs {
		paths := imagePaths(m)
		k := 0
		for _, p := range m.UserInputMultiContent {
			if p.Type == schema.ChatMessagePartTypeImageURL {
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
		shown := stubImages(m, shownStub, func(k int, _ schema.MessageInputPart) bool {
			return k < len(paths) && paths[k] != "" && !sent[paths[k]]
		})
		st.history[i] = stubImages(shown, notShownStub, nil)
	}
	st.v3.verify.viewsSent(sent)
}

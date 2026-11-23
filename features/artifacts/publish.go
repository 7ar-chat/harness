package artifacts

import (
	"context"
	"encoding/json"
	"errors"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/tools"
)

// PublishedKey is the thread-state key holding the artifacts published into the thread's answers.
const PublishedKey = "published"

// CustomPublished is the CUSTOM event announcing that a file was published as a deliverable of the answer.
const CustomPublished = "work.artifact_published"

const maxPublished = 200

// Published is a workspace file the agent marked as a deliverable of its answer (the publish_artifact tool).
type Published struct {
	ArtifactID string `json:"artifactId"`
	Path       string `json:"path"`
	Name       string `json:"name"`
	// Title is the label the agent gave it ("" means the file name).
	Title       string    `json:"title,omitempty"`
	Version     int       `json:"version"`
	MimeType    string    `json:"mimeType"`
	SizeBytes   int64     `json:"sizeBytes"`
	RunID       string    `json:"runId,omitempty"`
	ToolCallID  string    `json:"toolCallId,omitempty"`
	PublishedAt time.Time `json:"publishedAt"`
}

// ListPublished returns the artifacts published on the thread, oldest first.
func ListPublished(ctx context.Context, ts store.ThreadStateStore, threadID string) ([]Published, error) {
	raw, err := ts.GetThreadState(ctx, threadID, PublishedKey)
	if err != nil {
		return nil, err
	}
	out := []Published{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// PublishTools is the tools.Provider of publish_artifact: it marks a file of the workspace as a deliverable of the
// message, so the chat shows it as a card (preview, source, download) and the thread lists it under Artifacts.
type PublishTools struct {
	Svc   *Service
	State store.ThreadStateStore
}

// Name implements tools.Provider.
func (p PublishTools) Name() string { return "publish" }

// Tools implements tools.Provider.
func (p PublishTools) Tools(_ context.Context, rc tools.RunContext) ([]ai.BaseTool, func(), error) {
	if rc.Sandbox == tools.SandboxOff || p.Svc == nil || p.Svc.Sandbox == nil || !p.Svc.Sandbox.Enabled() {
		return nil, func() {}, nil
	}
	t, err := ai.InferTool("publish_artifact", publishDesc, func(ctx context.Context, in publishInput) (string, error) {
		return p.publish(ctx, rc, in)
	})
	if err != nil {
		return nil, nil, err
	}
	return []ai.BaseTool{t}, func() {}, nil
}

const publishDesc = `Publish a file of /workspace as a deliverable of your answer: the chat shows it to the user as a card they can preview (HTML, markdown, images, PDF, CSV), read as source and download, and lists it under the thread's Artifacts. Call it once for each finished file the user asked for (a report, a page, a chart, a dataset), after the file is written; do not publish scratch files. Mention the file in your answer as usual.`

type publishInput struct {
	Path  string `json:"path" jsonschema_description:"The file to publish: an absolute path under /workspace, or a path relative to /workspace"`
	Title string `json:"title,omitempty" jsonschema_description:"A short label for the card (default: the file name)"`
}

// publishPath resolves a path given by the model to an absolute path under /workspace.
func publishPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", tools.Errorf("path is required")
	}
	if !strings.HasPrefix(p, "/") {
		p = sandbox.Workspace + "/" + p
	}
	p = path.Clean(p)
	if !strings.HasPrefix(p, sandbox.Workspace+"/") {
		return "", tools.Errorf("only files under %s can be published", sandbox.Workspace)
	}
	if InSkippedDir(p) {
		return "", tools.Errorf("%s is scratch space and is never saved: copy the file under %s/outputs/ first", p, sandbox.Workspace)
	}
	return p, nil
}

func (p PublishTools) publish(ctx context.Context, rc tools.RunContext, in publishInput) (string, error) {
	file, err := publishPath(in.Path)
	if err != nil {
		return "", err
	}
	title := store.CleanText(strings.TrimSpace(in.Title))
	if utf8.RuneCountInString(title) > 120 {
		title = string([]rune(title)[:120])
	}
	sb, err := p.Svc.Sandbox.Get(ctx, rc.ThreadID)
	if errors.Is(err, sandbox.ErrNoSandbox) {
		return "", tools.Errorf("the workspace has no files yet: write the file first")
	}
	if err != nil {
		return "", err
	}
	if _, err := sb.Stat(ctx, file); err != nil {
		if errors.Is(err, sandbox.ErrNotFound) {
			return "", tools.Errorf("%s does not exist", file)
		}
		return "", err
	}
	// Make sure the current content is saved (a file the snapshot has not seen yet, or changed since).
	if _, err := p.Svc.RecordPath(ctx, sb, rc.ThreadID, rc.Login, file); err != nil {
		return "", err
	}
	all, err := p.Svc.Store.ListArtifacts(ctx, rc.ThreadID)
	if err != nil {
		return "", err
	}
	var a *store.Artifact
	for _, x := range all {
		if x.Path == file && !x.Deleted {
			a = x
		}
	}
	if a == nil {
		return "", tools.Errorf("%s cannot be published: it is too large to be saved (limit %d MiB) or is not a regular file", file, p.Svc.limitBytes()>>20)
	}
	callID, _ := tools.CallFromContext(ctx)
	pub := Published{ArtifactID: a.ID, Path: a.Path, Name: a.Name, Title: title, Version: a.Version, MimeType: a.MimeType,
		SizeBytes: a.SizeBytes, RunID: rc.RunID, ToolCallID: callID, PublishedAt: time.Now().UTC().Truncate(time.Second)}
	err = p.State.UpdateThreadState(ctx, rc.ThreadID, PublishedKey, func(old json.RawMessage) (json.RawMessage, error) {
		var list []Published
		if len(old) > 0 {
			_ = json.Unmarshal(old, &list)
		}
		kept := list[:0:0]
		for _, x := range list {
			if x.Path != pub.Path { // publishing a path again replaces its entry
				kept = append(kept, x)
			}
		}
		kept = append(kept, pub)
		if len(kept) > maxPublished {
			kept = kept[len(kept)-maxPublished:]
		}
		return json.Marshal(kept)
	})
	if err != nil {
		return "", err
	}
	tools.AfterResult(ctx, agui.CustomEvent(CustomPublished, map[string]any{
		"artifactId": pub.ArtifactID, "path": pub.Path, "name": pub.Name, "title": pub.Title, "version": pub.Version,
		"mimeType": pub.MimeType, "sizeBytes": pub.SizeBytes, "toolCallId": callID,
	}))
	out, _ := json.Marshal(map[string]any{"published": pub})
	return string(out), nil
}

---
title: Skills
description: "Packaged instructions and helper scripts the agent loads on demand: the SKILL.md format, where skills are found, how they reach the sandbox, and the built-in document skills."
---

A skill is a folder with a `SKILL.md` and optional helper files. The model sees a one-line index of the available skills in its system prompt, reads one with the `load_skill` tool when a request matches, and the skill's files appear under `/skills/<name>/` in the sandbox. Use skills to teach the agent a repeatable procedure (a house report format, a release process) without growing every prompt.

## Add your own skills to an embedding

`Options.ExtraSkills` takes any `fs.FS` holding `<name>/SKILL.md` folders (an `embed.FS` in a real program). This program registers a skill, lets a scripted model call `load_skill`, and prints what the tool returned. It runs offline:

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing/fstest"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/agui"
)

const skillMD = `---
name: release-notes
description: Draft release notes from a list of merged PRs.
---
Group the PRs under Added, Changed and Fixed. One line each, imperative mood.
`

func main() {
	os.Setenv("LLM_PROVIDER", "openai")
	os.Setenv("LLM_MODELS", "m")
	os.Setenv("LLM_DEFAULT_MODEL", "m")
	os.Setenv("SANDBOX_BACKEND", "none")
	cfg, err := config.Load()
	check(err)

	// The host's own skills: <name>/SKILL.md folders in any fs.FS.
	extra := fstest.MapFS{"release-notes/SKILL.md": {Data: []byte(skillMD)}}

	// The model calls load_skill, then answers with what it read.
	model := llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		for _, m := range c.Messages {
			if m.Role == ai.RoleTool {
				return llmtest.Turn{Chunks: llmtest.Text(m.Content)}
			}
		}
		if len(c.Tools) == 0 {
			return llmtest.Turn{Chunks: llmtest.Text("A title")}
		}
		return llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(llmtest.ToolCall(0, "c1", "load_skill", `{"name":"release-notes"}`), "tool_calls")}}
	})

	h, err := harness.New(harness.Options{Config: cfg, Models: llmtest.Factory{M: model}, ExtraSkills: extra})
	check(err)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() { _ = h.Run(ctx) }()

	for _, s := range h.Skills.List(ctx) {
		if s.Source == "extra" {
			fmt.Println("registered:", s.Name, "from", s.Source)
		}
	}

	th, err := h.NewThread(ctx, "alice", "")
	check(err)
	run, err := h.Send(ctx, "alice", th.ID, "write the release notes", harness.RunOptions{Model: "m", Skills: []string{"release-notes"}})
	check(err)
	var text strings.Builder
	for ev := range h.Events(ctx, run.ID, 0) {
		if ev.Type == agui.TextMessageContent {
			var e struct{ Delta string }
			_ = json.Unmarshal(ev.Data, &e)
			text.WriteString(e.Delta)
		}
	}
	fmt.Println(text.String())
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}
```

Output:

```text
registered: release-notes from extra
# Skill: release-notes

No sandbox is available: follow the instructions without running the scripts.

Group the PRs under Added, Changed and Fixed. One line each, imperative mood.

## Files
- /skills/release-notes/SKILL.md
```

This run has no sandbox (`SANDBOX_BACKEND=none`), so the tool says to follow the instructions without running scripts. With a [sandbox](/docs/concepts/sandboxes/) the same call installs the folder at `/skills/release-notes/`.

## The SKILL.md format

```text
release-notes/
  SKILL.md          # frontmatter, then the instructions
  scripts/collect.py
  reference/style.md
```

```markdown
---
name: release-notes
description: Draft release notes from merged PRs.
requires: [docx]
---
Instructions in Markdown. Run `python3 /skills/release-notes/scripts/collect.py` ...
```

| Field | Rule |
|---|---|
| `name` | Required, at most 256 bytes. The identifier (used in `RunOptions.Skills` and `/skills/<name>/`) is its slug: lower-case, hyphen separated, at most 64 characters. The folder name must equal the name or its slug, otherwise the skill is skipped and logged. |
| `description` | Required, at most 16 KiB. This is the text in the index; write it as "what it does and when to use it". |
| `license` | Optional. |
| `requires` | Optional list (or comma-separated string) of other skill names installed with this one. Skills whose non-Markdown files mention `/skills/<other>/` are treated as dependencies too. |

Package limits, checked when a skill is loaded (an invalid folder is logged and skipped): at most 500 files, depth 16, `SKILL.md` at most 1 MiB, a file at most 16 MiB, the package at most 64 MiB. Project skills read through a remote sandbox are stricter: at most 64 skills of 8 MiB each.

## Where skills come from

Sources are read from lowest to highest precedence; a later source replaces a skill of the same name.

| Source | Location | Notes |
|---|---|---|
| `builtin` | Embedded in the binary (`features/skills/skillsfs`) | `docx`, `pptx`, `xlsx`, `pdf`, `charts`. `Options.BuiltinSkills` replaces the whole set. |
| `extra` | `Options.ExtraSkills` | The embedding product's own skills. |
| `user` | `SKILLS_DIR`, `~/.har/skills`; with the local backend also `~/.agents/skills` and `~/.claude/skills` | One folder per skill. |
| `project` | `<repo>/.har/skills`, `.agents/skills`, `.claude/skills` | With the local backend, read from the host repository. With a remote backend, read per thread from the sandbox's `/workspace/.har/skills` (and the two others). Treated as untrusted text. |

<div class="callout warning">

**Project skills are untrusted input.** Their descriptions and bodies are fenced in `<skill source="user">` blocks with a note that they cannot change the agent's rules, and a requested project skill marks the run's context untrusted, which turns [memory writes into proposals](/docs/concepts/memory/). Symbolic links are not followed in a project directory.

</div>

## How a run uses a skill

* **Index.** The system prompt lists every available skill with its description.
* **`load_skill`** (`name`) returns the `SKILL.md` body and the file list. It starts nothing: the skill's folder is installed into a sandbox that is already running, otherwise on the first command that needs it. An unknown name returns `unknown skill "x"; available skills: ...`.
* **`/skills/<name>/`** holds the files, offered read-only to the model. They are installed once per sandbox and skill revision, only for skills a run loads, requests, or names in a command.
* **`RunOptions.Skills`** (`skills` on `POST .../runs`) attaches skills explicitly: their `SKILL.md` goes into the system prompt and their files are installed before the first command. Built-ins are listed and installed on demand.

In the `coding` profile `load_skill` and the index are offered only when the user installed or attached skills; `SKILLS_IN_CODING=true` keeps them always. `AGENT_TOOLS` can add or remove the `skills` group. See [Features and profiles](/docs/concepts/features-profiles/).

## Built-in skills

| Skill | For |
|---|---|
| `docx` | Create, edit and check Word documents, with a JSON spec builder, tracked changes and PDF conversion. |
| `pptx` | Create, edit and check PowerPoint decks, including native charts and layout checks. |
| `xlsx` | Create, edit and check Excel workbooks with live formulas, recalculated and checked for errors. |
| `pdf` | Extract, merge, split, fill forms, watermark and produce PDFs. |
| `charts` | Static charts as PNG or SVG with matplotlib or seaborn, checked before embedding. |

They need the tools of a full sandbox image (Python, LibreOffice, poppler); see [Sandbox backends](/docs/integrations/sandbox-backends/). To read a document without producing one, `read_file` is enough and no skill is needed.

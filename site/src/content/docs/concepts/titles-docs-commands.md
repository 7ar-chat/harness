---
title: "Titles, project docs, commands"
description: "Three small features around a conversation: model-written thread titles, the instruction files (AGENTS.md and friends) loaded into the coding prompt, and custom slash commands."
---

Three features make a conversation easier to find and to steer: **titles** name a thread from its first exchange, **project docs** put the repository's and the user's instruction files in front of the coding agent, and **commands** turn markdown files into reusable `/slash` prompts. All three are in the `work` and `coding` [profiles](/docs/concepts/features-profiles/) (commands are a host feature of the TUI and the HTTP API, not a profile feature).

## Titles

A thread whose title was not set by the user starts with a title derived from its first message. After the thread's first completed run, the `titles` hook asks the run's model for a better one:

- The prompt asks for 3 to 7 words, at most 60 characters, in the language of the user's message, with no quotes, trailing period, emoji or `Title:` prefix. The call sees the first user message (2000 characters) and the last answer (1000 characters) and runs at low reasoning effort for models that support it.
- The model's answer is cleaned (first non-empty line, quotes and `Title:` prefixes removed, capped at 60 characters with `…`).
- The run waits up to **3 seconds** for the title, then emits a `work.title` custom event with `{"title": "..."}` and counts the usage in the run's totals. A slower title is still saved when it arrives, after the run, without an event; a thread list shows it on its next refresh.
- The title is saved only while the thread's title source is still `auto`. A title set by the user (`PATCH /api/threads/{id}` with `{"title": ...}`, or a non-empty title passed to `NewThread`) has source `user` and is never overwritten. A generated title has source `generated`.
- Failed, cancelled and compaction runs are not titled. A failed title call is logged and the thread keeps its derived title.

<div class="callout note">

**No separate title model.** The title call uses the model of the run. `agents.title` in `config.yml` is rejected with `agents.title is not available yet: that role uses the model of the run`.

</div>

## Project docs

For the **coding** prompt variant (`AGENT_PROFILE=coding`), the `projectdocs` feature reads the instruction files that tell an agent how to build and test a project, and adds them to the system prompt. The other variants get nothing from it. Everything but the user's own file is read through the sandbox, so it works with remote sandboxes.

### What is read

Lowest priority first; a later file wins where files disagree, and all of them are shown to the model.

| # | File | When |
|---|---|---|
| 1 | `~/.har/AGENTS.md` | local sandbox backend, or `HARNESS_USER_INSTRUCTIONS=1`; the only trusted file |
| 2 | `.github/copilot-instructions.md`, `.cursorrules`, `.cursor/rules/*.mdc` with `alwaysApply: true` | only when the repository root has no `AGENTS.md` |
| 3 | `AGENTS.md`, else `CLAUDE.md`, else `GEMINI.md` at the repository root | the first that exists; the others are not also loaded |
| 4 | `.har/instructions.md` | always, last |

Each file is wrapped in a `<project_instructions file="..." source="project|user">` block, and the text cannot close the block or open a fake one. The text is computed once per run, so the system prompt is byte-identical at every step even if the agent edits an instruction file (prompt caching).

Because the repository's text is data, not the harness's own instructions, the run's context is reported untrusted whenever a project file was loaded: memory writes are only proposed. See [Memory](/docs/concepts/memory/).

### Size caps

| Cap | Value |
|---|---|
| One instruction file (and one imported file) | 16 KiB |
| All instructions in the system prompt | 48 KiB |
| Notes of one subdirectory | 8 KiB |
| `@import` depth | 3 |

A file over its cap is cut with `[…cut: read_file <path> for the rest]`; when the total is reached, the text is cut with `[…cut: size limit of the instructions reached]` and further files are replaced by `[…further instruction files omitted: size limit 49152 bytes reached]`.

### `@imports`

Inside an instruction file, `@path/to/file` appends that file as `[imported from <path>]`. The path is relative to the importing file, each file is imported once, and imports stay inside the workspace (symbolic links are resolved and refused if they leave it). A reference needs a `/` or a `.`, so `@alice` is a mention, not an import; text inside code spans and fenced blocks is ignored; references starting with `~` are never followed. In `~/.har/AGENTS.md` imports stay inside `~/.har`.

### Nested files

An `AGENTS.md` (else `CLAUDE.md`) in a subdirectory applies to the files under it, and the closest one wins. These notes are **not** in the system prompt. The first time `read_file`, `write_file` or `edit_file` touches a path below such a directory in a run, the note is appended once to that tool's result:

```text
[Instructions for pkg/sub (pkg/sub/AGENTS.md, repository notes for files under this directory): ...]
```

Cursor rules with `globs` work the same way when a matching file is touched (and only when the repository has no root `AGENTS.md`).

### Compatibility

| Standard | Read | Not read |
|---|---|---|
| `AGENTS.md` | repo root, nested directories, `~/.har/AGENTS.md` | `AGENTS.override.md`, `~/.codex/` |
| `CLAUDE.md` | repo root when there is no `AGENTS.md`; nested directories; `@imports` | `~/.claude/CLAUDE.md`, `CLAUDE.local.md`, `.claude/settings.json`, hooks |
| `GEMINI.md` | repo root when there is neither of the above | `~/.gemini/` |
| Cursor | `.cursorrules`, `.cursor/rules/*.mdc` (`alwaysApply`, `globs`) | rules that are only "agent requested", `.cursor/mcp.json` |
| Copilot | `.github/copilot-instructions.md` | `.github/instructions/*.instructions.md` |

The builtin `/init` command asks the agent to write or update `AGENTS.md` for the repository (see below). Project skills and MCP files follow the same layout rules: [Skills](/docs/concepts/skills/), [MCP](/docs/concepts/mcp/).

## Custom commands

A command is a markdown file that a user runs as `/name arguments`. The body is a prompt template; expanding it only produces text, which is then sent as a normal user message. Nothing is executed or read at expansion time, so the usual tools and approvals still apply.

```markdown
---
description: Review a file
argument-hint: <file>
---
Review $1 for bugs. Extra instructions: $ARGUMENTS
```

Save it as `.har/commands/review.md` in a project (or in `~/.har/commands/` for every project), then run `/review src/main.go "check error handling"`.

### Where commands come from

| Source | Location | Label |
|---|---|---|
| builtin | `/init` | `builtin` |
| skills | each installed skill, as `/<skill>`: asks the agent to load that skill for `$ARGUMENTS` | `skill` |
| user | `<user dir>/commands/**/*.md` (`~/.har/commands`) | `user` |
| project | `<project>/.har/commands/**/*.md` (nearest `.har` up to the git root) | `project` |

On a name clash the higher one wins: project, then user, skill, builtin. A file in a subfolder is named `folder:name` (`git/summary.md` becomes `/git:summary`). Names match `[A-Za-z0-9][A-Za-z0-9._-]*` with `:` between parts and at most 80 characters.

Files are read from disk on every request, so an edit is picked up without a restart; the TUI's `/reload` refetches the list. Symbolic links, files that are not regular, files over 64 KiB, an unclosed frontmatter and an empty body are skipped with a warning. A repository must not be able to point a command at a file elsewhere on the machine.

### Placeholders

| Placeholder | Replaced by |
|---|---|
| `$ARGUMENTS` | the whole argument text |
| `$1` to `$9` | the arguments split on spaces; single or double quotes group words (the quotes are dropped); a missing argument becomes empty |

Substitution is one pass, so a placeholder inside an argument is not expanded again. A template with no placeholder gets the arguments appended after a blank line. The frontmatter keys are `description` (default: the first line of the body) and `argument-hint`.

This program loads a project directory and expands a command offline:

```go
package main

import (
	"fmt"

	"github.com/7ar-chat/harness/features/commands"
)

func main() {
	set := commands.Load(commands.Options{ProjectDir: "proj/.har/commands"})
	for _, c := range set.List() {
		fmt.Printf("%-12s %-8s %q\n", c.Name, c.Source, c.Description)
	}
	prompt, err := set.Expand("review", `src/main.go "check error handling"`)
	if err != nil {
		panic(err)
	}
	fmt.Println(prompt)
	_, err = set.Expand("nope", "")
	fmt.Println(err)
}
```

With `review.md` as above and a `git/summary.md` containing `Summarise the changes on this branch.`, it prints:

```text
git:summary  project  "Summarise the changes on this branch."
init         builtin  "Create or update AGENTS.md for this repository"
review       project  "Review a file"
Review src/main.go for bugs. Extra instructions: src/main.go "check error handling"
unknown command: "nope"
```

### From a front end

The `hosts/client` contract has `Commands(ctx)` and `ExpandCommand(ctx, name, args)`; the HTTP server exposes them:

```bash
curl -s localhost:8000/api/commands
curl -s -X POST localhost:8000/api/commands/review/expand \
  -H 'Content-Type: application/json' -d '{"args":"src/main.go"}'
# {"prompt":"Review src/main.go for bugs. Extra instructions: src/main.go"}
```

`GET /api/commands` returns `[{name, description, source, argHint}]`; an unknown name on `expand` is a not-found error. The terminal UI turns each command into a slash command, expands it on the server and sends the result as a user message. Names a builtin already has, or that cannot be typed as `/command` (the UI accepts lower-case letters and `-`), are not available and are listed by `/help`. See the [HTTP API](/docs/reference/http-api/) and the [Client contract](/docs/reference/client-contract/).

Related: [Threads, runs and events](/docs/concepts/threads-runs-events/), [Configuration files](/docs/reference/configuration/).

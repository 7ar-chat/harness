---
title: Memory
description: "Long-term, per-user memory for the agent: entries with scopes and tags, a write policy with secret redaction, recall into the system prompt, and a consolidation job."
---

The memory feature (`features/memory`) lets the agent keep durable facts, preferences and decisions across conversations. Entries are stored per user in the [store](/docs/concepts/store/); the agent reads and writes them with the `memory_*` tools, and the most relevant ones are put into the system prompt at the start of each run. Use it for knowledge that should outlive one thread; use [project docs](/docs/concepts/titles-docs-commands/) for instructions that live in the repository.

## Use the service directly

The service works on any `store.MemoryStore`, so you can try the policy without a model. This program uses the in-memory store and runs offline:

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/7ar-chat/harness/adapters/store/memstore"
	"github.com/7ar-chat/harness/features/memory"
)

func main() {
	ctx := context.Background()
	svc := memory.New(memstore.New(), memory.Options{WriteMode: memory.WriteStrict})
	alice := memory.Writer{Login: "alice", ThreadID: "t1"}

	// Strict mode refuses a write that has no trigger.
	r := svc.Create(ctx, alice, memory.CreateInput{Content: "tabs width is 4"})
	fmt.Println("no trigger:", r.IsError, r.Data["error"].(map[string]any)["code"])

	// "Remember that" is a trigger; a pinned entry is recalled in every run.
	r = svc.Create(ctx, alice, memory.CreateInput{
		Content: "Remember that we deploy with `make release`. api_key=sk-abcdefghijklmnopqrstuv",
		Tags:    []string{"pinned"}, Scope: "repository", Repository: "acme/shop",
	})
	b, _ := json.Marshal(r.Data["memory"])
	fmt.Println("created:", r.IsError, string(b)[:60]+"...")

	// An untrusted writer (after reading a web page, say) can only propose.
	r = svc.Create(ctx, memory.Writer{Login: "alice", ThreadID: "t1", Untrusted: true},
		memory.CreateInput{Content: "We decided to use Postgres 16."})
	fmt.Println("untrusted:", r.Changes[0].Action, r.Data["confirmation"] != nil)

	// Recall: the pinned entry, then the entries matching the question.
	block, info := svc.RecallBlock(ctx, svc.Layers(ctx, "alice", "t1"), "how do we deploy?")
	fmt.Println(len(info.Pinned), "pinned;", len(info.Relevant), "relevant")
	fmt.Println(block)
}
```

Output (the timestamp of `created` varies):

```text
no trigger: true policy_violation
created: false {"created_at":"2026-10-31T14:51:39.441617Z","embedding_updat...
untrusted: proposed true
1 pinned; 0 relevant
<user_memories>
- [user] Remember that we deploy with `make release`. api_key= [REDACTED]
  Remember that we deploy with `make release`. api_key= [REDACTED]
</user_memories>
```

The secret was redacted before storage. The untrusted write became a proposal (`pending`), so it is not recalled.

## Install it

`harness.Memory()` is part of the `work` profile. In the `coding` profile the memory tools and prompt are left out unless `MEMORY_IN_CODING=true` (they cost tokens in every request and a coding agent rarely needs them); `harness-web -team` turns it on for you. `AGENT_TOOLS` with `+memory` or `-memory` adds or removes the whole `memory_*` group. See [Features and profiles](/docs/concepts/features-profiles/).

A user can switch their memory off; the tools and recall then disappear for that user.

## Scopes

Every entry carries a `scope` and a `repository` label (default `"default"`). They are stored with the entry and `memory_search` filters on them; the three scopes are `personal` (the default), `repository` and `workspace`, so a repository fact can be kept apart from a personal preference.

`HARNESS_MEMORY_DEFAULT_SCOPE` sets the default for new entries. `memory_search` filters on `scope` and `repository`. The memory itself belongs to a *key*: the user's login by default. A host can return another key from `Options.MemoryKey`, which is how the team product shares one project memory between members, with `Options.MemoryUserKey` for each member's private layer.

## Writing: triggers, modes, redaction

A write needs a **trigger**: `metadata.trigger` set to one of `explicit_remember`, `durable_preference`, `accepted_decision`, `stale_correction`, `explicit_revocation`, `explicit_contradiction`, `discovered_fact` or `investigation`. Without one, the service infers it from `metadata.reason`, `metadata.user_message` and the content with simple patterns (for example "remember that" or "we decided"). Deletes need a revocation, contradiction or stale-correction trigger.

| `HARNESS_MEMORY_WRITE_MODE` | A write with no trigger |
|---|---|
| `warn` (default) | Allowed; the violation `write_trigger_missing` is reported in the tool result. |
| `strict` | Refused with a `policy_violation` error. |

| `HARNESS_MEMORY_DELETE_MODE` | Effect of `memory_delete` |
|---|---|
| `soft` (default) | The entry becomes `deleted` and is purged after `HARNESS_MEMORY_DELETED_RETENTION_DAYS`. |
| `hard` | The entry is removed. |

Content, metadata, tags and queries are scanned for secrets (inline `api_key=`/`token=`/`password=` pairs, `sk-...` keys, GitHub tokens, AWS access keys, private-key blocks) and replaced with `[REDACTED]`. Sensitive metadata keys such as `password` or `*_token` are redacted too.

<div class="callout note">

**Untrusted context turns writes into proposals.** When the run has read untrusted text (a project skill requested for the run, or instruction files read from the repository), `memory_create`, `memory_update` and `memory_delete` do not change active memory: the entry is stored as `pending` and the tool tells the model it waits for the user's confirmation. Pending entries are never recalled or read back. With `HARNESS_MEMORY_EXTRACT=true`, a model also proposes memories from each completed run; those are always `pending`.

</div>

The review pages where a person keeps or discards proposals (`/api/memory/{id}/confirm` and `/reject`, the Settings > Memory screen) are part of the web product (`app/product`), not of the library or the TUI.

## Recall

When `HARNESS_MEMORY_RECALL_INJECTION` is true (default), the start of each run gets a `<user_memories>` block built from:

1. **Pinned** entries (tag `pinned` or `metadata.pinned: true`) of every layer, within `PinnedBudget` (2,500 characters; one entry is cut at 1,200).
2. Entries that **match the user's last message**: word overlap with light stemming, or meaning too when an embedder is configured; at most 6, above a minimum relevance, within 6,500 characters in total.
3. With no match, the latest decisions, facts and constraints.

`HARNESS_MEMORY_EMBEDDING_BACKEND` is `simple` (word overlap, default) or `openai` (vectors from an OpenAI-compatible `/embeddings` endpoint, with `HARNESS_MEMORY_EMBEDDING_URL`, `_KEY` and `_MODEL`; URL and key default to the LLM's). `disabled`, `none` and `keyword` only change the capabilities the service reports.

## Tools

`memorytools.Names` lists the eight tools:

| Tool | Purpose |
|---|---|
| `memory_search` | Rank entries by `query`, with `scope`, `repository`, `top_k` (1-50, default 5) and `filters` (including `path_prefix`). |
| `memory_get` | Read one entry by `id` or `path`. |
| `memory_create`, `memory_create_batch` | Add one entry or up to 100. In a project, `audience: "me"` writes to the user's private layer. |
| `memory_update` | Change content, tags or metadata of an entry. A concurrent change gives `memory_conflict`. |
| `memory_delete`, `memory_delete_batch` | Delete per the delete mode. |
| `memory_capabilities` | Report which features the backend supports. |

## Limits and consolidation

| Variable | Default | Meaning |
|---|---|---|
| `HARNESS_MEMORY_MAX_ENTRIES` | `1000` | Active entries per memory; `<= 0` means no limit. Pending proposals have their own cap of 100. |
| `HARNESS_MEMORY_MAX_CONTENT_BYTES` | `200000` | Size of one entry (`content_too_large`). |
| `HARNESS_MEMORY_CONSOLIDATION_HOURS` | `168` | Period of the consolidation job; `<= 0` disables it. |
| `HARNESS_MEMORY_DELETED_RETENTION_DAYS` | `30` | Age after which soft-deleted entries are purged. |
| `HARNESS_MEMORY_EXTRACT`, `HARNESS_MEMORY_EXTRACT_MODEL` | off, the run's model | Propose memories after each completed run. |

The `memory-consolidation` job is registered with the runtime as a periodic [job](/docs/concepts/infra/). It removes duplicate lines from the aggregated semantic files (`decisions.md`, `facts.md`, `constraints.md`) and purges soft-deleted entries past the retention. All keys are also settable under `memory:` in `config.yml` (`write_mode`, `delete_mode`, `default_scope`, `max_entries`, `in_coding`, ...); see [config.yml keys](/docs/reference/config-yml/) and [Environment variables](/docs/reference/env-vars/).

---
title: LLM providers
description: Point the harness at an OpenAI-compatible endpoint, the OpenAI Responses API or Anthropic, and tune models, keys, reasoning, presets and timeouts.
---

The harness talks to one model provider per process. Pick a built-in type (`openai` for any OpenAI-compatible endpoint, `anthropic`) or define a named provider in your user `config.yml`. This page covers selecting the provider, the model allow-list, API keys and the per-model knobs. For using the model client without the harness, see [Use the ai module on its own](/docs/guides/use-ai/).

## Minimal setup

<div class="tabs">

<div class="tab" data-label="Environment">

```bash
# OpenAI-compatible endpoint (Chat Completions)
export LLM_BASE_URL=https://api.openai.com/v1
export LLM_API_KEY=sk-...
export LLM_MODELS=gpt-5.6-terra,gpt-5.6-sol
export LLM_DEFAULT_MODEL=gpt-5.6-terra

# Anthropic
export LLM_PROVIDER=anthropic
export LLM_API_KEY=sk-ant-...
export LLM_MODELS=claude-sonnet-5-5
export LLM_DEFAULT_MODEL=claude-sonnet-5-5
```

</div>

<div class="tab" data-label="config.yml">

```yaml
# ~/.har/config.yml (the user file; the key itself goes in auth.json or the environment)
provider: opencode
providers:
  opencode:
    type: openai
    base_url: https://opencode.ai/zen/go/v1
    session_header: x-opencode-session
    models: [deepseek-v4.1-flash]
    default_model: deepseek-v4.1-flash
    api_key_env: OPENCODE_KEY      # optional: the NAME of a variable
  claude:
    type: anthropic
    cache_ttl: 1h
    models: [claude-sonnet-5-5]
    default_model: claude-sonnet-5-5
```

</div>

<div class="tab" data-label="Go">

```go
package main

import (
	"log"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai/llm"
	"github.com/7ar-chat/harness/config"
)

func main() {
	cfg, err := config.Load() // reads LLM_* from the environment and the config.yml files
	if err != nil {
		log.Fatal(err)
	}

	// harness.New builds exactly this when Options.Models is nil.
	models := llm.New(llm.Options{
		Provider:        cfg.LLMProvider, // "openai" or "anthropic"
		BaseURL:         cfg.LLMBaseURL,
		APIKey:          cfg.LLMAPIKey,
		Models:          cfg.LLMModels, // allow-list: any other id is refused
		ResponsesModels: cfg.LLMResponsesModels,
		TimeoutSeconds:  cfg.LLMTimeout,
		SessionHeader:   cfg.LLMSessionHeader,
		CacheTTL:        cfg.LLMCacheTTL,
	})

	h, err := harness.New(harness.Options{Config: cfg, Models: models})
	if err != nil {
		log.Fatal(err)
	}
	_ = h
}
```

</div>

</div>

`Options.Models` takes any `llm.Factory` (one method, `Model(ctx, name)`). In tests, pass the scripted `llmtest.Factory{M: llmtest.New(...)}` so nothing touches the network; see [Test your agent](/docs/guides/testing/).

In the CLI, `harness -provider NAME` selects a provider for one run, and `harness -config-dump` prints each effective value with its source (the key is masked).

## Provider types

| Type | Wire protocol | Base URL default | Notes |
|---|---|---|---|
| `openai` (default) | Chat Completions, `POST {base}/chat/completions` | `https://api.openai.com/v1` | Any compatible endpoint: set `LLM_BASE_URL`. Model ids are sent verbatim. |
| `openai` + Responses | `POST {base}/responses` | same | For the model ids listed in `LLM_RESPONSES_MODELS`. Reasoning items are carried across tool steps and runs. Other models stay on Chat Completions. |
| `anthropic` | Messages, `POST {base}/v1/messages` | `https://api.anthropic.com` | Key is sent as `x-api-key`. Supports `LLM_CACHE_TTL`. |

`LLM_RESPONSES_MODELS` defaults to `gpt-5.6-sol,gpt-5.6-terra`; an id that is not in `LLM_MODELS` has no effect. Set it to `none` to use Chat Completions for every model.

<div class="callout note">

**Responses is an OpenAI-endpoint feature.** The factory checks `LLM_RESPONSES_MODELS` before the provider type, so listing a model there routes it to `{LLM_BASE_URL}/responses` whatever `LLM_PROVIDER` is. Only point it at endpoints that implement the Responses API.

</div>

## The model allow-list

`LLM_MODELS` (comma separated, default `gpt-5.6-sol,gpt-5.6-terra`) is an allow-list: a run that names another model is refused with `model "x" is not allowed`. Around it:

- `LLM_DEFAULT_MODEL` (default `gpt-5.6-terra`) is the model of new threads. If it is missing from `LLM_MODELS` it is added at the front.
- `EXPLORE_MODEL`, when set and missing, is appended.
- `LLM_DISCOVER_MODELS=true` asks the provider at startup which models it serves and appends them to the list.

### Model discovery

With `LLM_DISCOVER_MODELS=true`, `harness.New` calls `GET {base}/models` (Anthropic: `GET {base}/v1/models`) once, with an 8-second timeout, and appends the returned ids after the configured ones. The default model does not change. If the provider cannot be asked, the harness logs `model discovery failed; using LLM_MODELS only` and starts with the configured list.

It is off by default because discovery widens an allow-list to everything the key can reach. It is read once per process start. `llm.Discover(ctx, client, llm.Options{...})` is the exported function behind it.

<div class="callout warning">

**A project file cannot enable discovery.** In `config.yml`, `discover_models` (like `models`, `presets`, `default_preset`, `providers`, `base_url`, `session_header`, `cache_ttl`, `timeout_seconds` and `api_key_env`) is accepted only in the user file or the environment. A repository you clone must not decide where your requests go or which models exist; a project file that sets one is an error naming `~/.har/config.yml`.

</div>

## Named providers

`providers:` in the user `config.yml` defines providers you select with `provider: NAME` (a project file may select one of yours) or `-provider NAME`. Each entry accepts `type`, `base_url`, `session_header`, `cache_ttl`, `models`, `default_model` and `api_key_env`; any other key is an error, and so is an `api_key`/`password`/`secret`/`token`. Names match `[A-Za-z0-9][A-Za-z0-9._-]*`.

A bare `provider: openai` or `anthropic` stays the built-in type. The values of the selected provider sit above the flat keys of the files and below the environment, so `LLM_BASE_URL` in the environment still wins. `LLM_PROVIDER` must be `openai`, `anthropic` or the name of a defined provider; anything else fails with `LLM_PROVIDER must be openai, anthropic or the name of a provider defined under providers: in a config.yml, got "x"`.

## API keys

The key of the selected provider is resolved in this order; the first match wins.

1. `LLM_API_KEY` in the environment (or a flat `api_key_env:` in a file, which fills it).
2. The environment variable named by the provider's `api_key_env`.
3. The provider's entry in `auth.json`.
4. The conventional variable of its type: `OPENAI_API_KEY` or `ANTHROPIC_API_KEY`. A named provider that sets its own `base_url` skips this step, so an OpenAI key is never sent to another host.

`auth.json` lives in the user directory (`~/.har/auth.json`, or under `$HAR_HOME`; `HARNESS_NO_USER_DIR=1` disables reading it):

```json
{"providers": {"opencode": {"api_key": "sk-..."}, "anthropic": {"api_key": "sk-ant-..."}}}
```

```bash
harness auth login opencode            # prompts without echo; or: echo "$KEY" | harness auth login opencode -key-stdin
harness auth list                      # providers with a key, last 4 characters, and the path
harness auth remove opencode
```

The file must not be readable by the group or others (mode `0600`). A file that is refuses to be used: `<file> is readable by other users (mode 0644): refusing to use it; run: chmod 600 <file>`. The CLI creates it `0600` in a `0700` directory and rewrites it atomically.

The `harness` command stops at startup when the provider has no key and the base URL is not `localhost`, `127.0.0.1` or `::1`: `no API key for provider NAME: run `harness auth login NAME`, or export LLM_API_KEY`, followed by the hint for the type. Local endpoints (Ollama, llama.cpp) need none. As a library, `config.Load` does not check; call `cfg.RequireLLMKey()` if you want the same message.

## Providers that route by session

`LLM_SESSION_HEADER` names a request header that carries the thread id on every model call (steps, compaction and judges included). Gateways that route a conversation to one backend by session then keep that backend's prompt cache warm. OpenCode Go requires it:

```bash
LLM_BASE_URL=https://opencode.ai/zen/go/v1 LLM_API_KEY=... \
LLM_SESSION_HEADER=x-opencode-session LLM_MODELS=deepseek-v4.1-flash LLM_DEFAULT_MODEL=deepseek-v4.1-flash
```

In your own code, `llm.WithSession(ctx, id)` sets the id for the calls made under that context; without a configured header it is ignored.

## Prompt caching

| Setting | Default | Effect |
|---|---|---|
| `LLM_CACHE_TTL` | `5m` | Anthropic prompt-cache lifetime: `5m` or `1h`. |
| `LLM_PROMPT_CACHE_KEY` | `true` | The `openai` provider sends `prompt_cache_key` (the thread id). Turn it off for endpoints that reject unknown fields. DeepSeek model ids never receive it. |

## Reasoning, vision and context windows

These per-model tables default to the GPT-5.6 models. A deployment with other models must list them, or they get the plain defaults (no reasoning effort, no images, a 128,000-token window).

| Variable | Default | Meaning |
|---|---|---|
| `LLM_REASONING_MODELS` | `gpt-5.6-sol,gpt-5.6-terra` | Models that accept a reasoning effort. |
| `LLM_REASONING_EFFORTS` | `none\|low\|medium\|high\|xhigh` for both GPT-5.6 models | `model=effort\|effort,...`. A reasoning model not listed accepts `minimal`, `low`, `medium`, `high`. A requested effort the model lacks is mapped to the nearest accepted one. |
| `LLM_VISION_MODELS` | `gpt-5.6-sol,gpt-5.6-terra` | Models that receive attached images. |
| `LLM_CONTEXT_WINDOWS` | `gpt-5.6-sol=272000,gpt-5.6-terra=272000` | `model=tokens` (at least 1000). Models not listed get 128000; DeepSeek ids get 1,000,000. |
| `LLM_MAX_OUTPUT_TOKENS` | unset | `model=tokens`: output reserved below the window for compaction (16000 by default, plus a 20,000 margin). Not sent to the provider. |
| `LLM_MODEL_LABELS` | the GPT-5.6 labels | `model=Label` names shown in model pickers. |
| `LLM_PROMPT_VARIANTS` | `gpt-5.6` for both | `model=variant`; unlisted models use `deepseek` when the name contains it, else `default`. |

DeepSeek ids (`deepseek/...` or `DeepSeek-...`) in `LLM_MODELS` get a 1M window, join the reasoning models and have their reasoning content passed back, unless you list them yourself (`LLM_REASONING_PASSBACK_MODELS`).

### Presets

Presets are the stops of the composer's "thinking" slider: `LLM_PRESETS=id=model:effort,...` (the effort is optional) and `LLM_DEFAULT_PRESET` (default `balanced`). The default list is `fast=gpt-5.6-terra:low,balanced=gpt-5.6-terra:medium,thinking=gpt-5.6-sol:medium,deep=gpt-5.6-sol:high`.

```bash
LLM_MODELS=claude-sonnet-5-5,claude-opus-5 LLM_REASONING_MODELS=claude-sonnet-5-5,claude-opus-5 \
LLM_PRESETS="fast=claude-sonnet-5-5:low,deep=claude-opus-5:high" LLM_DEFAULT_PRESET=fast
```

The built-in default is dropped (no slider; the reason is logged) when `LLM_MODELS` or `LLM_REASONING_MODELS` lack its models, so other deployments still start. An explicit `LLM_PRESETS` is validated strictly: every model must be in `LLM_MODELS`, an effort needs a reasoning model that accepts it, and the default preset must exist. `LLM_PRESETS=none` removes the slider and the UI shows separate model and reasoning menus.

## Timeouts and retries

| Variable | Default | Bounds |
|---|---|---|
| `LLM_TIMEOUT_SECONDS` | `300` | Connecting and the response headers (transport level); also the wait for the first chunk of a stream. Doubled at `high` reasoning effort and tripled at `xhigh`, for reasoning models. |
| `LLM_STREAM_IDLE_SECONDS` | `90` | The gap between two chunks of a stream. |
| `LLM_CALL_MAX_SECONDS` | `1200` | One whole model call, whatever the stream sends (scaled by effort like the first-chunk wait). |
| `LLM_STEP_RETRIES` | `3` | How often a failed call is re-issued with the same request. `0` disables retries. |

Only timeouts, `429`, `5xx` and connection resets are retried (`llm.Classify` sorts errors into `transient`, `rate_limited`, `timeout`, `overflow`, `fatal`). The delay starts at 1 s, doubles up to 30 s with jitter, and never falls below the provider's `Retry-After` (capped at 120 s). A context overflow is not retried: the run compacts instead, see [Compaction](/docs/concepts/compaction/). Each retry emits a `work.retry` custom event.

The same settings exist as `config.yml` keys; see [config.yml keys](/docs/reference/config-yml/) and [Environment variables](/docs/reference/env-vars/).

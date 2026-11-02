package agui

// Harness v3 §1 (loop & context): RUN_ERROR codes and CUSTOM names.
const (
	// CodeModelTimeout: the model call (connect, first chunk, or a gap of
	// LLM_STREAM_IDLE_SECONDS between chunks) exceeded its budget after retries.
	CodeModelTimeout = "model_timeout"
	// CodeEmptyAnswer: no text and no tool call twice in a row (after one nudge); refusals count as empty.
	CodeEmptyAnswer = "empty_answer"
	// CodeContentFilter: the provider stopped the answer (finish_reason=content_filter).
	CodeContentFilter = "content_filter"
	// CodeRateLimited: 429 retries exhausted.
	CodeRateLimited = "rate_limited"

	// CustomRetry precedes a new attempt of a model call: {attempt, reason}.
	CustomRetry = "work.retry"
)

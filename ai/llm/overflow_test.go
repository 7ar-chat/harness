package llm_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/7ar-chat/harness/ai/openai"

	"github.com/7ar-chat/harness/ai/llm"
)

func TestContextOverflowErrorsAreRecognisedAndNotRetried(t *testing.T) {
	// Provider "context too long" errors are context overflows, which are never transient even with a retryable status.
	// Arrange
	overflows := []error{
		&openai.APIError{Status: 400, Code: "context_length_exceeded", Message: "This model's maximum context length is 128000 tokens."},
		errors.New("error, status code: 400, message: This model's maximum context length is 8192 tokens"),
		errors.New("prompt is too long: 210000 tokens > 200000 maximum"),
		errors.New("Too many tokens in the request"),
		errors.New("input exceeds the context window of this model"),
		errors.New("Input is too long for requested model."),
		fmt.Errorf("model call: %w", errors.New("context_length_exceeded")),
	}
	others := []error{errors.New("status code: 429, rate limited"), errors.New("invalid api key"), nil}

	// Act
	var gotOverflow, gotTransient, gotOther []bool
	for _, err := range overflows {
		gotOverflow = append(gotOverflow, llm.IsContextOverflow(err))
		gotTransient = append(gotTransient, llm.IsTransient(err))
	}
	for _, err := range others {
		gotOther = append(gotOther, llm.IsContextOverflow(err))
	}

	// Assert
	for i := range overflows {
		if !gotOverflow[i] || gotTransient[i] {
			t.Errorf("%v: overflow %v transient %v", overflows[i], gotOverflow[i], gotTransient[i])
		}
	}
	for i := range others {
		if gotOther[i] {
			t.Errorf("%v reported as overflow", others[i])
		}
	}
}

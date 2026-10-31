package llm_test

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/7ar-chat/harness/ai/llm"
)

// Classify turns a provider error into a decision. Only transient, rate-limited and timed-out calls are worth
// retrying as they are; an overflow needs a smaller request; anything else will not go away.
func ExampleClassify() {
	for _, err := range []error{
		errors.New("status code: 429, rate limit exceeded"),
		errors.New("status code: 503, service unavailable"),
		errors.New("this model's maximum context length is 128000 tokens"),
		errors.New("status code: 401, invalid api key"),
	} {
		class := llm.Classify(err)
		fmt.Printf("%-12s retry=%v\n", class, class.Retryable())
	}
	// Output:
	// rate_limited retry=true
	// transient    retry=true
	// overflow     retry=false
	// fatal        retry=false
}

// Backoff is the wait before retry n: base·2^(n-1) with jitter, never less than the provider's own Retry-After hint.
func ExampleBackoff() {
	hint := llm.ParseRetryAfter(http.Header{"Retry-After": {"7"}}, time.Now())

	fmt.Println(llm.Backoff(1, time.Second, 0, 0.5)) // jitter 0.5 -> factor 1.0
	fmt.Println(llm.Backoff(3, time.Second, 0, 0.5))
	fmt.Println(llm.Backoff(1, time.Second, hint, 0.5)) // the hint wins
	// Output:
	// 1s
	// 4s
	// 7s
}

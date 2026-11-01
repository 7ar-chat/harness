package review

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/7ar-chat/harness/core/factory"
)

// commentMarker opens every comment PostVerdict writes; the reviewer skips
// those when it reads a pull request's earlier comments.
const commentMarker = "<!-- harness-review -->"

// FormatVerdict is the body PostVerdict posts.
func FormatVerdict(v factory.Verdict) string {
	var b strings.Builder
	b.WriteString(commentMarker + "\n")
	switch {
	case v.Approve:
		b.WriteString("### Automated review: approved\n\n")
	case v.Hard:
		b.WriteString("### Automated review: blocked, needs a human\n\n")
	default:
		b.WriteString("### Automated review: changes requested\n\n")
	}
	if s := strings.TrimSpace(v.Summary); s != "" {
		b.WriteString(s + "\n")
	}
	if len(v.Findings) > 0 {
		b.WriteString("\n**Findings**\n")
		for _, f := range v.Findings {
			fmt.Fprintf(&b, "- %s\n", strings.Join(strings.Fields(f), " "))
		}
	}
	return b.String()
}

// PostVerdict comments the verdict on the pull request.
func PostVerdict(ctx context.Context, forge factory.Forge, repo string, number int, v factory.Verdict) error {
	if forge == nil {
		return errors.New("review: no forge to post the verdict to")
	}
	if err := forge.Comment(ctx, repo, number, FormatVerdict(v)); err != nil {
		return fmt.Errorf("review: posting the verdict on %s#%d: %w", repo, number, err)
	}
	return nil
}

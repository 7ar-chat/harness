package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/7ar-chat/harness/core/roles"
)

// applyRole folds the role of a run (RunOptions.Role or RoleRef) into its options and records the run's
// reference to it among its requested skills, which is how the prompt section and the tool filter of
// features_roles.go find it when the run executes, possibly in another process.
func (h *Harness) applyRole(ctx context.Context, user, threadID string, o *RunOptions) error {
	// A role reference is never taken from the requested skills: it is only what this function records.
	if len(o.Skills) > 0 {
		o.Skills = roles.Without(o.Skills)
	}
	name := strings.ToLower(strings.TrimSpace(o.Role))
	if name == "" && o.RoleRef == "" {
		return nil
	}
	if h.roles == nil {
		return fmt.Errorf("%w: this harness has no roles", ErrInvalidRequest)
	}
	var r *roles.Role
	var err error
	explicit := name != ""
	if explicit {
		asker := user
		if o.Author != "" {
			asker = o.Author
		}
		r, err = h.roles.Resolve(ctx, roles.Query{Login: asker, ThreadID: threadID}, name)
	} else {
		ref, ok := roles.ParseRef(o.RoleRef)
		if !ok {
			return fmt.Errorf("%w: malformed role reference", ErrInvalidRequest)
		}
		r, err = h.roles.Get(ctx, ref)
	}
	switch {
	case errors.Is(err, roles.ErrNotFound):
		return fmt.Errorf("%w: unknown role %q", ErrInvalidRequest, strings.TrimPrefix(o.Role+o.RoleRef, "role:"))
	case err != nil:
		return fmt.Errorf("resolving the role: %w", err)
	}
	if r.Model != "" && (explicit || o.Model == "") {
		o.Model = r.Model
	}
	if o.ReasoningEffort == "" {
		o.ReasoningEffort = r.ReasoningEffort
	}
	if r.ReadOnly() {
		o.Mode = ModePlan
	}
	if r.PermissionMode == roles.PermAsk {
		o.Interactive = true
	}
	skills := o.Skills
	for _, s := range r.Skills {
		if !containsString(skills, s) {
			skills = append(skills, s)
		}
	}
	o.Skills = append(skills, roles.RefOf(*r).String())
	o.Role, o.RoleRef = "", ""
	return nil
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

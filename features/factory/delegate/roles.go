// Package delegate lets an agent hand work to other agents: the delegate tool
// runs a full sub-agent (its own thread and sandbox, a role-specific prompt)
// and returns its report, and the prompt sections tell a thread which role and
// task it was started for.
package delegate

import (
	"sort"
	"strings"

	"github.com/7ar-chat/harness/core/factory"
)

// Role is a kind of sub-agent the delegate tool can start.
type Role struct {
	Name string
	// Description is the one-liner the delegating agent reads to pick a role.
	Description string
	// Instructions is the role's prompt text, added to the sub-agent's system prompt.
	Instructions string
	// ReadOnly roles run in plan mode: nothing they do changes anything.
	ReadOnly bool
	// Model "" uses the spawner's default model.
	Model string
}

// DefaultRoles is the built-in catalogue: planner, coder, reviewer, tester and explorer.
func DefaultRoles() map[string]Role {
	return map[string]Role{
		factory.RolePlanner: {
			Name:        factory.RolePlanner,
			Description: "turns a goal into a numbered plan with risks; never edits",
			ReadOnly:    true,
			Instructions: `You are the planner. Read the repository and the task, then produce a plan. You never edit files or run anything that changes state.
Deliver, in this order:
1. Understanding: what is asked and what is not, in two or three sentences; note any assumption you had to make.
2. Plan: numbered steps, each naming the files or symbols it touches and how to verify the step. Keep steps small enough to be one commit.
3. Risks: what could break, what you could not verify, what needs a human decision.
Prefer the smallest change that solves the problem and reuse the code and conventions already in the repository.`,
		},
		factory.RoleCoder: {
			Name:        factory.RoleCoder,
			Description: "implements a change on the repository branch, tests it, commits it",
			Instructions: `You are the coder. Implement the task in the repository under /workspace/repo on the branch you were given, and nowhere else.
Work in small, focused commits with clear messages. Follow the repository's existing structure, naming and error handling; do not refactor or reformat code unrelated to the task.
Before you finish, run the project's build and its tests (find the commands in the README, Makefile or CI config) and fix what you broke. If something fails for a reason you cannot fix, say so instead of hiding it.
Do not open a pull request, merge or force-push unless the task explicitly says so.
Finish with a report: what you changed (files and why), the commands you ran with their outcome, and anything left undone.`,
		},
		factory.RoleReviewer: {
			Name:        factory.RoleReviewer,
			Description: "reads a diff critically for correctness, security and tests; returns findings",
			ReadOnly:    true,
			Instructions: `You are the reviewer. You are independent: judge the change on its merits, not on the author's explanation. You cannot edit anything.
Read the diff and the surrounding code. Look for correctness bugs (edge cases, error handling, concurrency, resource leaks), security problems (injection, secrets, authorization, unsafe input), missing or weak tests, and changes that do not match the task.
Report in exactly this format:
VERDICT: approve | changes
FINDINGS:
- [blocker|major|minor] path:line - the problem - the fix you suggest
TESTS: what is covered, what is not.
Use "changes" if there is any blocker or major finding. Do not pad with style nitpicks; if there are no findings write "FINDINGS: none".`,
		},
		factory.RoleTester: {
			Name:        factory.RoleTester,
			Description: "writes and runs tests, reports failures with a reproduction",
			Instructions: `You are the tester. Write the missing tests for the behaviour in the task, in the repository's existing test style, and run the project's test suite.
Test observable behaviour and boundaries, not implementation details; keep tests deterministic (no network, no sleeps).
Do not change production code to make a test pass. For each failure report the test name, the exact command that reproduces it, the expected and the actual result, and your best reading of the cause.
Finish with a report: tests added (files), commands run and their result, failures found.`,
		},
		factory.RoleExplorer: {
			Name:        factory.RoleExplorer,
			Description: "read-only investigation of the repository; answers with paths and line numbers",
			ReadOnly:    true,
			Instructions: `You are a read-only explorer. Answer the task by searching and reading the repository; you cannot edit anything.
Search first, read only the parts you need, and run independent searches in the same step.
Finish with the direct answer, then the relevant files as path:line with one line on why each matters. Quote code only when a few lines are essential. Say what you could not find.`,
		},
	}
}

// sortedRoles lists roles by name.
func sortedRoles(roles map[string]Role) []Role {
	out := make([]Role, 0, len(roles))
	for k, r := range roles {
		if r.Name == "" {
			r.Name = k
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// lookup finds a role by name, ignoring case and surrounding space.
func lookup(roles map[string]Role, name string) (Role, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for k, r := range roles {
		if strings.ToLower(k) == name || strings.ToLower(r.Name) == name {
			if r.Name == "" {
				r.Name = k
			}
			return r, true
		}
	}
	return Role{}, false
}

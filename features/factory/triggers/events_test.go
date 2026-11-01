package triggers_test

import (
	"reflect"
	"testing"

	"github.com/7ar-chat/harness/features/factory/triggers"
)

func TestNormalizeGitHubEvents(t *testing.T) {
	tests := []struct {
		name, event, body string
		wantName          string
		wantKey, wantURL  string
		wantFields        map[string]string
	}{
		{
			name: "issues.labeled", event: "issues", body: issueLabeled,
			wantName: "issues.labeled", wantKey: "github:issue:acme/app#12", wantURL: "https://github.com/acme/app/issues/12",
			wantFields: map[string]string{
				"repo": "acme/app", "number": "12", "title": "Login returns 500", "body": "Steps:\n1. open /login",
				"label": "agent", "labels": "bug,agent", "sender": "carol", "action": "labeled",
			},
		},
		{
			name: "issues.opened", event: "issues",
			body: `{"action":"opened","issue":{"number":3,"title":"Add dark mode","body":"please","html_url":"https://github.com/acme/app/issues/3","labels":[]},
			        "repository":{"full_name":"acme/app"},"sender":{"login":"bob"}}`,
			wantName: "issues.opened", wantKey: "github:issue:acme/app#3", wantURL: "https://github.com/acme/app/issues/3",
			wantFields: map[string]string{"repo": "acme/app", "number": "3", "title": "Add dark mode", "body": "please", "sender": "bob", "action": "opened"},
		},
		{
			name: "issue_comment.created on an issue", event: "issue_comment",
			body: `{"action":"created","issue":{"number":12,"title":"Login returns 500","html_url":"https://github.com/acme/app/issues/12","labels":[{"name":"bug"}]},
			        "comment":{"body":"@factory please retry","html_url":"https://github.com/acme/app/issues/12#issuecomment-77"},
			        "repository":{"full_name":"acme/app"},"sender":{"login":"carol"}}`,
			wantName: "issue_comment.created", wantKey: "github:issue:acme/app#12", wantURL: "https://github.com/acme/app/issues/12#issuecomment-77",
			wantFields: map[string]string{
				"repo": "acme/app", "number": "12", "title": "Login returns 500", "labels": "bug",
				"comment": "@factory please retry", "sender": "carol", "action": "created",
			},
		},
		{
			name: "issue_comment.created on a pull request", event: "issue_comment",
			body: `{"action":"created","issue":{"number":7,"title":"Fix login","html_url":"https://github.com/acme/app/pull/7","labels":[],"pull_request":{"url":"https://api.github.com/repos/acme/app/pulls/7"}},
			        "comment":{"body":"LGTM"},"repository":{"full_name":"acme/app"},"sender":{"login":"carol"}}`,
			wantName: "issue_comment.created", wantKey: "github:issue:acme/app#7", wantURL: "https://github.com/acme/app/pull/7",
			wantFields: map[string]string{
				"repo": "acme/app", "number": "7", "title": "Fix login", "is_pr": "true", "comment": "LGTM", "sender": "carol", "action": "created",
			},
		},
		{
			name: "pull_request.closed merged", event: "pull_request",
			body: `{"action":"closed","number":7,
			        "pull_request":{"number":7,"title":"Fix login","body":"Closes #12","html_url":"https://github.com/acme/app/pull/7","merged":true,
			          "labels":[{"name":"agent"}],"head":{"ref":"factory/1a2b3c4d","sha":"abc123"},"base":{"ref":"main"}},
			        "repository":{"full_name":"acme/app"},"sender":{"login":"carol"}}`,
			wantName: "pull_request.closed", wantKey: "github:pr:acme/app#7", wantURL: "https://github.com/acme/app/pull/7",
			wantFields: map[string]string{
				"repo": "acme/app", "number": "7", "title": "Fix login", "body": "Closes #12", "labels": "agent",
				"branch": "factory/1a2b3c4d", "sha": "abc123", "base": "main", "merged": "true", "sender": "carol", "action": "closed",
			},
		},
		{
			name: "pull_request.closed not merged", event: "pull_request",
			body: `{"action":"closed","pull_request":{"number":8,"title":"Try","html_url":"https://github.com/acme/app/pull/8","merged":false,
			          "head":{"ref":"x","sha":"d00d"},"base":{"ref":"main"}},"repository":{"full_name":"acme/app"},"sender":{"login":"carol"}}`,
			wantName: "pull_request.closed", wantKey: "github:pr:acme/app#8", wantURL: "https://github.com/acme/app/pull/8",
			wantFields: map[string]string{
				"repo": "acme/app", "number": "8", "title": "Try", "branch": "x", "sha": "d00d", "base": "main", "merged": "false", "sender": "carol", "action": "closed",
			},
		},
		{
			name: "pull_request_review.submitted", event: "pull_request_review",
			body: `{"action":"submitted",
			        "review":{"state":"CHANGES_REQUESTED","body":"rename x","html_url":"https://github.com/acme/app/pull/7#pullrequestreview-5"},
			        "pull_request":{"number":7,"title":"Fix login","html_url":"https://github.com/acme/app/pull/7","head":{"ref":"factory/1a2b3c4d","sha":"abc123"},"base":{"ref":"main"}},
			        "repository":{"full_name":"acme/app"},"sender":{"login":"erin"}}`,
			wantName: "pull_request_review.submitted", wantKey: "github:pr:acme/app#7", wantURL: "https://github.com/acme/app/pull/7#pullrequestreview-5",
			wantFields: map[string]string{
				"repo": "acme/app", "number": "7", "title": "Fix login", "branch": "factory/1a2b3c4d", "sha": "abc123", "base": "main",
				"state": "changes_requested", "comment": "rename x", "sender": "erin", "action": "submitted",
			},
		},
		{
			name: "check_suite.completed", event: "check_suite",
			body: `{"action":"completed",
			        "check_suite":{"id":1,"head_branch":"factory/1a2b3c4d","head_sha":"abc123","conclusion":"failure",
			          "pull_requests":[{"number":7,"url":"https://api.github.com/repos/acme/app/pulls/7"}]},
			        "repository":{"full_name":"acme/app"},"sender":{"login":"github-actions[bot]"}}`,
			wantName: "check_suite.completed", wantKey: "github:pr:acme/app#7", wantURL: "",
			wantFields: map[string]string{
				"repo": "acme/app", "branch": "factory/1a2b3c4d", "sha": "abc123", "conclusion": "failure", "number": "7",
				"sender": "github-actions[bot]", "action": "completed",
			},
		},
		{
			name: "check_suite.completed without a pull request", event: "check_suite",
			body: `{"action":"completed","check_suite":{"head_branch":"main","head_sha":"def456","conclusion":"success","pull_requests":[]},
			        "repository":{"full_name":"acme/app"},"sender":{"login":"x"}}`,
			wantName: "check_suite.completed", wantKey: "github:sha:acme/app@def456", wantURL: "",
			wantFields: map[string]string{"repo": "acme/app", "branch": "main", "sha": "def456", "conclusion": "success", "sender": "x", "action": "completed"},
		},
		{
			name: "check_run.completed", event: "check_run",
			body: `{"action":"completed",
			        "check_run":{"name":"lint","head_sha":"abc123","conclusion":"success","html_url":"https://github.com/acme/app/runs/9",
			          "check_suite":{"head_branch":"factory/1a2b3c4d"},"pull_requests":[{"number":7}]},
			        "repository":{"full_name":"acme/app"},"sender":{"login":"github-actions[bot]"}}`,
			wantName: "check_run.completed", wantKey: "github:pr:acme/app#7", wantURL: "https://github.com/acme/app/runs/9",
			wantFields: map[string]string{
				"repo": "acme/app", "check": "lint", "branch": "factory/1a2b3c4d", "sha": "abc123", "conclusion": "success", "number": "7",
				"sender": "github-actions[bot]", "action": "completed",
			},
		},
		{
			name: "workflow_run.completed", event: "workflow_run",
			body: `{"action":"completed",
			        "workflow_run":{"name":"CI","head_branch":"factory/1a2b3c4d","head_sha":"abc123","conclusion":"cancelled","html_url":"https://github.com/acme/app/actions/runs/55","pull_requests":[]},
			        "repository":{"full_name":"acme/app"},"sender":{"login":"carol"}}`,
			wantName: "workflow_run.completed", wantKey: "github:sha:acme/app@abc123", wantURL: "https://github.com/acme/app/actions/runs/55",
			wantFields: map[string]string{
				"repo": "acme/app", "check": "CI", "branch": "factory/1a2b3c4d", "sha": "abc123", "conclusion": "cancelled", "sender": "carol", "action": "completed",
			},
		},
		{
			name: "ping", event: "ping",
			body:     `{"zen":"Design for failure.","hook_id":1,"repository":{"full_name":"acme/app"},"sender":{"login":"carol"}}`,
			wantName: "ping", wantKey: "", wantURL: "",
			wantFields: map[string]string{"repo": "acme/app", "zen": "Design for failure.", "sender": "carol"},
		},
		{
			name: "unlisted event is kept by name", event: "push",
			body:     `{"ref":"refs/heads/main","repository":{"full_name":"acme/app"},"sender":{"login":"carol"}}`,
			wantName: "push", wantKey: "", wantURL: "",
			wantFields: map[string]string{"repo": "acme/app", "sender": "carol"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Act
			ev, err := triggers.Normalize(tc.event, []byte(tc.body))

			// Assert
			if err != nil {
				t.Fatal(err)
			}
			if ev.Name != tc.wantName || ev.Key != tc.wantKey || ev.URL != tc.wantURL || ev.Repo != tc.wantFields["repo"] {
				t.Errorf("event = name %q key %q url %q repo %q; want %q %q %q %q", ev.Name, ev.Key, ev.URL, ev.Repo, tc.wantName, tc.wantKey, tc.wantURL, tc.wantFields["repo"])
			}
			if !reflect.DeepEqual(ev.Fields, tc.wantFields) {
				t.Errorf("fields = %#v\nwant     %#v", ev.Fields, tc.wantFields)
			}
		})
	}
}

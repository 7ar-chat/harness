package recipes

import "time"

// builtinEpoch dates the builtin gallery; bump a recipe's Version when its text changes.
var builtinEpoch = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

// Builtins returns the gallery that ships with the product: plain recipes with a read-only or sandboxed policy and a
// budget cap. Work that only reads and reports is a schedule (read-only); work that changes code is a trigger that
// creates a task (sandboxed, with a spend cap and a pull request for a person to review).
func Builtins() []Recipe {
	list := []Recipe{
		dependencyScan(), dependencyUpdatePR(), ciTriage(), changelog(), staleWork(), docsDrift(), flakyTests(),
		statusDigest(), securityReview(), issueTriage(), dependabotReviewer(), explainCodebase(), writeTests(),
	}
	for i := range list {
		r := &list[i]
		r.Scope, r.Author, r.Version = ScopeBuiltin, "7ar", 1
		r.CreatedAt, r.UpdatedAt = builtinEpoch, builtinEpoch
		if r.Params == nil {
			r.Params = []Param{}
		}
	}
	return list
}

// BuiltinByID returns the builtin recipe id.
func BuiltinByID(id string) (Recipe, bool) {
	for _, r := range Builtins() {
		if r.ID == id {
			return r, true
		}
	}
	return Recipe{}, false
}

func when(def, help string) Param {
	return Param{Key: "when", Label: "When to run", Help: help, Type: TypeCron, Default: def, Required: true}
}

func zone() Param {
	return Param{Key: "timezone", Label: "Time zone", Help: "The clock the time above is read in, for example Europe/Paris. UTC if unsure.", Type: TypeText, Default: "UTC", Required: true}
}

func branchParam() Param {
	return Param{Key: "branch", Label: "Main branch", Help: "The branch the work is compared against or sent to.", Type: TypeText, Default: "main", Required: true}
}

const readOnlyNote = "\n\nYou are running unattended and read-only: do not change any file, branch or pull request. Finish with a short report in plain language: what you checked, what you found (most important first), and what you suggest doing next. If everything is fine, say so in one line."

func dependencyScan() Recipe {
	return Recipe{ID: "nightly-dependency-scan", Name: "Nightly dependency and vulnerability scan", Category: CatSecurity, Kind: KindSchedule,
		Summary: "Every night, look for outdated or vulnerable dependencies and report only what needs action.",
		Body: Body{Name: "Nightly dependency scan", Cron: "{{when}}", Timezone: "{{timezone}}",
			Prompt: "Find the dependency manifests of this repository (go.mod, package.json and lockfiles, requirements.txt, pyproject.toml, Cargo.toml, Gemfile and similar). " +
				"Check them for known vulnerabilities using the audit tool of each ecosystem when it is installed (for example `npm audit`, `govulncheck ./...`, `pip-audit`, `cargo audit`); " +
				"if a tool is missing, read the manifests and say which versions look old or risky. Ignore dev-only issues unless they are critical. " +
				"Report severity {{severity}} and above. For each finding give the package, the installed version, the fixed version if there is one, and whether it is reachable from our code if you can tell." + readOnlyNote},
		Params: []Param{
			when("0 2 * * *", "Every night at 02:00 by default."), zone(),
			{Key: "severity", Label: "Smallest severity to report", Help: "Findings below this level are left out.", Type: TypeChoice, Choices: []string{"low", "moderate", "high", "critical"}, Default: "moderate", Required: true},
		},
		Suggest: Suggest{Policy: "readonly", MaxUSD: 1, TimeoutMinutes: 30}}
}

func dependencyUpdatePR() Recipe {
	return Recipe{ID: "weekly-dependency-update", Name: "Weekly dependency update pull request", Category: CatMaintenance, Kind: KindTrigger,
		Summary: "Once a week, update safe dependency upgrades, run the tests and open one pull request for a person to review.",
		Body: Body{Name: "Weekly dependency update", Cron: "{{when}}", Title: "Dependency update {{.Date}}",
			Prompt: "Update this repository's dependencies, keeping to {{scope}} upgrades. Steps: 1) list what is outdated with the ecosystem's own tool; " +
				"2) apply the upgrades in one go, and drop any single upgrade that breaks the build; 3) run the project's tests and linters and fix only what the upgrade broke; " +
				"4) commit on a new branch and open one pull request against {{branch}} with a table of old and new versions and a note on anything risky (major bumps, changelog warnings). " +
				"Do not touch application code beyond what the upgrades require. If the tests fail and you cannot fix them quickly, open no pull request and explain why."},
		Params: []Param{
			when("0 6 * * 1", "Mondays at 06:00 by default."),
			{Key: "scope", Label: "Which upgrades", Help: "Patch and minor upgrades are the safest start.", Type: TypeChoice, Choices: []string{"patch", "patch and minor", "all (including major)"}, Default: "patch and minor", Required: true},
			branchParam(),
		},
		Suggest: Suggest{Policy: "sandboxed", MaxUSD: 3, BaseBranch: ""}}
}

func ciTriage() Recipe {
	return Recipe{ID: "daily-ci-triage", Name: "Daily CI failure triage", Category: CatQuality, Kind: KindSchedule,
		Summary: "Each weekday morning, look at failed builds from the last day and explain the likely cause of each.",
		Body: Body{Name: "Daily CI triage", Cron: "{{when}}", Timezone: "{{timezone}}",
			Prompt: "Look at the continuous integration runs of this repository from the last 24 hours (use the GitHub CLI `gh run list` and `gh run view --log-failed` if it is available, or the CI configuration and logs you can reach). " +
				"Group the failures by cause. For each group say: which workflow and branch, the first error line, the most likely cause, whether it looks like a real bug, a flaky test or an infrastructure problem, and who or which change probably triggered it. " +
				"Start with failures on {{branch}}, since those block everyone." + readOnlyNote},
		Params:  []Param{when("0 8 * * 1-5", "Weekdays at 08:00 by default."), zone(), branchParam()},
		Suggest: Suggest{Policy: "readonly", MaxUSD: 1, TimeoutMinutes: 30}}
}

func changelog() Recipe {
	return Recipe{ID: "weekly-changelog", Name: "Weekly changelog from merged pull requests", Category: CatDocs, Kind: KindSchedule,
		Summary: "Every Friday, turn the week's merged pull requests into readable release notes.",
		Body: Body{Name: "Weekly changelog", Cron: "{{when}}", Timezone: "{{timezone}}",
			Prompt: "List the pull requests merged into {{branch}} in the last 7 days (`gh pr list --state merged --search \"merged:>=$(date -v-7d +%F)\"`, or use git log of merge commits). " +
				"Write a changelog for {{audience}}: group the changes under New, Improved and Fixed, one plain sentence each, with the pull request number. " +
				"Leave out dependency bumps and internal refactors unless they matter to readers. Put the text in your final answer ready to paste." + readOnlyNote},
		Params: []Param{when("0 16 * * 5", "Fridays at 16:00 by default."), zone(), branchParam(),
			{Key: "audience", Label: "Who reads it", Help: "Sets the tone and the level of detail.", Type: TypeChoice, Choices: []string{"users of the product", "the engineering team"}, Default: "the engineering team", Required: true}},
		Suggest: Suggest{Policy: "readonly", MaxUSD: 1, TimeoutMinutes: 20}}
}

func staleWork() Recipe {
	return Recipe{ID: "stale-branches-and-prs", Name: "Stale branches and pull request nag", Category: CatMaintenance, Kind: KindSchedule,
		Summary: "Every Monday, list pull requests waiting on someone and branches nobody touched for weeks.",
		Body: Body{Name: "Stale work nag", Cron: "{{when}}", Timezone: "{{timezone}}",
			Prompt: "Find open pull requests with no activity for {{days}} days or more, and remote branches with no commit for {{days}} days that are not {{branch}} (use `gh pr list` and `git for-each-ref`). " +
				"For each pull request say who it is waiting on (author or reviewer), what is blocking it (failing checks, requested changes, no reviewer) and the single next step. " +
				"For each branch say who last committed and whether it is already merged and safe to delete. Keep it short: a table or a list, oldest first." + readOnlyNote},
		Params: []Param{when("0 9 * * 1", "Mondays at 09:00 by default."), zone(),
			{Key: "days", Label: "Stale after (days)", Help: "How long without activity counts as stale.", Type: TypeNumber, Default: "14", Required: true}, branchParam()},
		Suggest: Suggest{Policy: "readonly", MaxUSD: 0.5, TimeoutMinutes: 20}}
}

func docsDrift() Recipe {
	return Recipe{ID: "docs-drift-check", Name: "Docs drift check", Category: CatDocs, Kind: KindSchedule,
		Summary: "Once a week, compare the README and docs with what the code really does and list what is out of date.",
		Body: Body{Name: "Docs drift check", Cron: "{{when}}", Timezone: "{{timezone}}",
			Prompt: "Read the README and the documents under {{docs_path}}, then check their claims against the code: commands and flags, configuration and environment variables, file paths, API routes and examples. " +
				"List every place where the documentation is wrong, missing or out of date, with the file and line, what the docs say, what the code does, and the suggested new wording. " +
				"Most important first; skip style nitpicks." + readOnlyNote},
		Params: []Param{when("0 10 * * 3", "Wednesdays at 10:00 by default."), zone(),
			{Key: "docs_path", Label: "Where the docs are", Help: "A folder or file, besides the README.", Type: TypeText, Default: "docs/", Required: true}},
		Suggest: Suggest{Policy: "readonly", MaxUSD: 1.5, TimeoutMinutes: 30}}
}

func flakyTests() Recipe {
	return Recipe{ID: "test-flakiness-hunt", Name: "Test flakiness hunt", Category: CatQuality, Kind: KindSchedule,
		Summary: "Every week, look through recent test runs for tests that pass and fail on the same code.",
		Body: Body{Name: "Test flakiness hunt", Cron: "{{when}}", Timezone: "{{timezone}}",
			Prompt: "Find tests of this repository that look flaky. Use the CI history of the last {{days}} days when you can reach it (`gh run list`, `gh run view --log`), and look for tests that failed on one run and passed on a re-run of the same commit. " +
				"If you cannot read the history, run the test suite {{runs}} times with the project's test command and compare. For each suspect give the test name, how often it failed, the most likely cause (timing, shared state, ordering, network, randomness) and a concrete fix. " +
				"Do not change any file." + readOnlyNote},
		Params: []Param{when("0 3 * * 6", "Saturdays at 03:00 by default."), zone(),
			{Key: "days", Label: "Look back (days)", Type: TypeNumber, Default: "7", Required: true},
			{Key: "runs", Label: "Local re-runs", Help: "Used only when the CI history is not available.", Type: TypeNumber, Default: "3", Required: true}},
		Suggest: Suggest{Policy: "readonly", MaxUSD: 2, TimeoutMinutes: 60}}
}

func statusDigest() Recipe {
	return Recipe{ID: "weekly-status-digest", Name: "Weekly project status digest", Category: CatReporting, Kind: KindSchedule,
		Summary: "Every Friday, a short status report: what shipped, what is stuck, and what is next.",
		Body: Body{Name: "Weekly status digest", Cron: "{{when}}", Timezone: "{{timezone}}",
			Prompt: "Write this week's status report for the project for {{audience}}. If the project has an insights or report page, start from its numbers (tasks done, spend, failures). " +
				"Otherwise summarise the pull requests merged into the main branch in the last 7 days and the issues and pull requests still open. " +
				"Use these headings: Shipped (what changed for users), In progress, Blocked or at risk (with what would unblock it), and Next week. Plain sentences, at most {{length}} lines, no jargon." + readOnlyNote},
		Params: []Param{when("0 17 * * 5", "Fridays at 17:00 by default."), zone(),
			{Key: "audience", Label: "Who reads it", Type: TypeChoice, Choices: []string{"the whole team", "managers and stakeholders"}, Default: "the whole team", Required: true},
			{Key: "length", Label: "Maximum length (lines)", Type: TypeNumber, Default: "20", Required: true}},
		Suggest: Suggest{Policy: "readonly", MaxUSD: 1, TimeoutMinutes: 20}}
}

func securityReview() Recipe {
	return Recipe{ID: "weekly-security-review", Name: "Security review of the last 7 days of commits", Category: CatSecurity, Kind: KindSchedule,
		Summary: "Every Monday, read last week's changes for security problems such as secrets, injection and weak access checks.",
		Body: Body{Name: "Weekly security review", Cron: "{{when}}", Timezone: "{{timezone}}",
			Prompt: "Review the commits that reached {{branch}} in the last 7 days (`git log --since=7.days -p`). Look for: secrets or tokens committed, missing authentication or permission checks, " +
				"SQL or command injection, unsafe file paths, unvalidated input, weakened security settings, new dependencies from unknown sources, and logging of sensitive data. " +
				"For each issue give the commit, the file and line, why it matters, and a concrete fix. Rank by severity. Do not report style or performance. If you find nothing, list what you looked at." + readOnlyNote},
		Params:  []Param{when("0 7 * * 1", "Mondays at 07:00 by default."), zone(), branchParam()},
		Suggest: Suggest{Policy: "readonly", MaxUSD: 2, TimeoutMinutes: 45}}
}

func issueTriage() Recipe {
	return Recipe{ID: "issue-triage", Name: "Issue triage: label new GitHub issues", Category: CatMaintenance, Kind: KindTrigger,
		Summary: "When someone opens a GitHub issue, read it, add labels and a short first reply, and say what is missing.",
		Body: Body{Name: "Issue triage", Event: "issues.opened", Title: "Triage: {{.title}}",
			Prompt: "A new issue was opened: {{.URL}}\n\nRead it with `gh issue view`. 1) Choose labels from the ones that exist in the repository (`gh label list`); prefer {{labels}}. " +
				"2) Decide whether it is a bug, a feature request, a question or a duplicate (search open issues). 3) Post one short, friendly comment: thank the author, say what you understood, and ask for anything missing (steps to reproduce, version, logs). " +
				"4) Apply the labels. Do not close the issue, do not promise a date, and do not change code."},
		Params:  []Param{{Key: "labels", Label: "Labels to prefer", Help: "Comma separated names of labels your repository uses.", Type: TypeText, Default: "bug, enhancement, question, documentation", Required: true}},
		Suggest: Suggest{Policy: "sandboxed", MaxUSD: 0.5}}
}

func dependabotReviewer() Recipe {
	return Recipe{ID: "dependabot-pr-reviewer", Name: "Dependabot pull request reviewer", Category: CatMaintenance, Kind: KindTrigger,
		Summary: "When Dependabot opens a pull request, read the changelog and tell the team whether it is safe to merge.",
		Body: Body{Name: "Dependabot reviewer", Event: "pull_request.opened", Match: map[string]string{"sender": "{{bot}}"}, Title: "Review dependency PR: {{.title}}",
			Prompt: "Dependabot opened a pull request: {{.URL}}\n\nRead the diff and the release notes or changelog of the upgraded package (follow the links in the pull request description). " +
				"Say: what changed, whether it is a patch, minor or major upgrade, any breaking changes or deprecations that touch how this repository uses the package, whether the checks passed, " +
				"and a clear verdict: safe to merge, merge after a change (say which), or needs a person. Post the verdict as one comment on the pull request. Do not merge it."},
		Params:  []Param{{Key: "bot", Label: "Bot account", Help: "The account whose pull requests to review.", Type: TypeText, Default: "dependabot[bot]", Required: true}},
		Suggest: Suggest{Policy: "sandboxed", MaxUSD: 0.5}}
}

func explainCodebase() Recipe {
	return Recipe{ID: "explain-this-codebase", Name: "Explain this codebase to a new team member", Category: CatOnboarding, Kind: KindTask,
		Summary: "A one-off guided tour of the repository: what it does, how it is organised, and where to start.",
		Body: Body{Title: "Explain this codebase to {{who}}",
			Prompt: "Write a guided tour of this repository for {{who}}, who knows {{background}} but has never seen this code. Read the README, the folder layout, the build and test setup and the main entry points. " +
				"Cover: what the product does in two sentences; the main parts and how they talk to each other (a simple diagram in text is welcome); how to build, run and test it locally; " +
				"the three files to read first and why; the conventions the code follows; and three small, safe first tasks. Use plain words and define any term you use. " +
				"Save the result as ONBOARDING.md in a new branch and open a pull request, or if you cannot, give it in your final answer. Do not change any other file."},
		Params: []Param{
			{Key: "who", Label: "Who is it for", Help: "A name or a role, for example 'the new backend engineer'.", Type: TypeText, Default: "a new team member", Required: true},
			{Key: "background", Label: "What they already know", Help: "So the tour does not explain the basics.", Type: TypeText, Default: "programming in general", Required: true}},
		Suggest: Suggest{Policy: "sandboxed", MaxUSD: 2}}
}

func writeTests() Recipe {
	return Recipe{ID: "write-missing-tests", Name: "Write tests for a file or module", Category: CatQuality, Kind: KindTask,
		Summary: "A one-off task: add focused tests for code that has none, and open a pull request.",
		Body: Body{Title: "Add tests for {{target}}",
			Prompt: "Add automated tests for {{target}}. First read the code and the existing tests to match their style and tools. Test the behaviour people rely on: the normal paths, the edge cases and the error cases; " +
				"do not test private details. Run the tests until they pass and make sure they fail when the code is broken on purpose. Do not change the code under test except to fix a clear bug, and if you do, say so. " +
				"Open one pull request with a short description of what is covered and what is not."},
		Params:  []Param{{Key: "target", Label: "What to test", Help: "A file, folder or function, for example internal/billing.", Type: TypeText, Required: true}},
		Suggest: Suggest{Policy: "sandboxed", MaxUSD: 2}}
}

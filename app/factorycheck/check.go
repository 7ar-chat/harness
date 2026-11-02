// Package factorycheck answers one question before the agent factory is trusted with a real repository: are the
// GitHub App, its webhook, the installation token, the repository and the surrounding configuration set up the
// way the factory needs? It only reads; nothing is written to the repository.
package factorycheck

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/7ar-chat/harness/app/product"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/features/factory/forge"
)

// Status is the outcome of one check.
type Status int

// A Fail means the factory cannot work as configured; a Warn that it works with a weaker guarantee; a Skip that
// the check could not run because something it depends on failed or was not given.
const (
	OK Status = iota
	Warn
	Fail
	Skip
)

// String is the tag Report.Write prints.
func (s Status) String() string {
	switch s {
	case OK:
		return " OK "
	case Warn:
		return "WARN"
	case Fail:
		return "FAIL"
	default:
		return "SKIP"
	}
}

// Check is one line of the report. Fix says what to change when the check did not pass.
type Check struct {
	Name   string
	Status Status
	Detail string
	Fix    string
}

// The names of the checks, in report order.
const (
	NameFactory      = "factory enabled"
	NameApp          = "github app"
	NameWebhook      = "webhook secret"
	NamePermissions  = "app permissions"
	NameEvents       = "app events"
	NameInstalled    = "app installed"
	NameToken        = "installation token"
	NameRepoAccess   = "repository access"
	NameBranch       = "default branch"
	NameReviewer     = "reviewer"
	NameSandbox      = "sandbox"
	NameSandboxQuota = "sandbox quota"
	NameBudgets      = "budgets"
)

// Report is the outcome of all checks.
type Report struct {
	Checks []Check
	// At is when the run started (Options.Now).
	At time.Time
}

// Failed reports whether any check failed. Warnings and skipped checks do not count.
func (r Report) Failed() bool {
	for _, c := range r.Checks {
		if c.Status == Fail {
			return true
		}
	}
	return false
}

// Write prints one line per check ("[FAIL] name: detail") and, when the check has one, a "fix:" line under it.
// Plain text, no colours.
func (r Report) Write(w io.Writer) {
	for _, c := range r.Checks {
		line := "[" + c.Status.String() + "] " + c.Name
		if c.Detail != "" {
			line += ": " + c.Detail
		}
		fmt.Fprintln(w, line)
		if c.Fix != "" {
			fmt.Fprintln(w, "       fix: "+c.Fix)
		}
	}
}

// Options are the inputs of Run.
type Options struct {
	// Getenv reads the environment (default os.Getenv).
	Getenv func(string) string
	// App is the GitHub App built from the environment; nil when it is not configured (or its key did not parse).
	App *product.GitHubApp
	// Repo is "owner/name"; the repository checks need it.
	Repo string
	// HTTP is the client for GitHub calls; it is used for an App that has none of its own.
	HTTP *http.Client
	// Now is the clock (default time.Now).
	Now func() time.Time
}

// finding is one observation of a check; a check's status is the worst of its findings.
type finding struct {
	status Status
	detail string
	fix    string
}

// combine folds findings into one check: OK with okDetail when there are none, else the worst status with every
// finding's detail and fix.
func combine(name, okDetail string, fs ...finding) Check {
	if len(fs) == 0 {
		return Check{Name: name, Status: OK, Detail: okDetail}
	}
	c := Check{Name: name}
	var details, fixes []string
	for _, f := range fs {
		if f.status > c.Status && f.status != Skip {
			c.Status = f.status
		}
		details = append(details, f.detail)
		if f.fix != "" {
			fixes = append(fixes, f.fix)
		}
	}
	c.Detail = strings.Join(details, "; ")
	c.Fix = strings.Join(fixes, " ")
	return c
}

// run holds the state the checks share.
type run struct {
	o       Options
	ctx     context.Context
	getenv  func(string) string
	secrets []string
	checks  []Check

	cfg      *config.Config
	cfgErr   error
	info     *product.AppInfo
	owner    string
	repo     string
	repoErr  string
	install  int64
	details  *product.RepoDetails
	tokenOK  bool
	appReady bool
}

// Run executes every check. It never writes to GitHub, and a failing call fails only its own check.
func Run(ctx context.Context, o Options) Report {
	r := &run{o: o, ctx: ctx, getenv: o.Getenv}
	if r.getenv == nil {
		r.getenv = os.Getenv
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	if o.App != nil && o.HTTP != nil && o.App.HTTP == nil {
		o.App.HTTP = o.HTTP
	}
	if s := r.getenv("FACTORY_WEBHOOK_SECRET"); s != "" {
		r.secrets = append(r.secrets, s)
	}
	r.parseRepo()
	r.loadConfig()

	r.factoryEnabled()
	r.githubApp()
	r.webhookSecret()
	r.permissionsAndEvents()
	r.installed()
	r.token()
	r.repoAccess()
	r.defaultBranch()
	r.reviewer()
	r.sandbox()
	r.sandboxQuota()
	r.budgets()
	return Report{Checks: r.checks, At: now()}
}

func (r *run) add(c Check) { r.checks = append(r.checks, c) }

func (r *run) skip(name, why, fix string) {
	r.add(Check{Name: name, Status: Skip, Detail: why, Fix: fix})
}

func (r *run) fail(name string, err error, fix string) {
	r.add(Check{Name: name, Status: Fail, Detail: r.errText(err), Fix: fix})
}

// cfgFail reports a setting the harness itself would refuse (a non-numeric MAX_SANDBOXES_PER_USER, say).
func (r *run) cfgFail(name string) {
	r.add(Check{Name: name, Status: Fail, Detail: "the configuration is invalid: " + r.errText(r.cfgErr),
		Fix: "correct the variable named in the message"})
}

// errText is an error message safe to print: the webhook secret and anything shaped like a credential is masked.
func (r *run) errText(err error) string { return forge.Redact(err.Error(), r.secrets...) }

func (r *run) parseRepo() {
	switch owner, name, ok := strings.Cut(strings.TrimSpace(r.o.Repo), "/"); {
	case strings.TrimSpace(r.o.Repo) == "":
		r.repoErr = "no repository given"
	case !ok || owner == "" || name == "" || strings.Contains(name, "/"):
		r.repoErr = fmt.Sprintf("%q is not owner/name", r.o.Repo)
	default:
		r.owner, r.repo = owner, name
	}
}

// repoSkip adds a skipped check when there is no usable repository and reports whether it did.
func (r *run) repoSkip(name string) bool {
	if r.repoErr == "" {
		return false
	}
	if strings.HasPrefix(r.repoErr, "no repository") {
		r.skip(name, r.repoErr, "pass the repository to check as owner/name")
	} else {
		r.add(Check{Name: name, Status: Fail, Detail: r.repoErr, Fix: "pass the repository to check as owner/name"})
	}
	return true
}

// loadConfig reads the settings the harness itself defaults (models, sandbox backend, quota) through the config
// package, so a default here is the default the server would use.
func (r *run) loadConfig() {
	var environ []string
	for _, k := range []string{
		"LLM_DEFAULT_MODEL", "LLM_MODELS", "SANDBOX_BACKEND", "OPENSANDBOX_URL",
		"OPENSANDBOX_CREDENTIAL_PROXY", "MAX_SANDBOXES_PER_USER",
	} {
		if v := r.getenv(k); v != "" {
			environ = append(environ, k+"="+v)
		}
	}
	r.cfg, r.cfgErr = config.LoadWith(config.LoadOptions{NoFiles: true, Environ: environ})
}

func (r *run) factoryEnabled() {
	if r.getenv("FACTORY") != "1" {
		r.add(Check{Name: NameFactory, Status: Fail, Detail: "FACTORY is not 1: harness-web will not start the factory",
			Fix: "set FACTORY=1"})
		return
	}
	r.add(Check{Name: NameFactory, Status: OK, Detail: "FACTORY=1"})
}

func (r *run) githubApp() {
	if r.o.App != nil {
		r.appReady = true
		r.add(Check{Name: NameApp, Status: OK, Detail: "the App's private key parsed"})
		return
	}
	switch {
	case r.getenv("GITHUB_APP_ID") == "":
		r.add(Check{Name: NameApp, Status: Fail, Detail: "GITHUB_APP_ID is not set: no GitHub App is configured",
			Fix: "set GITHUB_APP_ID and GITHUB_APP_PRIVATE_KEY (or GITHUB_APP_PRIVATE_KEY_FILE) of the App"})
	case r.getenv("GITHUB_APP_PRIVATE_KEY") == "" && r.getenv("GITHUB_APP_PRIVATE_KEY_FILE") == "":
		r.add(Check{Name: NameApp, Status: Fail, Detail: "GITHUB_APP_ID is set but the App has no private key",
			Fix: "set GITHUB_APP_PRIVATE_KEY (PEM) or GITHUB_APP_PRIVATE_KEY_FILE"})
	default:
		r.add(Check{Name: NameApp, Status: Fail, Detail: "the App's private key could not be read or parsed",
			Fix: "GITHUB_APP_PRIVATE_KEY / GITHUB_APP_PRIVATE_KEY_FILE must hold the PEM key generated on the App's settings page"})
	}
}

func (r *run) webhookSecret() {
	s := r.getenv("FACTORY_WEBHOOK_SECRET")
	switch {
	case s == "":
		r.add(Check{Name: NameWebhook, Status: Fail, Detail: "FACTORY_WEBHOOK_SECRET is not set: every webhook delivery is refused",
			Fix: "set FACTORY_WEBHOOK_SECRET to the secret of the App's webhook (POST /hooks/github)"})
	case len(s) < 16:
		r.add(Check{Name: NameWebhook, Status: Warn, Detail: fmt.Sprintf("the secret is %d characters: easy to guess", len(s)),
			Fix: "use a random secret of at least 16 characters, on both the App's webhook and FACTORY_WEBHOOK_SECRET"})
	default:
		r.add(Check{Name: NameWebhook, Status: OK, Detail: "set"})
	}
}

// level orders GitHub's permission levels.
func level(s string) int {
	switch s {
	case "read":
		return 1
	case "write":
		return 2
	case "admin":
		return 3
	}
	return 0
}

// requiredPermissions are what the factory does with the App: contents (push the branch), pull requests (open
// and merge), issues and checks (read the trigger and CI), metadata (GitHub always grants it).
var requiredPermissions = []struct {
	key, label, want string
}{
	{"contents", "Contents", "write"},
	{"pull_requests", "Pull requests", "write"},
	{"issues", "Issues", "read"},
	{"checks", "Checks", "read"},
	{"metadata", "Metadata", "read"},
}

func (r *run) permissionsAndEvents() {
	if !r.appReady {
		r.skip(NamePermissions, "no GitHub App", "")
		r.skip(NameEvents, "no GitHub App", "")
		return
	}
	info, err := r.o.App.Info(r.ctx)
	if err != nil {
		r.fail(NamePermissions, fmt.Errorf("reading the App from GitHub: %w", err),
			"check GITHUB_APP_ID and that the private key belongs to that App")
		r.skip(NameEvents, "the App could not be read", "")
		return
	}
	r.info = info

	var fs []finding
	for _, p := range requiredPermissions {
		got := info.Permissions[p.key]
		if level(got) >= level(p.want) {
			continue
		}
		have := "missing"
		if got != "" {
			have = "only " + got
		}
		fs = append(fs, finding{Fail, fmt.Sprintf("%s: %s (needs %s)", p.key, have, p.want),
			fmt.Sprintf("GitHub > Settings > Developer settings > GitHub Apps > %s > Permissions & events > Repository permissions: set %s to %s, then accept the new permissions on the installation.",
				appName(info, r.o.App), p.label, access(p.want))})
	}
	r.add(combine(NamePermissions, "contents, pull requests, issues, checks and metadata are granted", fs...))

	fs = nil
	for _, e := range []string{"issues", "issue_comment", "pull_request", "pull_request_review", "check_suite"} {
		if slices.Contains(info.Events, e) {
			continue
		}
		why := "the loop still polls, just slower"
		if e == "issues" {
			why = "label triggers cannot fire"
		}
		fs = append(fs, finding{Warn, fmt.Sprintf("%s is not subscribed (%s)", e, why),
			fmt.Sprintf("GitHub App settings > Permissions & events > Subscribe to events: tick %s.", e)})
	}
	r.add(combine(NameEvents, "issues, comments, pull requests, reviews and check suites are subscribed", fs...))
}

func appName(info *product.AppInfo, app *product.GitHubApp) string {
	switch {
	case info != nil && info.Name != "":
		return info.Name
	case info != nil && info.Slug != "":
		return info.Slug
	}
	return app.Slug
}

func access(level string) string {
	if level == "write" {
		return "Read and write"
	}
	return "Read-only"
}

func (r *run) installed() {
	if !r.appReady {
		r.skip(NameInstalled, "no GitHub App", "")
		return
	}
	if r.repoSkip(NameInstalled) {
		return
	}
	id, err := r.o.App.RepoInstallation(r.ctx, r.owner, r.repo)
	if errors.Is(err, product.ErrNotInstalled) {
		slug := r.o.App.Slug
		if r.info != nil && r.info.Slug != "" {
			slug = r.info.Slug
		}
		fix := "install the GitHub App on " + r.owner + "/" + r.repo + " (set GITHUB_APP_SLUG for a link)"
		if slug != "" {
			fix = "install the GitHub App on " + r.owner + "/" + r.repo + ": https://github.com/apps/" + slug + "/installations/new"
		}
		r.add(Check{Name: NameInstalled, Status: Fail, Detail: "the App is not installed on " + r.owner + "/" + r.repo, Fix: fix})
		return
	}
	if err != nil {
		r.fail(NameInstalled, err, "")
		return
	}
	r.install = id
	r.add(Check{Name: NameInstalled, Status: OK, Detail: "installation " + strconv.FormatInt(id, 10)})
}

func (r *run) token() {
	if r.install == 0 {
		r.skip(NameToken, "the App is not installed on the repository", "")
		return
	}
	tok, err := r.o.App.InstallationToken(r.ctx, r.install, r.repo)
	if err != nil {
		r.fail(NameToken, fmt.Errorf("minting a token for %s: %w", r.repo, err),
			"the App needs access to "+r.owner+"/"+r.repo+" in its installation settings")
		return
	}
	r.secrets = append(r.secrets, tok)
	r.tokenOK = true
	r.add(Check{Name: NameToken, Status: OK, Detail: "a token limited to " + r.repo + " mints"})
}

func (r *run) repoAccess() {
	if !r.tokenOK {
		r.skip(NameRepoAccess, "no installation token", "")
		return
	}
	d, err := r.o.App.RepoDetails(r.ctx, r.install, r.owner, r.repo)
	if err != nil {
		r.fail(NameRepoAccess, err, "")
		return
	}
	r.details = d
	if !d.CanPush {
		r.add(Check{Name: NameRepoAccess, Status: Fail, Detail: "the installation token cannot push to " + d.FullName,
			Fix: "give the App Contents: Read and write and accept it on the installation; an installation limited to read cannot push the factory's branch"})
		return
	}
	r.add(Check{Name: NameRepoAccess, Status: OK, Detail: "the token can push to " + d.FullName})
}

func (r *run) defaultBranch() {
	if r.details == nil {
		r.skip(NameBranch, "the repository could not be read", "")
		return
	}
	b, err := r.o.App.Branch(r.ctx, r.install, r.owner, r.repo, r.details.DefaultBranch)
	if errors.Is(err, product.ErrBranchNotFound) {
		r.add(Check{Name: NameBranch, Status: Fail, Detail: fmt.Sprintf("the default branch %q does not exist", r.details.DefaultBranch),
			Fix: "push a first commit to the repository (an empty repository has no branch to open pull requests against)"})
		return
	}
	if err != nil {
		r.fail(NameBranch, err, "")
		return
	}
	if !b.Protected {
		r.add(Check{Name: NameBranch, Status: Warn,
			Detail: fmt.Sprintf("%s: branch protection is off: the factory relies on it as the last line of defence against a bad merge", b.Name),
			Fix:    "Repository settings > Branches > add a rule for " + b.Name + ": require a pull request and passing status checks"})
		return
	}
	r.add(Check{Name: NameBranch, Status: OK, Detail: b.Name + " exists and is protected"})
}

func (r *run) reviewer() {
	if r.cfgErr != nil {
		r.cfgFail(NameReviewer)
		return
	}
	model := strings.TrimSpace(r.getenv("FACTORY_REVIEW_MODEL"))
	noReview := r.getenv("FACTORY_NO_REVIEW") == "1"
	author := r.cfg.LLMDefaultModel
	var fs []finding
	switch {
	case noReview:
		fs = append(fs, finding{Warn, "FACTORY_NO_REVIEW=1: merges rest on CI alone",
			"unset it to have a model review every pull request before it can merge"})
	case model == "":
		// Allowed: the default model reviews. It grades its own work, so the review is weaker than an independent one,
		// and every approval it posts says so.
		fs = append(fs, finding{Warn, fmt.Sprintf("no FACTORY_REVIEW_MODEL: %q reviews the changes it wrote itself, so the review is not independent", author),
			"set FACTORY_REVIEW_MODEL to a different model on your router for an independent review"})
	default:
		if model == author {
			fs = append(fs, finding{Fail, fmt.Sprintf("the reviewer %q is the author's model: the gate refuses to approve its own author's work", model),
				"leave FACTORY_REVIEW_MODEL unset to use the default model on purpose, or set a different one"})
		}
		if !r.cfg.ModelAllowed(model) {
			fs = append(fs, finding{Fail, fmt.Sprintf("the reviewer %q is not in LLM_MODELS", model),
				"add " + model + " to LLM_MODELS"})
		}
	}
	okDetail := ""
	if model != "" && !noReview {
		okDetail = fmt.Sprintf("%s reviews, %s writes", model, author)
	}
	r.add(combine(NameReviewer, okDetail, fs...))
}

func (r *run) sandbox() {
	if r.cfgErr != nil {
		r.cfgFail(NameSandbox)
		return
	}
	team := r.getenv("HARNESS_TEAM") == "1" || r.getenv("OIDC_ISSUER") != ""
	backend := r.cfg.SandboxBackendName()
	var fs []finding
	switch backend {
	case config.BackendNone:
		fs = append(fs, finding{Fail, "no sandbox backend: the factory's agents have no tools",
			"set SANDBOX_BACKEND=opensandbox with OPENSANDBOX_URL"})
	case config.BackendLocal:
		if team && r.getenv("HARNESS_ALLOW_LOCAL_SANDBOX") != "1" {
			fs = append(fs, finding{Fail, "SANDBOX_BACKEND=local in team mode: the agent's commands run on this machine without isolation",
				"set SANDBOX_BACKEND=opensandbox, or HARNESS_ALLOW_LOCAL_SANDBOX=1 to accept the risk"})
		} else if team {
			fs = append(fs, finding{Warn, "SANDBOX_BACKEND=local accepted with HARNESS_ALLOW_LOCAL_SANDBOX=1: no isolation",
				"use SANDBOX_BACKEND=opensandbox for untrusted tasks"})
		}
	case config.BackendOpenSandbox:
		// The agent holds no push credential: the host pushes on open_pr. The token only exists, briefly, in the
		// environment of the clone command, so the credential proxy is no longer needed to keep it from the agent.
	}
	r.add(combine(NameSandbox, backend+" backend", fs...))
}

func (r *run) sandboxQuota() {
	if r.cfgErr != nil {
		r.cfgFail(NameSandboxQuota)
		return
	}
	concurrent := 4
	if v := strings.TrimSpace(r.getenv("FACTORY_MAX_CONCURRENT")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			r.add(Check{Name: NameSandboxQuota, Status: Fail, Detail: fmt.Sprintf("FACTORY_MAX_CONCURRENT=%q is not a positive number", v),
				Fix: "set FACTORY_MAX_CONCURRENT to the number of tasks to run at once"})
			return
		}
		concurrent = n
	}
	quota := r.cfg.MaxSandboxesPerUser
	if quota <= 0 {
		quota = 5 // the sandbox manager's own default
	}
	var fs []finding
	switch {
	case quota < concurrent:
		fs = append(fs, finding{Fail, fmt.Sprintf("MAX_SANDBOXES_PER_USER=%d is below FACTORY_MAX_CONCURRENT=%d: a task's run cannot get a sandbox", quota, concurrent),
			fmt.Sprintf("raise MAX_SANDBOXES_PER_USER to at least %d, or lower FACTORY_MAX_CONCURRENT", concurrent*2)})
	case quota < concurrent*2:
		fs = append(fs, finding{Warn, fmt.Sprintf("MAX_SANDBOXES_PER_USER=%d is below twice FACTORY_MAX_CONCURRENT=%d: delegated sub-agents need their own sandbox", quota, concurrent),
			fmt.Sprintf("raise MAX_SANDBOXES_PER_USER to at least %d", concurrent*2)})
	}
	r.add(combine(NameSandboxQuota, fmt.Sprintf("%d sandboxes for %d concurrent tasks", quota, concurrent), fs...))
}

func (r *run) budgets() {
	// A task's budget is set by whoever creates the task or the trigger, so the deployment has only the day's cap.
	var fs []finding
	capped := false
	for _, k := range []string{"FACTORY_DAILY_BUDGET_USD", "FACTORY_DAILY_BUDGET_TOKENS"} {
		if f, err := strconv.ParseFloat(strings.TrimSpace(r.getenv(k)), 64); err == nil && f > 0 {
			capped = true
		}
	}
	if !capped {
		fs = append(fs, finding{Warn, "no daily spend cap: the day's total is unlimited, and a task has a cap only if its creator sets one",
			"set FACTORY_DAILY_BUDGET_USD or FACTORY_DAILY_BUDGET_TOKENS (tokens are counted for every model; dollars only for models with a known price)"})
	}
	for _, k := range []string{"FACTORY_TASK_BUDGET_USD", "FACTORY_TASK_BUDGET_TOKENS"} {
		if strings.TrimSpace(r.getenv(k)) != "" {
			fs = append(fs, finding{Warn, k + " is set but no longer read: a task's budget belongs to whoever creates the task",
				"remove " + k + " from the environment"})
		}
	}
	r.add(combine(NameBudgets, "a daily spend cap is set; a task's own budget is set by whoever creates it", fs...))
}

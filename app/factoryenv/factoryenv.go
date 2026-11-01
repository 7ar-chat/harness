// Package factoryenv turns the FACTORY_* and GITHUB_APP_* environment into the options of the agent factory. It is
// shared by the processes that must install the same factory: harness-web (which dispatches tasks and serves the
// API) and harness-worker (which executes the runs, so it must fire the end-of-run hooks, serve the tools and
// enforce the policy and the budget of the tasks it runs).
package factoryenv

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/factorystore"
	"github.com/7ar-chat/harness/app/product"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/features/factory/forge"
	"github.com/7ar-chat/harness/modelinfo"
)

// Wiring is what FACTORY_* turns into: the facade's options and the escalator and hooks the product binds to.
type Wiring struct {
	Options   harness.FactoryOptions
	Escalator *product.FactoryEscalator
	Hooks     *product.FactoryHooks
}

// FromEnv reads the FACTORY_* variables. The factory is off (enabled false) unless FACTORY=1.
//
//	FACTORY=1                       turn the agent factory on (team mode)
//	FACTORY_RUN=0                   do not run the dispatcher, scheduler and PR loop in this process (default 1)
//	FACTORY_WEBHOOK_SECRET          GitHub webhook secret (POST /hooks/github); without it deliveries are refused
//	FACTORY_REVIEW_MODEL            model of the independent reviewer that gates merges (needs the GitHub App)
//	FACTORY_NO_REVIEW=1             merge on green CI without the reviewer (deliberate opt-out)
//	FACTORY_REQUIRE_TESTS=1         the reviewer flags source changes that touch no test
//	FACTORY_PROTECTED               comma-separated path globs no agent change may touch
//	FACTORY_MAX_CONCURRENT          tasks running at once (default 4)
//	FACTORY_MAX_ATTEMPTS            attempts per task (default 3)
//	FACTORY_POLICY                  default autonomy: supervised | sandboxed | autonomous (default sandboxed)
//	FACTORY_TASK_BUDGET_USD / _TOKENS    default budget of one task
//	FACTORY_DAILY_BUDGET_USD / _TOKENS   cap on all factory spend per 24h (FACTORY_DAILY_PER_PROJECT=1: per project)
//	FACTORY_LESSONS=1               distil lessons from finished tasks into project memory
//
// st is the task store (nil: in memory, nothing survives the process; harness-web passes a file-backed or Postgres one). team links the threads of a task to its project; nil leaves them private
// to the owner, which is what a process without the team store wants.
func FromEnv(getenv func(string) string, cfg *config.Config, st factory.Store, gh *product.GitHubApp, team store.TeamStore) (Wiring, bool, error) {
	var w Wiring
	if getenv("FACTORY") != "1" {
		return w, false, nil
	}
	if st == nil {
		// No store given: keep the factory in memory, for embedders and tests.
		st = factorystore.NewMem(nil)
	}
	num := func(name string) (float64, error) {
		v := strings.TrimSpace(getenv(name))
		if v == "" {
			return 0, nil
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 {
			return 0, fmt.Errorf("%s: %q is not a non-negative number", name, v)
		}
		return f, nil
	}
	var err error
	o := harness.FactoryOptions{
		Store: st, Run: getenv("FACTORY_RUN") != "0", Policy: true, Lessons: getenv("FACTORY_LESSONS") == "1",
		WebhookSecret: []byte(getenv("FACTORY_WEBHOOK_SECRET")), RequireTests: getenv("FACTORY_REQUIRE_TESTS") == "1",
		NoReview: getenv("FACTORY_NO_REVIEW") == "1", ReviewModel: getenv("FACTORY_REVIEW_MODEL"),
		DefaultPolicy: getenv("FACTORY_POLICY"), PerProject: getenv("FACTORY_DAILY_PER_PROJECT") == "1",
		Protected: splitList(getenv("FACTORY_PROTECTED")),
	}
	switch o.DefaultPolicy {
	case "", factory.PolicySupervised, factory.PolicySandboxed, factory.PolicyAutonomous:
	default:
		return w, false, fmt.Errorf("FACTORY_POLICY: %q is not supervised, sandboxed or autonomous", o.DefaultPolicy)
	}
	for name, dst := range map[string]*float64{
		"FACTORY_TASK_BUDGET_USD": &o.DefaultBudget.MaxUSD, "FACTORY_DAILY_BUDGET_USD": &o.DailyBudget.MaxUSD,
	} {
		if *dst, err = num(name); err != nil {
			return w, false, err
		}
	}
	for name, dst := range map[string]*int64{
		"FACTORY_TASK_BUDGET_TOKENS": &o.DefaultBudget.MaxTokens, "FACTORY_DAILY_BUDGET_TOKENS": &o.DailyBudget.MaxTokens,
	} {
		f, err := num(name)
		if err != nil {
			return w, false, err
		}
		*dst = int64(f)
	}
	for name, dst := range map[string]*int{"FACTORY_MAX_CONCURRENT": &o.MaxConcurrent, "FACTORY_MAX_ATTEMPTS": &o.MaxAttempts} {
		f, err := num(name)
		if err != nil {
			return w, false, err
		}
		*dst = int(f)
	}
	if mo, ok := config.ModelPriceOptions(); ok {
		// The table loads in the background; until it arrives every model is unpriced (tokens are still counted).
		table := modelinfo.LoadAsync(context.Background(), mo)
		o.Price = func(model string) (modelinfo.Info, bool) {
			if i, ok := table.Lookup(model, cfg.LLMProviderName); ok {
				return i, true
			}
			return table.Lookup(model, cfg.LLMProvider)
		}
	}
	if gh != nil {
		o.Auth = product.FactoryAuth{App: gh}
		o.Forge = forge.NewGitHub(forge.GitHubOptions{Auth: o.Auth, API: gh.API})
		if o.ReviewModel == "" && !o.NoReview {
			return w, false, errors.New("FACTORY with a GitHub App needs FACTORY_REVIEW_MODEL (an independent reviewer gates every merge) or FACTORY_NO_REVIEW=1")
		}
	}
	if len(o.WebhookSecret) == 0 {
		slog.Warn("FACTORY without FACTORY_WEBHOOK_SECRET: GitHub deliveries will be refused")
	}
	w.Escalator = &product.FactoryEscalator{}
	o.Escalator = w.Escalator
	w.Hooks = &product.FactoryHooks{Team: team}
	o.OnSpawn = w.Hooks.OnSpawn
	o.PollEvery = 5 * time.Second
	w.Options = o
	return w, true, nil
}

// GitHubFromEnv builds the GitHub App from GITHUB_APP_ID, GITHUB_APP_SLUG and
// GITHUB_APP_PRIVATE_KEY (PEM) or GITHUB_APP_PRIVATE_KEY_FILE; nil when no App is set.
func GitHubFromEnv(getenv func(string) string) (*product.GitHubApp, error) {
	id := getenv("GITHUB_APP_ID")
	if id == "" {
		return nil, nil
	}
	key := []byte(getenv("GITHUB_APP_PRIVATE_KEY"))
	if f := getenv("GITHUB_APP_PRIVATE_KEY_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("read GITHUB_APP_PRIVATE_KEY_FILE: %w", err)
		}
		key = b
	}
	if len(key) == 0 {
		return nil, fmt.Errorf("GITHUB_APP_ID needs GITHUB_APP_PRIVATE_KEY or GITHUB_APP_PRIVATE_KEY_FILE")
	}
	app, err := product.NewGitHubApp(id, getenv("GITHUB_APP_SLUG"), key)
	if err != nil {
		return nil, err
	}
	app.ClientID, app.ClientSecret = getenv("GITHUB_APP_CLIENT_ID"), getenv("GITHUB_APP_CLIENT_SECRET")
	return app, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

package factoryenv

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/store/factorystore"
	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/app/product"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/store"
)

// TestMain keeps the tests from downloading the model price table.
func TestMain(m *testing.M) {
	os.Setenv("HARNESS_MODEL_PRICES", "off")
	os.Exit(m.Run())
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// fxEnv is a self-contained configuration: one model, private directories, no Postgres, Redis or GitHub App.
func fxEnv(t *testing.T) {
	t.Helper()
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	t.Setenv("LLM_DEFAULT_MODEL", "gpt-test")
	t.Setenv("LOCAL_STATE_DIR", t.TempDir())
	t.Setenv("LOCAL_WORKSPACE", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("HAR_HOME", filepath.Join(t.TempDir(), "har"))
}

func fxConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

var (
	fxKeyOnce sync.Once
	fxKeyPEM  []byte
)

// fxPrivateKeyPEM is a throw-away GitHub App key.
func fxPrivateKeyPEM(t *testing.T) []byte {
	t.Helper()
	fxKeyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		fxKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	})
	return fxKeyPEM
}

// fxApp is a GitHub App with a throw-away key, the way githubFromEnv builds one.
func fxApp(t *testing.T) *product.GitHubApp {
	t.Helper()
	app, err := product.NewGitHubApp("1", "my-app", fxPrivateKeyPEM(t))
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func TestFactoryIsOffUnlessSwitchedOn(t *testing.T) {
	fxEnv(t)
	cases := map[string]map[string]string{
		"unset":           nil,
		"0":               {"FACTORY": "0"},
		"true":            {"FACTORY": "true"},
		"only the extras": {"FACTORY_WEBHOOK_SECRET": "s", "FACTORY_REVIEW_MODEL": "m"},
		// An invalid setting of a switched-off factory is not an error: nothing reads it.
		"bad value, but off": {"FACTORY_POLICY": "anarchy"},
	}
	for name, vars := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			w, on, err := FromEnv(env(vars), fxConfig(t), nil, nil, nil)

			// Assert
			if on || err != nil || w.Escalator != nil || w.Hooks != nil || w.Options.Store != nil {
				t.Errorf("on=%v err=%v wiring=%+v, want an inert result", on, err, w)
			}
		})
	}
}

func TestFactoryFromEnvReadsEveryVariable(t *testing.T) {
	// Arrange
	fxEnv(t)
	team := teamstore.NewMem()
	st := factorystore.NewMem(nil)
	vars := map[string]string{
		"FACTORY": "1", "FACTORY_RUN": "0", "FACTORY_WEBHOOK_SECRET": "whsec", "FACTORY_REVIEW_MODEL": "reviewer-model",
		"FACTORY_NO_REVIEW": "1", "FACTORY_REQUIRE_TESTS": "1", "FACTORY_PROTECTED": "infra/**, *.pem ,,",
		"FACTORY_MAX_CONCURRENT": "7", "FACTORY_MAX_ATTEMPTS": "5", "FACTORY_POLICY": "supervised",
		"FACTORY_DAILY_BUDGET_USD": "40", "FACTORY_DAILY_BUDGET_TOKENS": "9000000", "FACTORY_DAILY_PER_PROJECT": "1",
		"FACTORY_LESSONS": "1",
	}

	// Act
	w, on, err := FromEnv(env(vars), fxConfig(t), st, nil, team)

	// Assert
	if err != nil || !on {
		t.Fatalf("on=%v err=%v", on, err)
	}
	o := w.Options
	if o.Store != factory.Store(st) || o.Run || !o.Policy || !o.Lessons || !o.RequireTests || !o.NoReview || !o.PerProject {
		t.Errorf("flags: %+v", o)
	}
	if string(o.WebhookSecret) != "whsec" || o.ReviewModel != "reviewer-model" || o.DefaultPolicy != factory.PolicySupervised {
		t.Errorf("strings: secret %q review %q policy %q", o.WebhookSecret, o.ReviewModel, o.DefaultPolicy)
	}
	if len(o.Protected) != 2 || o.Protected[0] != "infra/**" || o.Protected[1] != "*.pem" {
		t.Errorf("Protected = %q", o.Protected)
	}
	if o.MaxConcurrent != 7 || o.MaxAttempts != 5 {
		t.Errorf("limits: %d, %d", o.MaxConcurrent, o.MaxAttempts)
	}
	if o.DefaultBudget != (factory.Budget{}) || o.DailyBudget != (factory.Budget{MaxUSD: 40, MaxTokens: 9000000}) {
		t.Errorf("budgets: task %+v daily %+v; only the daily cap is global", o.DefaultBudget, o.DailyBudget)
	}
	if o.PollEvery != 5*time.Second || o.Escalator != factory.Escalator(w.Escalator) || w.Escalator == nil || w.Hooks == nil || o.OnSpawn == nil {
		t.Errorf("wiring: poll %v escalator %v hooks %v", o.PollEvery, o.Escalator, w.Hooks)
	}
	if o.Forge != nil || o.Auth != nil {
		t.Error("a forge was configured without a GitHub App")
	}
}

func TestFactoryFromEnvDefaultsToTheDocumentedOnes(t *testing.T) {
	// Arrange
	fxEnv(t)

	// Act
	w, on, err := FromEnv(env(map[string]string{"FACTORY": "1"}), fxConfig(t), nil, nil, nil)

	// Assert: runs here, policy engine on, no caps; the zero values let the harness apply its own defaults.
	if err != nil || !on {
		t.Fatalf("on=%v err=%v", on, err)
	}
	o := w.Options
	if !o.Run || !o.Policy || o.Lessons || o.RequireTests || o.NoReview || o.PerProject || o.DefaultPolicy != "" {
		t.Errorf("defaults: %+v", o)
	}
	if o.DefaultBudget != (factory.Budget{}) || o.DailyBudget != (factory.Budget{}) || o.MaxConcurrent != 0 || o.MaxAttempts != 0 || len(o.WebhookSecret) != 0 {
		t.Errorf("caps: %+v", o)
	}
}

func TestFactoryFromEnvFallsBackToMemoryWithoutAStore(t *testing.T) {
	// Arrange
	fxEnv(t)
	ctx := context.Background()

	// Act
	w, _, err := FromEnv(env(map[string]string{"FACTORY": "1"}), fxConfig(t), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Assert: a working store, not nil: tasks can be created straight away.
	if w.Options.Store == nil {
		t.Fatal("no store without Postgres")
	}
	task, created, err := w.Options.Store.CreateTask(ctx, factory.Task{Owner: "ann", Title: "t"})
	if err != nil || !created {
		t.Fatalf("CreateTask = %v, %v", created, err)
	}
	if got, err := w.Options.Store.GetTask(ctx, task.ID); err != nil || got.Title != "t" {
		t.Errorf("GetTask = %v, %v", got, err)
	}
}

func TestFactoryFromEnvRejectsBadValuesNamingTheVariable(t *testing.T) {
	fxEnv(t)
	for name, tc := range map[string]struct{ key, value string }{
		"policy":                 {"FACTORY_POLICY", "anarchy"},
		"daily usd not a number": {"FACTORY_DAILY_BUDGET_USD", "lots"},
		"daily tokens":           {"FACTORY_DAILY_BUDGET_TOKENS", "many"},
		"max concurrent":         {"FACTORY_MAX_CONCURRENT", "four"},
		"max attempts negative":  {"FACTORY_MAX_ATTEMPTS", "-3"},
	} {
		t.Run(name, func(t *testing.T) {
			// Act
			_, on, err := FromEnv(env(map[string]string{"FACTORY": "1", tc.key: tc.value}), fxConfig(t), nil, nil, nil)

			// Assert
			if err == nil || on || !strings.Contains(err.Error(), tc.key) {
				t.Errorf("on=%v err=%v, want an error naming %s", on, err, tc.key)
			}
		})
	}
}

func TestFactoryWithAGitHubAppStartsWithoutAReviewerModelAndFallsBackOnTheDefaultOne(t *testing.T) {
	fxEnv(t)
	app := fxApp(t)
	for name, vars := range map[string]map[string]string{
		"no reviewer model":    {"FACTORY": "1"},
		"empty reviewer model": {"FACTORY": "1", "FACTORY_REVIEW_MODEL": ""},
		"a reviewer model":     {"FACTORY": "1", "FACTORY_REVIEW_MODEL": "reviewer"},
		"the explicit opt out": {"FACTORY": "1", "FACTORY_NO_REVIEW": "1"},
	} {
		t.Run(name, func(t *testing.T) {
			// Act
			w, on, err := FromEnv(env(vars), fxConfig(t), nil, app, nil)

			// Assert: never an error. The model is resolved by the facade: empty means the default model reviews.
			if err != nil || !on {
				t.Fatalf("on=%v err=%v", on, err)
			}
			if w.Options.Forge == nil || w.Options.Auth == nil || w.Options.ReviewModel != vars["FACTORY_REVIEW_MODEL"] {
				t.Errorf("options = %+v", w.Options)
			}
		})
	}
}

func TestTheRemovedGlobalTaskBudgetIsIgnoredAndSaidSo(t *testing.T) {
	// Arrange
	fxEnv(t)
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	// Act
	w, on, err := FromEnv(env(map[string]string{"FACTORY": "1", "FACTORY_TASK_BUDGET_USD": "5", "FACTORY_TASK_BUDGET_TOKENS": "100"}), fxConfig(t), nil, nil, nil)

	// Assert: no default budget reaches the tasks, and the operator is told why their setting did nothing.
	if err != nil || !on {
		t.Fatalf("on=%v err=%v", on, err)
	}
	if w.Options.DefaultBudget != (factory.Budget{}) {
		t.Errorf("DefaultBudget = %+v, want none", w.Options.DefaultBudget)
	}
	for _, name := range []string{"FACTORY_TASK_BUDGET_USD", "FACTORY_TASK_BUDGET_TOKENS"} {
		if !strings.Contains(logs.String(), name+" is no longer read") {
			t.Errorf("no warning for %s in %q", name, logs.String())
		}
	}
}

func TestFactoryPricesAreOffWhenTheTableIsDisabled(t *testing.T) {
	// Arrange: TestMain sets HARNESS_MODEL_PRICES=off.
	fxEnv(t)

	// Act
	w, _, err := FromEnv(env(map[string]string{"FACTORY": "1"}), fxConfig(t), nil, nil, nil)

	// Assert
	if err != nil || w.Options.Price != nil {
		t.Errorf("err=%v Price set=%v, want tokens only", err, w.Options.Price != nil)
	}
}

func TestFactoryHooksLinkATasksThreadToItsProject(t *testing.T) {
	// Arrange
	fxEnv(t)
	ctx := context.Background()
	team := teamstore.NewMem()
	p, err := team.CreateProject(ctx, store.Project{Name: "api"}, "ann")
	if err != nil {
		t.Fatal(err)
	}
	w, _, err := FromEnv(env(map[string]string{"FACTORY": "1"}), fxConfig(t), nil, nil, team)
	if err != nil {
		t.Fatal(err)
	}

	// Act: the facade calls the hook the options carry.
	w.Options.OnSpawn(ctx, "thread-1", factory.Task{ID: "t1", ProjectID: p.ID, Owner: "ann"}, factory.SpawnRequest{Login: "ann", Title: "work"})
	w.Options.OnSpawn(ctx, "thread-2", factory.Task{ID: "t2", Owner: "ann"}, factory.SpawnRequest{Login: "ann"})

	// Assert
	m, err := team.GetThreadMeta(ctx, "thread-1")
	if err != nil || m.ProjectID != p.ID || m.Owner != "ann" || m.CreatedBy != "ann" {
		t.Errorf("meta = %+v, %v", m, err)
	}
	if _, err := team.GetThreadMeta(ctx, "thread-2"); err == nil {
		t.Error("a task without a project was linked")
	}
}

func TestGitHubFromEnv(t *testing.T) {
	pemKey := string(fxPrivateKeyPEM(t))
	keyFile := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(keyFile, []byte(pemKey), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		env     map[string]string
		wantNil bool
		wantErr string
	}{
		{"no app configured", nil, true, ""},
		{"app id without a key", map[string]string{"GITHUB_APP_ID": "1"}, false, "GITHUB_APP_PRIVATE_KEY"},
		{"key that is not PEM", map[string]string{"GITHUB_APP_ID": "1", "GITHUB_APP_PRIVATE_KEY": "garbage"}, false, "PEM"},
		{"key file that does not exist", map[string]string{"GITHUB_APP_ID": "1", "GITHUB_APP_PRIVATE_KEY_FILE": filepath.Join(t.TempDir(), "missing")}, false, "GITHUB_APP_PRIVATE_KEY_FILE"},
		{"inline key", map[string]string{"GITHUB_APP_ID": "1", "GITHUB_APP_SLUG": "my-app", "GITHUB_APP_PRIVATE_KEY": pemKey, "GITHUB_APP_CLIENT_ID": "cid", "GITHUB_APP_CLIENT_SECRET": "csec"}, false, ""},
		{"key file wins over a broken inline key", map[string]string{"GITHUB_APP_ID": "1", "GITHUB_APP_PRIVATE_KEY": "garbage", "GITHUB_APP_PRIVATE_KEY_FILE": keyFile}, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Act
			app, err := GitHubFromEnv(env(c.env))

			// Assert
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil || (app == nil) != c.wantNil {
				t.Fatalf("app %v err %v", app, err)
			}
			if app != nil && c.env["GITHUB_APP_CLIENT_ID"] != "" && (app.ClientID != "cid" || app.ClientSecret != "csec" || app.Slug != "my-app") {
				t.Errorf("app = %+v", app)
			}
		})
	}
}

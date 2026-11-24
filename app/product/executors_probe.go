package product

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/7ar-chat/harness/core/executors"
	"github.com/7ar-chat/harness/core/sandbox"
)

// Availability of an executor's CLI in the default sandbox image, as ExecutorView.Availability says it.
const (
	AvailReady   = "ready"   // the binary is in the image
	AvailInstall = "install" // it is not, and a run installs it on first use
	AvailMissing = "missing" // it is not, and nothing installs it: an admin must add it to the image
	AvailUnknown = "unknown" // no sandbox backend, or the probe has not answered yet
)

const (
	probeTTL     = 10 * time.Minute
	probeWait    = 3 * time.Second
	probeRunTime = 90 * time.Second
	probeKey     = "executors-probe"
)

// cliProbe finds which CLIs the default sandbox image has: one `command -v` per binary in a short-lived sandbox,
// cached for probeTTL. It is best effort: a request never waits more than probeWait for it.
type cliProbe struct {
	mu      sync.Mutex
	at      time.Time
	found   map[string]bool
	running chan struct{}
}

// probe returns whether each binary is in the image (ok=false when unknown), starting a probe when the cache is stale.
func (p *cliProbe) probe(ctx context.Context, mgr sandbox.Manager, binaries []string) (map[string]bool, bool) {
	if mgr == nil || !mgr.Enabled() {
		return nil, false
	}
	p.mu.Lock()
	covers := p.found != nil && time.Since(p.at) < probeTTL
	if covers {
		for _, b := range binaries {
			if _, ok := p.found[b]; !ok {
				covers = false
				break
			}
		}
	}
	if covers {
		defer p.mu.Unlock()
		return p.found, true
	}
	done := p.running
	if done == nil {
		done = make(chan struct{})
		p.running = done
		go p.run(mgr, binaries, done)
	}
	p.mu.Unlock()
	t := time.NewTimer(probeWait)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	case <-ctx.Done():
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.found != nil {
		for _, b := range binaries {
			if _, ok := p.found[b]; !ok {
				return nil, false
			}
		}
		return p.found, true
	}
	return nil, false
}

func (p *cliProbe) run(mgr sandbox.Manager, binaries []string, done chan struct{}) {
	defer func() {
		p.mu.Lock()
		p.running = nil
		p.mu.Unlock()
		close(done)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), probeRunTime)
	defer cancel()
	sb, err := mgr.Acquire(ctx, probeKey, "system")
	if err != nil {
		slog.Debug("executors: the availability probe has no sandbox", "error", err)
		return
	}
	defer func() {
		_ = mgr.Kill(context.WithoutCancel(ctx), sb.ID())
		mgr.Invalidate(context.WithoutCancel(ctx), probeKey, sb.ID())
	}()
	uniq := map[string]bool{}
	for _, b := range binaries {
		uniq[b] = true
	}
	var names []string
	for b := range uniq {
		names = append(names, b)
	}
	sort.Strings(names)
	var q strings.Builder
	for _, b := range names {
		q.WriteString(" '" + strings.ReplaceAll(b, "'", `'\''`) + "'")
	}
	var out strings.Builder
	res, err := sb.Exec(ctx, sandbox.ExecRequest{
		Command:  `for b in` + q.String() + `; do if command -v "$b" >/dev/null 2>&1; then echo "FOUND $b"; else echo "MISSING $b"; fi; done`,
		Timeout:  30 * time.Second,
		OnStdout: func(t string) { out.WriteString(t) },
	})
	if err != nil || res.ExitCode != 0 {
		return
	}
	found := map[string]bool{}
	for _, line := range strings.Split(out.String(), "\n") {
		switch {
		case strings.HasPrefix(line, "FOUND "):
			found[strings.TrimSpace(strings.TrimPrefix(line, "FOUND "))] = true
		case strings.HasPrefix(line, "MISSING "):
			found[strings.TrimSpace(strings.TrimPrefix(line, "MISSING "))] = false
		}
	}
	p.mu.Lock()
	p.found, p.at = found, time.Now()
	p.mu.Unlock()
}

// availability labels each executor of list.
func (a *ExecutorAPI) availability(ctx context.Context, list []executors.Executor) map[string]string {
	out := make(map[string]string, len(list))
	bins := make([]string, 0, len(list))
	for _, e := range list {
		if e.Binary != "" {
			bins = append(bins, e.Binary)
		}
	}
	found, ok := a.cli.probe(ctx, a.sandboxes(), bins)
	for _, e := range list {
		switch {
		case !ok || e.Binary == "":
			out[e.Name] = AvailUnknown
		case found[e.Binary]:
			out[e.Name] = AvailReady
		case e.InstallCommand != "" && a.AllowInstall:
			out[e.Name] = AvailInstall
		default:
			out[e.Name] = AvailMissing
		}
	}
	return out
}

func (a *ExecutorAPI) sandboxes() sandbox.Manager {
	a.mgrMu.Lock()
	defer a.mgrMu.Unlock()
	return a.mgr
}

// SetSandboxes gives the availability probe its sandbox manager (the Feature does it when the harness is built).
func (a *ExecutorAPI) SetSandboxes(m sandbox.Manager) {
	a.mgrMu.Lock()
	a.mgr = m
	a.mgrMu.Unlock()
}

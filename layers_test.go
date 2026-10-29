package harness_test

import (
	"os/exec"
	"sort"
	"strings"
	"testing"
)

const module = "github.com/GhaziBenDahmane/harness"

// layer says where a package of this module sits. A package may import only packages of its own layer or a
// lower one; the rules below tighten that further.
//
//	0 foundation  store (interfaces), config, modelinfo, engine/agui, engine/syncx, engine/agent/prompts
//	1 sandbox     the sandbox contract, its manager and backends
//	2 engine      the agent loop, LLM clients, tools, runtime, event log, queue, approvals
//	3 adapters    store implementations (memory, file, Postgres), Redis helpers
//	4 features    optional capabilities built on the engine (permissions, memory, skills, documents...)
//	5 hosts       the facade (package harness), the client contract, TUI, web server, worker
//	9 tooling     benchmarks, long-session tests and commands: may import anything
func layer(p string) int {
	rel := strings.TrimPrefix(strings.TrimPrefix(p, module), "/")
	switch {
	case rel == "":
		return 5
	case rel == "store", rel == "config", rel == "modelinfo", rel == "engine/agui", rel == "engine/syncx", rel == "engine/agent/prompts":
		return 0
	case strings.HasPrefix(rel, "sandbox"):
		return 1
	case strings.HasPrefix(rel, "engine/"):
		return 2
	case strings.HasPrefix(rel, "store/"), strings.HasPrefix(rel, "redisx"):
		return 3
	case strings.HasPrefix(rel, "features/"):
		return 4
	case strings.HasPrefix(rel, "hosts/"):
		return 5
	}
	return 9 // bench, longtest, cmd/...
}

// featureEdges are the few places where one feature builds on another.
var featureEdges = map[string][]string{
	"features/memory/memorytools": {"features/memory"},
	"features/skills/skilltools":  {"features/skills"},
	"features/verify":             {"features/goals"},
	"features/workspace":          {"features/artifacts"},
}

func TestPackagesOnlyImportTheSameOrLowerLayers(t *testing.T) {
	// Arrange
	out, err := exec.Command("go", "list", "-f", "{{.ImportPath}} {{join .Imports \" \"}}", "./...").Output()
	if err != nil {
		t.Fatal(err)
	}

	// Act
	violations := findViolations(strings.Split(strings.TrimSpace(string(out)), "\n"))

	// Assert
	if len(violations) > 0 {
		t.Fatalf("layering violations:\n  %s", strings.Join(violations, "\n  "))
	}
}

func TestTheLayeringCheckCatchesBadImports(t *testing.T) {
	// Arrange: an engine package importing a feature and a host, a feature importing an unrelated feature,
	// and legal edges that must not be reported.
	m := module + "/"
	lines := []string{
		m + "engine/agent " + m + "features/memory " + m + "hosts/web " + m + "engine/llm " + m + "store",
		m + "features/goals " + m + "features/artifacts " + m + "engine/agent",
		m + "features/verify " + m + "features/goals",
		m + "features/memory/memorytools " + m + "features/memory",
		m + "sandbox " + m + "engine/agent",
		m + "store/memstore " + m + "store " + m + "engine/eventlog",
		m + "bench " + m + " " + m + "hosts/tui",
	}

	// Act
	got := strings.Join(findViolations(lines), "\n")

	// Assert
	for _, want := range []string{
		"engine/agent (layer 2) imports features/memory (layer 4)",
		"engine/agent (layer 2) imports hosts/web (layer 5)",
		"features/goals imports the feature features/artifacts",
		"sandbox (layer 1) imports engine/agent (layer 2)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing violation %q in:\n%s", want, got)
		}
	}
	for _, bad := range []string{"features/verify imports", "memorytools imports", "store/memstore", "bench "} {
		if strings.Contains(got, bad) {
			t.Errorf("a legal edge was reported (%q):\n%s", bad, got)
		}
	}
}

func TestTheHostRulesCatchBadImports(t *testing.T) {
	// Arrange: synthetic violations of each rule, and the legal edges next to them.
	m := module + "/"
	lines := []string{
		m + "hosts/tui " + m + " " + m + "hosts/web " + m + "hosts/worker " + m + "engine/agent " + m + "features/memory " +
			m + "store " + m + "sandbox " + m + "hosts/client " + m + "engine/agui",
		m + "hosts/web " + m + "hosts/tui " + m + "hosts/client " + m + " " + m + "store",
		m + "hosts/worker " + m + "hosts/web " + m + "hosts/client " + m + " " + m + "engine/runtime",
		m + "hosts/client " + m + "hosts/tui " + m + " " + m + "hosts/client/clienttest",
		m + "engine/agent " + m + "hosts/client",
		m + "features/memory " + m + "hosts/web",
		m + " " + m + "hosts/web " + m + "engine/agent",
		m + "hosts/client/clienttest " + m + "hosts/client " + m + " " + m + "engine/llm/llmtest",
	}

	// Act
	got := strings.Join(findViolations(lines), "\n")

	// Assert
	for _, want := range []string{
		"hosts/tui imports the facade (package harness) (hosts/tui may import only",
		"hosts/tui imports hosts/web (hosts/tui may import only",
		"hosts/tui imports hosts/worker (hosts/tui may import only",
		"hosts/tui imports engine/agent (hosts/tui may import only",
		"hosts/tui imports features/memory (hosts/tui may import only",
		"hosts/tui imports store (hosts/tui may import only",
		"hosts/tui imports sandbox (hosts/tui may import only",
		"hosts/web imports hosts/tui (hosts/web must not import another host)",
		"hosts/worker imports hosts/web (hosts/worker must not import another host)",
		"hosts/worker imports hosts/client (hosts/worker must not import another host)",
		"hosts/client imports hosts/tui (hosts/client must not import a host that uses it)",
		"engine/agent imports hosts/client (the engine must not import hosts or the facade)",
		"features/memory imports hosts/web (the engine must not import hosts or the facade)",
		"the facade (package harness) imports hosts/web (hosts import the facade, not the reverse)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing violation %q in:\n%s", want, got)
		}
	}
	for _, bad := range []string{"hosts/tui imports hosts/client ", "hosts/tui imports engine/agui", "hosts/web imports hosts/client",
		"hosts/web imports the facade", "hosts/web imports store", "hosts/worker imports the facade", "hosts/worker imports engine/runtime",
		"hosts/client imports the facade", "hosts/client imports hosts/client/clienttest", "clienttest imports"} {
		if strings.Contains(got, bad) {
			t.Errorf("a legal edge was reported (%q):\n%s", bad, got)
		}
	}
}

// findViolations takes `go list` lines ("<package> <imports...>") and returns the import edges that break the layers.
func findViolations(lines []string) []string {
	var violations []string
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		pkg, imports := fields[0], fields[1:]
		rel := strings.TrimPrefix(strings.TrimPrefix(pkg, module), "/")
		for _, imp := range imports {
			if !strings.HasPrefix(imp, module) {
				continue
			}
			irel := strings.TrimPrefix(strings.TrimPrefix(imp, module), "/")
			if layer(pkg) < layer(imp) {
				violations = append(violations, rel+" (layer "+itoa(layer(pkg))+") imports "+irel+" (layer "+itoa(layer(imp))+")")
			}
			if v := hostViolation(rel, irel); v != "" {
				violations = append(violations, v)
			}
			if layer(pkg) == 4 && layer(imp) == 4 && !sameFeature(rel, irel) && !allowed(rel, irel) {
				violations = append(violations, rel+" imports the feature "+irel+" (not an allowed edge)")
			}
		}
	}
	sort.Strings(violations)
	return violations
}

// hostViolation applies the rules between the front ends and the engine. The picture:
//
//	engine <- facade (package harness) <- hosts/client (InProcess) <- front ends (hosts/tui, other apps)
//	                                      hosts/web (HTTP API server) <- hosts/client (HTTP)
//
// A front end is written against hosts/client only, so that it works the same on an in-process engine and on a
// remote server. Hosts do not depend on each other.
func hostViolation(pkg, imp string) string {
	isHost := func(p string) bool { return strings.HasPrefix(p, "hosts/") }
	under := func(p, root string) bool { return p == root || strings.HasPrefix(p, root+"/") }
	name := func(p string) string {
		if p == "" {
			return "the facade (package harness)"
		}
		return p
	}
	switch {
	case under(pkg, "hosts/tui"):
		if imp != "engine/agui" && imp != "hosts/client" && !under(imp, "hosts/tui") {
			return pkg + " imports " + name(imp) + " (hosts/tui may import only hosts/client and engine/agui)"
		}
	case under(pkg, "hosts/web"):
		if under(imp, "hosts/tui") || under(imp, "hosts/worker") {
			return pkg + " imports " + imp + " (hosts/web must not import another host)"
		}
	case under(pkg, "hosts/worker"):
		if isHost(imp) && !under(imp, "hosts/worker") {
			return pkg + " imports " + imp + " (hosts/worker must not import another host)"
		}
	case under(pkg, "hosts/client"):
		if isHost(imp) && !under(imp, "hosts/client") {
			return pkg + " imports " + imp + " (hosts/client must not import a host that uses it)"
		}
	case under(pkg, "engine"), under(pkg, "features"), under(pkg, "store"), under(pkg, "sandbox"), under(pkg, "config"):
		if isHost(imp) || imp == "" {
			return pkg + " imports " + name(imp) + " (the engine must not import hosts or the facade)"
		}
	case pkg == "":
		if isHost(imp) {
			return "the facade (package harness) imports " + imp + " (hosts import the facade, not the reverse)"
		}
	}
	return ""
}

func itoa(n int) string { return string(rune('0' + n)) }

// sameFeature: a feature and its subpackages (features/skills and features/skills/skillsfs).
func sameFeature(a, b string) bool {
	root := func(p string) string { return strings.Join(strings.SplitN(p, "/", 3)[:2], "/") }
	return root(a) == root(b)
}

// TestTheCoreDoesNotDependOnRedis: engine, sandbox, features, store and config define the interfaces the
// infrastructure must satisfy; only the adapter package redisx (and the facade, as a convenience) import go-redis.
func TestTheCoreDoesNotDependOnRedis(t *testing.T) {
	// Arrange / Act
	out, err := exec.Command("go", "list", "-deps", "./engine/...", "./sandbox/...", "./features/...", "./store/...", "./config/...").Output()
	if err != nil {
		t.Fatal(err)
	}

	// Assert
	for _, dep := range strings.Fields(string(out)) {
		if strings.Contains(dep, "redis/go-redis") {
			t.Errorf("the core depends on %s (use redisx)", dep)
		}
	}
}

func allowed(from, to string) bool {
	for _, t := range featureEdges[from] {
		if t == to {
			return true
		}
	}
	return false
}

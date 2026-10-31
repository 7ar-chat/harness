package harness_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

const module = "github.com/7ar-chat/harness"

// layer says where a package of this module sits. A package may import only packages of its own layer or a
// lower one; the rules below tighten that further.
//
//	0 foundation  core/ (events, domain types, ports: agui, store, sandbox, blobs), ai/ (messages, tools, model
//	              contract and the OpenAI client), config, modelinfo, engine/syncx, prompts
//	2 engine      the agent loop, LLM clients, tools, runtime, event log, queue, approvals
//	3 plug-ins    adapters/ (store, sandbox, blob and Redis implementations of the ports) and coding/ (the
//	              coding toolset); they do not import each other's group
//	4 features    optional capabilities built on the engine (permissions, memory, skills, documents...)
//	5 hosts       the facade (package harness), the client contract, TUI, HTTP API, worker
//	6 product     teams, identity and connectors on top of the HTTP API (app/product/)
//	9 tooling     benchmarks, long-session tests, examples and commands: may import anything
func layer(p string) int {
	rel := strings.TrimPrefix(strings.TrimPrefix(p, module), "/")
	switch {
	case rel == "":
		return 5
	case under(rel, "core"), rel == "config", rel == "modelinfo", rel == "engine/syncx", rel == "prompts", under(rel, "ai"):
		return 0
	case strings.HasPrefix(rel, "engine/"):
		return 2
	case under(rel, "adapters"), under(rel, "coding"):
		return 3
	case strings.HasPrefix(rel, "features/"):
		return 4
	case strings.HasPrefix(rel, "hosts/"):
		return 5
	case under(rel, "app/product"):
		return 6
	}
	return 9 // bench, longtest, examples, app/cmd/...
}

func under(p, root string) bool { return p == root || strings.HasPrefix(p, root+"/") }

// featureEdges are the places where one feature builds on another (none: a feature and its subpackages, such as
// features/skills/skilltools, are one unit, and a shared type belongs in core).
var featureEdges = map[string][]string{}

// mayImportConfig lists the packages of ai/, engine/, adapters/ and features/ that still read the whole
// *config.Config. They are migrated to their own option structs after the MCP trust work; until then this list
// is the one place that names them. Every other package under those roots takes its own options (the
// composition roots - the facade, hosts and cmd - map config.Config onto them).
var mayImportConfig = map[string]bool{
	"features/mcp":         true,
	"features/permissions": true,
}

// configRoots are the trees whose packages must not import config (except mayImportConfig).
var configRoots = []string{"ai", "engine", "adapters", "features"}

func mustNotImportConfig(rel string) bool {
	for _, r := range configRoots {
		if under(rel, r) {
			return !mayImportConfig[rel]
		}
	}
	return false
}

func TestPackagesOnlyImportTheSameOrLowerLayers(t *testing.T) {
	// Arrange
	out, err := exec.Command("go", "list", "-f", "{{.ImportPath}} {{join .Imports \" \"}}", module+"/...").Output()
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
	// the core importing the settings, the plug-in groups importing each other, and legal edges that must not
	// be reported.
	m := module + "/"
	lines := []string{
		m + "engine/agent " + m + "features/memory " + m + "hosts/httpapi " + m + "ai/llm " + m + "core/store",
		m + "features/goals " + m + "features/artifacts " + m + "engine/agent " + m + "coding/sandboxtools",
		m + "features/artifacts/workspace " + m + "features/artifacts",
		m + "features/memory/memorytools " + m + "features/memory",
		m + "core/sandbox " + m + "engine/agent " + m + "config " + m + "core/store",
		m + "adapters/store/memstore " + m + "core/store " + m + "engine/eventlog",
		m + "adapters/redisx " + m + "coding/sandboxtools " + m + "adapters/store/memstore",
		m + "coding/sandboxtools " + m + "adapters/redisx " + m + "core/sandbox " + m + "engine/tools",
		m + "bench " + m + " " + m + "hosts/tui",
		m + "features/skills " + m + "config " + m + "core/layout",
		m + "adapters/redisx " + m + "config",
		m + "ai/llm " + m + "config",
		m + "features/mcp " + m + "config " + m + "engine/tools",
		m + "features/permissions " + m + "config",
	}

	// Act
	got := strings.Join(findViolations(lines), "\n")

	// Assert
	for _, want := range []string{
		"engine/agent (layer 2) imports features/memory (layer 4)",
		"engine/agent (layer 2) imports hosts/httpapi (layer 5)",
		"features/goals imports the feature features/artifacts",
		"core/sandbox (layer 0) imports engine/agent (layer 2)",
		"core/sandbox imports config (the core imports only core)",
		"features/skills imports config (take your own options",
		"adapters/redisx imports config (take your own options",
		"ai/llm imports config (take your own options",
		"adapters/redisx imports coding/sandboxtools (adapters and coding do not import each other)",
		"coding/sandboxtools imports adapters/redisx (adapters and coding do not import each other)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing violation %q in:\n%s", want, got)
		}
	}
	for _, bad := range []string{"features/goals imports coding", "workspace imports", "memorytools imports", "adapters/store/memstore", "bench ", "features/mcp imports config", "features/permissions imports config"} {
		if strings.Contains(got, bad) {
			t.Errorf("a legal edge was reported (%q):\n%s", bad, got)
		}
	}
}

func TestTheHostRulesCatchBadImports(t *testing.T) {
	// Arrange: synthetic violations of each rule, and the legal edges next to them.
	m := module + "/"
	lines := []string{
		m + "hosts/tui " + m + " " + m + "hosts/httpapi " + m + "hosts/worker " + m + "engine/agent " + m + "features/memory " +
			m + "core/store " + m + "core/sandbox " + m + "hosts/client " + m + "core/agui",
		m + "hosts/httpapi " + m + "hosts/tui " + m + "hosts/client " + m + " " + m + "core/store " + m + "engine/agent " +
			m + "features/memory " + m + "app/product",
		m + "hosts/worker " + m + "hosts/httpapi " + m + "hosts/client " + m + " " + m + "engine/runtime " + m + "adapters/redisx",
		m + "hosts/client " + m + "hosts/tui " + m + " " + m + "hosts/inproc " + m + "hosts/clienttest",
		m + "hosts/inproc " + m + "hosts/client " + m + " " + m + "hosts/tui",
		m + "app/product " + m + "hosts/httpapi " + m + "hosts/client " + m + " " + m + "core/store " + m + "engine/agent " + m + "hosts/tui " +
			m + "adapters/redisx " + m + "features/memory " + m + "features/goals",
		m + "engine/agent " + m + "hosts/client",
		m + "features/memory " + m + "hosts/httpapi",
		m + " " + m + "hosts/httpapi " + m + "engine/agent " + m + "app/product",
		m + "hosts/clienttest " + m + "hosts/client " + m + " " + m + "ai/llm/llmtest",
	}

	// Act
	got := strings.Join(findViolations(lines), "\n")

	// Assert
	for _, want := range []string{
		"hosts/tui imports the facade (package harness) (hosts/tui may import only",
		"hosts/tui imports hosts/httpapi (hosts/tui may import only",
		"hosts/tui imports hosts/worker (hosts/tui may import only",
		"hosts/tui imports engine/agent (hosts/tui may import only",
		"hosts/tui imports features/memory (hosts/tui may import only",
		"hosts/tui imports core/store (hosts/tui may import only",
		"hosts/tui imports core/sandbox (hosts/tui may import only",
		"hosts/httpapi imports hosts/tui (hosts/httpapi must not import another host)",
		"hosts/httpapi imports engine/agent (hosts/httpapi may import only",
		"hosts/httpapi imports features/memory (hosts/httpapi may import only",
		"hosts/httpapi imports app/product (hosts/httpapi may import only",
		"hosts/worker imports hosts/httpapi (hosts/worker must not import another host)",
		"hosts/worker imports hosts/client (hosts/worker must not import another host)",
		"hosts/client imports hosts/tui (hosts/client must not import a host that uses it)",
		"hosts/client imports hosts/inproc (hosts/client must not import a host that uses it)",
		"hosts/inproc imports hosts/tui (the client implementations must not import a front end)",
		"app/product imports adapters/redisx (product may import only",
		"app/product imports hosts/tui (product may import only",
		"app/product imports features/goals (product may import only",
		"engine/agent imports hosts/client (the engine must not import hosts or the facade)",
		"features/memory imports hosts/httpapi (the engine must not import hosts or the facade)",
		"the facade (package harness) imports hosts/httpapi (hosts import the facade, not the reverse)",
		"the facade (package harness) imports app/product (hosts import the facade, not the reverse)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing violation %q in:\n%s", want, got)
		}
	}
	for _, bad := range []string{"hosts/tui imports hosts/client ", "hosts/tui imports core/agui", "hosts/httpapi imports hosts/client",
		"hosts/httpapi imports the facade", "hosts/httpapi imports core/store", "hosts/worker imports the facade", "hosts/worker imports engine/runtime",
		"hosts/worker imports adapters", "hosts/client imports the facade", "hosts/inproc imports hosts/client", "clienttest imports", "hosts/httpapi imports hosts/inproc",
		"app/product imports hosts/httpapi", "app/product imports hosts/client", "app/product imports the facade", "app/product imports core/store", "app/product imports engine/agent", "app/product imports features/memory"} {
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
			if irel == "config" && mustNotImportConfig(rel) {
				violations = append(violations, rel+" imports config (take your own options; the composition root maps the configuration)")
			}
			if under(rel, "core") && !under(irel, "core") {
				violations = append(violations, rel+" imports "+irel+" (the core imports only core)")
			}
			if (under(rel, "adapters") && under(irel, "coding")) || (under(rel, "coding") && under(irel, "adapters")) {
				violations = append(violations, rel+" imports "+irel+" (adapters and coding do not import each other)")
			}
		}
	}
	sort.Strings(violations)
	return violations
}

// hostViolation applies the rules between the front ends and the engine. The picture:
//
//	engine <- facade (package harness) <- hosts/client (InProcess) <- front ends (hosts/tui, other apps)
//	                                      hosts/httpapi (HTTP API server) <- hosts/client (HTTP)
//	                                      app/product (teams, identity, connectors) <- hosts/httpapi
//
// A front end is written against hosts/client only, so that it works the same on an in-process engine and on a
// remote server. Hosts do not depend on each other. The HTTP API is a transport over the client contract: it
// knows the facade, the client DTOs and core, not the engine's internals; the product extends it through its
// extension points.
func hostViolation(pkg, imp string) string {
	isHost := func(p string) bool { return strings.HasPrefix(p, "hosts/") }
	// The client family: the contract and its two engine-bound implementations (in process, conformance suite).
	// Only the contract is free of the engine; the other two import the facade.
	family := func(p string) bool {
		return under(p, "hosts/client") || under(p, "hosts/inproc") || under(p, "hosts/clienttest")
	}
	name := func(p string) string {
		if p == "" {
			return "the facade (package harness)"
		}
		return p
	}
	switch {
	case under(pkg, "hosts/tui"):
		if imp != "core/agui" && imp != "hosts/client" && !under(imp, "hosts/tui") {
			return pkg + " imports " + name(imp) + " (hosts/tui may import only hosts/client and core/agui)"
		}
	case under(pkg, "hosts/httpapi"):
		if under(imp, "hosts/tui") || under(imp, "hosts/worker") {
			return pkg + " imports " + imp + " (hosts/httpapi must not import another host)"
		}
		if !under(imp, "hosts/httpapi") && !family(imp) && imp != "" && !under(imp, "core") &&
			imp != "config" && imp != "modelinfo" && !under(imp, "ai/llm/llmtest") {
			return pkg + " imports " + name(imp) + " (hosts/httpapi may import only the facade, hosts/client and core)"
		}
	case under(pkg, "hosts/worker"):
		if isHost(imp) && !under(imp, "hosts/worker") {
			return pkg + " imports " + imp + " (hosts/worker must not import another host)"
		}
	case under(pkg, "hosts/client"):
		if isHost(imp) && !under(imp, "hosts/client") {
			return pkg + " imports " + imp + " (hosts/client must not import a host that uses it)"
		}
	case under(pkg, "hosts/inproc"), under(pkg, "hosts/clienttest"):
		if isHost(imp) && !family(imp) {
			return pkg + " imports " + imp + " (the client implementations must not import a front end)"
		}
	case under(pkg, "app/product"):
		// The product extends the harness with features of its own (the supervisor's tools, the repository
		// checkout), so it may use the engine's extension API; it must not reach into the plug-ins or other hosts.
		if under(imp, "adapters") || under(imp, "coding") || isHost(imp) && !under(imp, "hosts/httpapi") && !family(imp) ||
			strings.HasPrefix(imp, "features/") && !under(imp, "features/memory") {
			return pkg + " imports " + name(imp) + " (product may import only the facade, hosts/httpapi, hosts/client, core and the engine's extension API)"
		}
	case under(pkg, "engine"), under(pkg, "features"), under(pkg, "core"), under(pkg, "config"), under(pkg, "adapters"), under(pkg, "coding"):
		if isHost(imp) || imp == "" || under(imp, "app/product") {
			return pkg + " imports " + name(imp) + " (the engine must not import hosts or the facade)"
		}
	case pkg == "":
		if isHost(imp) || under(imp, "app/product") {
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

// TestTheCoreDoesNotDependOnRedis: core, engine, features, coding and config define the interfaces the
// infrastructure must satisfy; only the adapter package adapters/redisx (and the facade, as a convenience) import go-redis.
func TestTheCoreDoesNotDependOnRedis(t *testing.T) {
	// Arrange / Act
	out, err := exec.Command("go", "list", "-deps", module+"/core/...", module+"/engine/...", module+"/features/...", module+"/coding/...", module+"/config/...").Output()
	if err != nil {
		t.Fatal(err)
	}

	// Assert
	for _, dep := range strings.Fields(string(out)) {
		if strings.Contains(dep, "redis/go-redis") {
			t.Errorf("the core depends on %s (use adapters/redisx)", dep)
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

// TestTheAIPackageImportsNothingFromTheModule: ai/ is the standalone vocabulary of models and tools. It may
// import the standard library and third-party code only, which is what lets it move into its own module.
func TestTheAIPackageImportsNothingFromTheModule(t *testing.T) {
	out, err := exec.Command("go", "list", "-f", "{{.ImportPath}} {{join .Imports \" \"}}", module+"/ai/...").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		for _, imp := range fields[1:] {
			if strings.HasPrefix(imp, module+"/") && !under(strings.TrimPrefix(imp, module+"/"), "ai") {
				t.Errorf("%s imports %s", fields[0], imp)
			}
		}
	}
}

// modules are the standalone Go modules of the repository and the modules each may require. Every one builds and
// tests with GOWORK=off, which is what makes it usable without the rest; the root module requires them all.
var modules = map[string][]string{
	"ai":               {},
	"core":             {},
	"modelinfo":        {},
	"hosts/client":     {"core", "modelinfo"},
	"hosts/tui":        {"core", "modelinfo", "hosts/client"},
	"adapters/sandbox": {"core"},
	// The product and the commands: they use the root module (the engine) and everything it uses.
	"app": {"ai", "core", "modelinfo", "hosts/client", "hosts/tui", "adapters/sandbox"},
}

// TestTheStandaloneModulesRequireOnlyTheirDeclaredModules: a module that starts importing the engine, a feature or a
// sibling it did not declare would stop building alone; this fails before it ships.
func TestTheStandaloneModulesRequireOnlyTheirDeclaredModules(t *testing.T) {
	for dir, allowedDeps := range modules {
		gomod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		for _, line := range strings.Split(string(gomod), "\n") {
			line = strings.TrimSpace(line)
			if !strings.Contains(line, module+"/") || strings.HasPrefix(line, "module ") || strings.HasPrefix(line, "replace ") {
				continue
			}
			dep := strings.TrimPrefix(strings.Fields(strings.TrimPrefix(line, "require "))[0], module+"/")
			if !slices.Contains(allowedDeps, dep) {
				t.Errorf("module %s requires %s, allowed: %v", dir, dep, allowedDeps)
			}
		}
	}
}

package test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The layering rule from ARCHITECTURE.md 3:
//
//	transport -> command -> domain -> Port (interface) -> adapter
//
// Enforced here by walking the import graph, not by review. Discipline decays;
// this does not.
//
// Three deliberate properties of the check:
//
//   - It reads imports with go/parser, not with a text search. A mention of
//     "net" in a docstring is not a violation, and a function-local import is --
//     a local `import serial` inside a method is exactly the shortcut that rots
//     a layering rule.
//   - It checks the full transitive closure of the forbidden packages. Banning
//     `net` in internal/domain while allowing `internal/transport` to import it
//     would be satisfied by a one-line detour.
//   - It names the offending file, line and import path, because a failing
//     architecture test that does not say where is worse than no test.

type forbiddenRule struct {
	pkg      string // the package whose imports are restricted
	reason   string
	forbid   []string            // import path prefixes
	allowFor map[string][]string // package -> import paths that are permitted
}

// layerRules is the single statement of the architecture. ARCHITECTURE.md
// describes it in prose; this is the version that is enforced.
var layerRules = []forbiddenRule{
	{
		pkg:    "internal/domain",
		reason: "the vocabulary must not know that hardware or wire formats exist",
		forbid: []string{
			"github.com/rocsar/obc/api/rocsar/v1", // generated protobuf
			"github.com/go-zeromq",                // the wire
			"go.bug.st/serial",                    // the wire
			"net", "net/http", "os/exec", "os", "io",
		},
	},
	{
		pkg:    "internal/telemetry",
		reason: "assembly is pure; a clock and a socket would make it untestable",
		forbid: []string{
			"github.com/rocsar/obc/api/rocsar/v1",
			"github.com/go-zeromq",
			"go.bug.st/serial",
			"net", "net/http", "os/exec",
		},
		allowFor: map[string][]string{
			// time is how a 1 Hz pipeline is expressed. It is not I/O.
			"internal/telemetry": {"time"},
		},
	},
	{
		pkg:    "internal/config",
		reason: "configuration must not reach into a subsystem",
		forbid: []string{
			"github.com/rocsar/obc/internal/gnss",
			"github.com/rocsar/obc/internal/pico",
			"github.com/rocsar/obc/internal/camera",
			"github.com/rocsar/obc/internal/sdr",
			"github.com/rocsar/obc/internal/qos",
			"github.com/rocsar/obc/internal/transport",
		},
	},
	{
		pkg:    "internal/gsview",
		reason: "presentation logic must not touch the wire or a socket",
		forbid: []string{
			"github.com/rocsar/obc/api/rocsar/v1", // generated protobuf
			"github.com/go-zeromq",                // the wire
			"go.bug.st/serial",                    // the wire
			"net", "net/http", "os/exec", "os", "io",
		},
	},
}

// subsystemOwners maps a package allowed to use a forbidden import to the reason
// it may. os/exec in particular is confined to a short list, and the list is the
// point: it is easy to add an entry and hard to notice you have.
//
// Adding one here is a decision, not a convenience, so the reason travels with it.
// Removing one is a decision too: internal/qos was on this list because it shelled
// out to tc, and it no longer does, because the shaper talks to rtnetlink. An
// allowance nothing uses is worse than no list at all, because it reads as a
// decision that is still in force.
var subsystemOwners = map[string][]string{
	"os/exec": {
		// Runs connect (found on PATH) and reads USB device nodes for the SDR.
		"internal/sdr",
		// Runs fswebcam to photograph the camera.
		"internal/camera",
	},
}

func TestLayeringBoundaries(t *testing.T) {
	root := moduleRoot(t)

	for _, rule := range layerRules {
		t.Run(rule.pkg, func(t *testing.T) {
			files := goFilesIn(t, filepath.Join(root, rule.pkg))

			for _, f := range files {
				imports := parseImports(t, f)

				for _, imp := range imports {
					if !matchesAny(imp.path, rule.forbid) {
						continue
					}
					if isAllowed(rule, imp.path) {
						continue
					}
					t.Errorf("%s:%d imports %q\n    %s must not depend on it.\n"+
						"    If this is genuinely necessary the architecture is wrong, not the test.",
						rel(t, root, f), imp.line, imp.path, rule.pkg)
				}
			}
		})
	}
}

func TestSubprocessIsConfinedToItsOwners(t *testing.T) {
	root := moduleRoot(t)
	allowed := map[string]bool{}
	for _, p := range subsystemOwners["os/exec"] {
		allowed[p] = true
	}

	for _, f := range allGoFiles(t, root) {
		rel := rel(t, root, f)

		// Test files are not in the shipped binary, so what they import is not a
		// property of the system this rule exists to constrain.
		//
		// The alternative -- letting a test reach outside the layering rules --
		// would be worse than useless, because the rules are only worth having
		// while something is allowed to break them.
		if strings.HasSuffix(rel, "_test.go") {
			continue
		}

		pkg := filepath.Dir(rel)

		for _, imp := range parseImports(t, f) {
			if imp.path != "os/exec" {
				continue
			}
			if allowed[pkg] {
				continue
			}
			t.Errorf("%s:%d imports os/exec\n"+
				"    Only %v may shell out. A new one means a new decision about what is\n"+
				"    allowed to reach outside the process, not a convenience.",
				rel, imp.line, keysOf(allowed))
		}
	}
}

// The generated protobuf bindings must be confined to the codec layer.
//
// Two reasons, both learned the hard way. First, they are the one dependency
// that changes shape when the schema does, and letting them spread means a
// schema change ripples through the whole system instead of one package.
// Second, `domain` is supposed to be free of the wire, and importing them is
// the fastest way to lose that.
func TestGeneratedProtobufIsConfinedToCodecPackages(t *testing.T) {
	root := moduleRoot(t)
	const genPath = "github.com/rocsar/obc/api/rocsar/v1"

	// The five places where the wire and the domain meet, and nowhere else.
	allowed := map[string]bool{
		// transport encodes and decodes the frames.
		"internal/transport": true,
		// pico is the COBS+protobuf link; the codec lives there.
		"internal/pico": true,
		// command dispatches CommandRequest, so it necessarily sees the wire
		// type. It converts to domain types at the top of each handler and
		// nothing below that point touches protobuf -- which is the property
		// this rule is actually protecting.
		"internal/command": true,
		// the composition root wires concrete implementations together.
		"cmd/obc": true,
		// client is the Ground Station's end of the same wire: it encodes
		// CommandRequest and decodes TelemetryFrame. It was promoted out of
		// tools/gs_cli, which escaped this rule only because allGoFiles never
		// looked at tools/ -- so the entry below is not a loosening, it is the
		// first time this package has been checked at all.
		//
		// The same reasoning as internal/command applies and no further: it
		// converts at the socket edge and nothing below that point touches
		// protobuf. It does NOT reach into internal/domain, because domain
		// types describe what the OBC saw, and this side of the link never saw
		// any of it -- it has a TelemetryFrame and that is the whole of its
		// knowledge.
		"internal/client": true,
		// cmd/gs tests speak the wire to a fake OBC: the handler answers
		// CommandRequests and the publisher emits TelemetryFrames, and naming
		// those types requires the import. Production code in cmd/gs (app.go,
		// main.go) must not import the bindings -- it touches every command
		// and owns none of the schema, through internal/client's Request and
		// Response aliases. That half is enforced separately, by
		// TestCmdGsProductionHasNoProtobuf below, because this allowlist
		// cannot tell a test file from the code it tests.
		"cmd/gs": true,
	}

	for _, f := range allGoFiles(t, root) {
		rel := rel(t, root, f)
		pkg := filepath.Dir(rel)

		for _, imp := range parseImports(t, f) {
			if imp.path != genPath {
				continue
			}
			if allowed[pkg] {
				continue
			}
			t.Errorf("%s:%d imports the generated protobuf bindings\n"+
				"    Only %v may. Everything else goes through domain types, which is what\n"+
				"    keeps the schema from becoming the whole system's interface.",
				rel, imp.line, keysOf(allowed))
		}
	}
}

// Production code in cmd/gs must not import the generated protobuf bindings,
// even though its tests may (see the cmd/gs entry in the allowlist above).
// The binding layer touches every command and owns none of the schema: names
// flow through internal/client's Request and Response aliases, and a oneof
// constructed by hand in app.go would be a field renumbering away from a
// silent runtime failure. Test files are exempt -- they speak the wire to a
// fake OBC, which is what tests are for -- because they are not shipped.
func TestCmdGsProductionHasNoProtobuf(t *testing.T) {
	root := moduleRoot(t)
	const genPath = "github.com/rocsar/obc/api/rocsar/v1"

	files := goFilesIn(t, filepath.Join(root, "cmd", "gs"))
	if len(files) == 0 {
		t.Fatal("no Go files in cmd/gs; the production check is vacuous")
	}
	seenTest := false
	for _, f := range files {
		rel := rel(t, root, f)
		if strings.HasSuffix(rel, "_test.go") {
			seenTest = true
			continue
		}
		for _, imp := range parseImports(t, f) {
			if imp.path != genPath {
				continue
			}
			t.Errorf("%s:%d imports the generated protobuf bindings in production code\n"+
				"    Name the type through internal/client instead. A hand-built oneof\n"+
				"    in the binding layer is a field renumbering away from silence.",
				rel, imp.line)
		}
	}
	if !seenTest {
		t.Error("no test files in cmd/gs; untested binding code is unbound code")
	}
}

// The browser must never decode protobuf. Go decodes once, in internal/client,
// and the frontend receives JSON; a TypeScript protobuf runtime would be a
// second decoder for the same wire format, in a language with none of this
// repository's wire tests. A rule nobody checks is a comment, so this checks
// both the dependency manifest and the sources: neither `npm install` nor a
// vendored file may smuggle the decoder in.
func TestFrontendHasNoProtobuf(t *testing.T) {
	root := moduleRoot(t)
	frontend := filepath.Join(root, "cmd", "gs", "frontend")

	manifest, err := os.ReadFile(filepath.Join(frontend, "package.json"))
	if err != nil {
		t.Fatalf("read frontend/package.json: %v", err)
	}
	for _, token := range []string{
		"protobufjs", "protobuf-es", "@bufbuild", "google-protobuf",
		"ts-proto", "pbjs", "prost", "protojson", "gogoproto",
	} {
		if strings.Contains(string(manifest), token) {
			t.Errorf("frontend/package.json depends on %q; the browser must not decode protobuf", token)
		}
	}

	// Sources are matched on decoder-shaped tokens, not on the bare word:
	// comments legitimately say "protobuf" when they state this very rule.
	// What cannot appear is an import of it, a .proto file, or a generated
	// stub.
	src, err := os.ReadDir(filepath.Join(frontend, "src"))
	if err != nil {
		t.Fatalf("read frontend/src: %v", err)
	}
	for _, entry := range src {
		name := entry.Name()
		if entry.IsDir() {
			continue
		}
		if strings.HasSuffix(name, ".css") {
			continue
		}
		if strings.HasSuffix(name, ".proto") || strings.Contains(name, "_pb2") {
			t.Errorf("frontend/src/%s looks like a protobuf artefact; the browser receives JSON", name)
			continue
		}
		body, err := os.ReadFile(filepath.Join(frontend, "src", name))
		if err != nil {
			t.Fatalf("read frontend/src/%s: %v", name, err)
		}
		text := string(body)
		for _, token := range []string{
			"protobufjs", "protobuf-es", "@bufbuild", "google-protobuf",
			"ts-proto", "pbjs", "prost", "protojson", "gogoproto", ".proto\"",
			".proto'", "from \"buffer\"", "from 'buffer'",
		} {
			if strings.Contains(text, token) {
				t.Errorf("frontend/src/%s contains %q; the browser must not decode protobuf", name, token)
			}
		}
	}
}

// Every Port in internal/domain must have both a real adapter and a mock.
//
// An interface with one implementation is a class with no seam: there is no
// second implementation to disagree with it, so the interface is documentation
// rather than a boundary.
//
// Satisfaction is not inferred from names -- domain.Pico is implemented by
// pico.Link, and a name-based check would either miss it or match
// pico.MockPico by accident. It is asserted explicitly and idiomatically:
//
//	var _ domain.Pico = (*pico.Link)(nil)
//
// which the compiler checks, so this test only has to find the assertions and
// classify them. That is both cheaper and more trustworthy than reflection over
// the type graph.
func TestEveryPortHasAnAdapterAndAMock(t *testing.T) {
	root := moduleRoot(t)

	ports := declaredInterfaces(t, filepath.Join(root, "internal/domain"))
	if len(ports) == 0 {
		t.Fatal("no interfaces found in internal/domain; the port test is broken")
	}

	// interface -> {"real": [pkg...], "mock": [pkg...]}
	impl := map[string]map[string][]string{
		"real": {}, "mock": {},
	}

	for _, f := range allGoFiles(t, root) {
		pkg := filepath.Dir(rel(t, root, f))
		for name, mocked := range parseAssertions(t, f) {
			if impl[name] == nil {
				impl[name] = map[string][]string{"real": {}, "mock": {}}
			}
			kind := "real"
			if mocked {
				kind = "mock"
			}
			impl[name][kind] = append(impl[name][kind], pkg)
		}
	}

	var problems []string
	for _, port := range ports {
		kinds, asserted := impl[port]
		if !asserted {
			problems = append(problems, port+
				": no `var _ domain."+port+" = ...` assertion anywhere; nothing claims to implement it")
			continue
		}
		if len(kinds["mock"]) == 0 {
			if _, excluded := notAPort[port]; excluded {
				continue // implemented, and deliberately not mocked
			}
			problems = append(problems, port+
				": asserted only by "+joinPkgs(kinds["real"])+", with no mock")
		}
	}

	if len(problems) > 0 {
		sortStrings(problems)
		t.Errorf("ports without both an adapter and a mock:\n  %s\n"+
			"    A port with one implementation cannot be tested against a disagreement.\n"+
			"    Add a `var _ domain.%s = (*Something)(nil)` assertion where it is satisfied.",
			strings.Join(problems, "\n  "), "<Port>")
	}
}

func joinPkgs(pkgs []string) string {
	seen := map[string]bool{}
	var out []string
	for _, p := range pkgs {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sortStrings(out)
	return strings.Join(out, ", ")
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// assertionRegex matches the compile-time satisfaction idiom, in both forms Go
// programmers actually write:
//
//	var _ domain.Pico = (*Link)(nil)          // one line
//	var (                                     // or grouped, with no `var`
//	    _ domain.Pico = (*Link)(nil)
//	)
//
// Group 1 is the interface, group 2 the concrete type.
//
// `var` is OPTIONAL and that is the whole point. Requiring it silently misses
// every grouped block, which is the majority of them; forbidding it silently
// misses every one-line assertion. Both spellings are common, so the pattern
// accepts both. Getting this wrong in either direction looks exactly like
// "nobody has implemented this port".
var assertionRegex = regexp.MustCompile(`(?m)^\s*(?:var\s+)?_\s+\w+\.(\w+)\s*=\s*\(\*(\w+)\)`)

// doublePrefixes name the types this test counts as test doubles.
//
// Null and Recording are included because a null object IS a double: it accepts
// every call, changes nothing, and records what it was asked to do. NullShaper is
// exactly that -- it is what a laptop, a test and CI get instead of touching a
// real NIC -- and requiring a separate Mock alongside it would be two types
// doing one job.
var doublePrefixes = []string{"Mock", "Fake", "Null", "Recording", "Stub"}

// notAPort names interfaces that are not hardware ports and must not be held to
// the mock rule.
//
// Shutdown is a lifecycle constraint ("this type can be released"), not a port.
// There is nothing to disagree about: either a type can be closed or it cannot,
// and a MockShutdown that recorded Close calls would be theatre. It is still
// required to have an implementation, so it cannot rot unnoticed.
var notAPort = map[string]string{
	"Shutdown": "a lifecycle constraint, not a hardware port; there is no behaviour to mock",
}

func parseAssertions(t *testing.T, file string) map[string]bool {
	t.Helper()
	body, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading %s: %v", file, err)
	}
	out := map[string]bool{}
	for _, m := range assertionRegex.FindAllStringSubmatch(string(body), -1) {
		concrete := m[2]
		isDouble := false
		for _, p := range doublePrefixes {
			if strings.HasPrefix(concrete, p) {
				isDouble = true
				break
			}
		}
		out[m[1]] = isDouble
	}
	return out
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

type imp struct {
	path string
	line int
}

func parseImports(t *testing.T, file string) []imp {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}

	var out []imp
	for _, spec := range f.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatalf("bad import in %s: %v", file, err)
		}
		out = append(out, imp{path: p, line: fset.Position(spec.Pos()).Line})
	}
	return out
}

func matchesAny(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

func isAllowed(rule forbiddenRule, path string) bool {
	for _, ok := range rule.allowFor[rule.pkg] {
		if path == ok {
			return true
		}
	}
	return false
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func goFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(p, ".go") {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return out
}

func allGoFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	for _, sub := range []string{"cmd", "internal"} {
		out = append(out, goFilesIn(t, filepath.Join(root, sub))...)
	}
	return out
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join(".."))
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func rel(t *testing.T, root, path string) string {
	t.Helper()
	r, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return r
}

// declaredInterfaces returns the names of the interfaces declared in a package.
//
// Only interfaces. The package also declares structs -- Fix, Photo, PicoTelemetry
// and the rest -- and treating those as ports would demand a mock for a data
// type, which is meaningless.
func declaredInterfaces(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	for _, f := range goFilesIn(t, dir) {
		fset := token.NewFileSet()
		pf, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", f, err)
		}
		for _, d := range pf.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || ts.Name == nil {
					continue
				}
				if _, isIface := ts.Type.(*ast.InterfaceType); isIface {
					out = append(out, ts.Name.Name)
				}
			}
		}
	}
	return out
}

package test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"github.com/rocsar/obc/internal/config"
)

// The shipped rocsar.toml must parse, validate, and contain no key outside
// config.KnownKeys.
//
// The last clause is the one that earns its keep. TOML reads a key written
// after a [section] header as belonging to that section, so a top-level key
// placed at the end of the file becomes <section>.<key> -- an unknown key,
// which this system deliberately IGNORES rather than rejects. require_hardware
// sat under [qos] that way and behaved identically to correct, because the
// ignored key's default happened to equal the intended value. A test that only
// checked the resolved values would have passed. This one checks the key names.
func TestShippedConfigFileIsWellFormed(t *testing.T) {
	path := filepath.Join("..", "rocsar.toml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var doc map[string]any
	if err := toml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s does not parse: %v", path, err)
	}

	flattened := map[string]any{}
	var walk func(prefix string, m map[string]any)
	walk = func(prefix string, m map[string]any) {
		for k, v := range m {
			key := k
			if prefix != "" {
				key = prefix + "." + k
			}
			if sub, ok := v.(map[string]any); ok {
				walk(key, sub)
				continue
			}
			flattened[key] = v
		}
	}
	walk("", doc)

	if len(flattened) == 0 {
		t.Fatal("rocsar.toml parsed to nothing")
	}
	for _, k := range config.UnknownKeys(flattened) {
		t.Errorf("rocsar.toml has key %q, which is not in config.KnownKeys.\n"+
			"  Either it is a typo, or it landed in the wrong [section] -- a TOML key\n"+
			"  written after a section header belongs to that section, and an unknown\n"+
			"  key is IGNORED rather than rejected, so it fails open.", k)
	}

	t.Setenv(config.PathEnv, path)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// Every value in the shipped file must survive the round trip through the
// resolver with the declared type. A string where a number belongs parses fine
// and then fails at the point of use, which is usually a flight.
func TestShippedConfigTypesResolve(t *testing.T) {
	path := filepath.Join("..", "rocsar.toml")
	t.Setenv(config.PathEnv, path)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Link.RateKbps == 0 {
		t.Error("link.rate_kbps resolved to 0")
	}
	if len(cfg.GNSS.Ports) != 3 {
		t.Errorf("gnss.ports = %v, want 3 entries", cfg.GNSS.Ports)
	}
	if cfg.Telemetry.Interval <= 0 {
		t.Error("telemetry.interval did not resolve to a positive duration")
	}
	if cfg.GNSS.StaleAfter <= 0 {
		t.Error("gnss.stale_after did not resolve to a positive duration")
	}
	// require_hardware must actually arrive. Its default is false, so a nested
	// or misspelled key is indistinguishable from correct by value alone --
	// which is exactly why TestShippedConfigFileIsWellFormed checks the name.
	if !cfg.RequireHardware {
		t.Log("require_hardware resolved to false (the file's value; checked by name elsewhere)")
	}
}

func TestValidateRejectsImpossibleGNSSSelection(t *testing.T) {
	cfg := config.Defaults()

	// 1-based ID against a three-receiver bank.
	cfg.GNSS.Selected = 0
	if err := cfg.Validate(); err == nil {
		t.Error("Validate accepted receiver ID 0")
	}
	cfg.GNSS.Selected = 4
	if err := cfg.Validate(); err == nil {
		t.Error("Validate accepted receiver ID 4 for a three-receiver bank")
	}
	cfg.GNSS.Selected = 3
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate rejected receiver ID 3: %v", err)
	}
}

// An unknown key is ignored, not rejected. A newer Ground Station carrying a key
// this OBC predates must not stop it from starting; a version skew should not
// become an outage.
func TestUnknownKeyIsIgnoredNotRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rocsar.toml")
	body := "link_device_typo = 1\n[future]\nsome_new_key = true\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv(config.PathEnv, path)
	if _, err := config.Load(); err != nil {
		t.Fatalf("an unknown key must not fail Load: %v", err)
	}
}

// Every key must have a working ROCSAR_* form.
//
// ARCHITECTURE.md 11 mandates the resolution order
//
//	flag > ROCSAR_* environment > rocsar.toml > code default
//
// and README says every key has an environment form. Both were true on paper and
// false in fact: Apply handed the raw environment string to assign(), which
// type-asserts on the shapes go-toml produces, so every bool, every integer and
// the port list were rejected -- "expected a number, got string" -- and because
// Apply returns on the first error, one of them aborted startup. Seven of the
// nineteen keys were dead, including both of README's own examples
// (ROCSAR_REQUIRE_HARDWARE, ROCSAR_LINK_RATE_KBPS).
//
// The test is a case per key rather than a spot check, because the failure was
// per key: the three string keys that worked were the only reason it looked
// mostly fine.
func TestEveryKeyHasAWorkingEnvironmentForm(t *testing.T) {
	// One representative value per key, chosen so the value is distinguishable
	// from the code default -- a test that passes whether or not the key was
	// applied proves nothing.
	cases := []struct {
		key   string
		value string
	}{
		{"server.control_endpoint", "tcp://*:6001"},
		{"server.telemetry_endpoint", "tcp://*:6002"},
		{"client.control_endpoint", "tcp://10.0.0.9:6001"},
		{"client.telemetry_endpoint", "tcp://10.0.0.9:6002"},
		{"client.http_endpoint", "http://10.0.0.9:6003"},
		{"http.addr", ":6003"},
		{"http.root", "/tmp/rocsar-env-root"},
		{"http.device", "b82c5820-9183-45ba-bf2f-a956f6dec4ce"},
		{"link.device", "eth9"},
		{"link.rate_kbps", "256"},
		{"link.shaping", "true"},
		{"pico.port", "/dev/ttyACM9"},
		{"pico.baudrate", "57600"},
		{"gnss.ports", "[3001,3002,3003]"},
		{"gnss.selected", "2"},
		{"gnss.stale_after", "7s"},
		{"camera.device", "/dev/video9"},
		{"sdr.program", "/opt/sdr"},
		{"sdr.data_dir", "/opt/sdr-data"},
		{"telemetry.interval", "5s"},
		{"require_hardware", "true"},
	}

	seen := map[string]bool{}
	for _, tc := range cases {
		if !config.KnownKeys[tc.key] {
			t.Errorf("%s is tested here but is not in config.KnownKeys", tc.key)
			continue
		}
		seen[tc.key] = true
		t.Run(tc.key, func(t *testing.T) {
			t.Setenv(config.EnvName(tc.key), tc.value)

			cfg := config.Defaults()
			if err := cfg.Apply(); err != nil {
				t.Fatalf("%s=%q was rejected: %v", config.EnvName(tc.key), tc.value, err)
			}
			assertEnvApplied(t, cfg, tc.key, tc.value)
		})
	}

	// And the other direction: a key with no environment test is a key nobody
	// checked, which is how seven of them went missing in the first place.
	for key := range config.KnownKeys {
		if !seen[key] {
			t.Errorf("config.KnownKeys has %q but no environment case above", key)
		}
	}
}

// assertEnvApplied checks that the value actually landed, not merely that it was
// accepted. Accepting a value and discarding it is the other way this could pass.
func assertEnvApplied(t *testing.T, cfg config.Config, key, value string) {
	t.Helper()
	switch key {
	case "server.control_endpoint":
		if cfg.Server.ControlEndpoint != value {
			t.Errorf("got %q, want %q", cfg.Server.ControlEndpoint, value)
		}
	case "server.telemetry_endpoint":
		if cfg.Server.TelemetryEndpoint != value {
			t.Errorf("got %q, want %q", cfg.Server.TelemetryEndpoint, value)
		}
	case "client.control_endpoint":
		if cfg.Client.ControlEndpoint != value {
			t.Errorf("got %q, want %q", cfg.Client.ControlEndpoint, value)
		}
	case "client.telemetry_endpoint":
		if cfg.Client.TelemetryEndpoint != value {
			t.Errorf("got %q, want %q", cfg.Client.TelemetryEndpoint, value)
		}
	case "client.http_endpoint":
		if cfg.Client.HTTPEndpoint != value {
			t.Errorf("got %q, want %q", cfg.Client.HTTPEndpoint, value)
		}
	case "http.addr":
		if cfg.HTTP.Addr != value {
			t.Errorf("got %q, want %q", cfg.HTTP.Addr, value)
		}
	case "http.root":
		if cfg.HTTP.Root != value {
			t.Errorf("got %q, want %q", cfg.HTTP.Root, value)
		}
	case "http.device":
		if cfg.HTTP.Device != value {
			t.Errorf("got %q, want %q", cfg.HTTP.Device, value)
		}
	case "link.device":
		if cfg.Link.Device != value {
			t.Errorf("got %q, want %q", cfg.Link.Device, value)
		}
	case "link.rate_kbps":
		if cfg.Link.RateKbps != 256 {
			t.Errorf("got %d, want 256", cfg.Link.RateKbps)
		}
	case "link.shaping":
		if !cfg.Link.Shaping {
			t.Error("shaping is false, want true")
		}
	case "pico.port":
		if cfg.Pico.Port != value {
			t.Errorf("got %q, want %q", cfg.Pico.Port, value)
		}
	case "pico.baudrate":
		if cfg.Pico.Baudrate != 57600 {
			t.Errorf("got %d, want 57600", cfg.Pico.Baudrate)
		}
	case "gnss.ports":
		if len(cfg.GNSS.Ports) != 3 || cfg.GNSS.Ports[0] != 3001 || cfg.GNSS.Ports[2] != 3003 {
			t.Errorf("got %v, want [3001 3002 3003]", cfg.GNSS.Ports)
		}
	case "gnss.selected":
		if cfg.GNSS.Selected != 2 {
			t.Errorf("got %d, want 2", cfg.GNSS.Selected)
		}
	case "gnss.stale_after":
		if cfg.GNSS.StaleAfter.String() != value {
			t.Errorf("got %s, want %s", cfg.GNSS.StaleAfter, value)
		}
	case "camera.device":
		if cfg.Camera.Device != value {
			t.Errorf("got %q, want %q", cfg.Camera.Device, value)
		}
	case "sdr.program":
		if cfg.SDR.Program != value {
			t.Errorf("got %q, want %q", cfg.SDR.Program, value)
		}
	case "sdr.data_dir":
		if cfg.SDR.DataDir != value {
			t.Errorf("got %q, want %q", cfg.SDR.DataDir, value)
		}
	case "telemetry.interval":
		if cfg.Telemetry.Interval.String() != value {
			t.Errorf("got %s, want %s", cfg.Telemetry.Interval, value)
		}
	case "require_hardware":
		if !cfg.RequireHardware {
			t.Error("require_hardware is false, want true")
		}
	default:
		t.Fatalf("assertEnvApplied has no case for %q", key)
	}
}

// A value the key cannot hold is still an error. The fix routes environment
// values through the TOML parser; it must not have turned a typo into a value.
func TestEnvironmentValuesAreStillChecked(t *testing.T) {
	for _, tc := range []struct{ key, value, want string }{
		{"link.rate_kbps", "not-a-number", "number"},
		{"link.shaping", "yes-please", "true or false"},
		{"pico.baudrate", "-1", "number"},
		{"gnss.ports", "2001", "list"},
		{"gnss.selected", "0", "Validate"},
		{"require_hardware", "perhaps", "true or false"},
		{"gnss.stale_after", "soon", "duration"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			t.Setenv(config.EnvName(tc.key), tc.value)
			cfg := config.Defaults()
			applyErr := cfg.Apply()
			if applyErr != nil && tc.want == "Validate" {
				// gnss.selected only becomes wrong at validation time.
				if err := cfg.Validate(); err == nil {
					t.Errorf("%s=%q passed validation", tc.key, tc.value)
				}
				return
			}
			if applyErr == nil {
				// Accepted by Apply; must then fail Validate if the test says so.
				if tc.want == "Validate" {
					if err := cfg.Validate(); err == nil {
						t.Errorf("%s=%q passed validation", tc.key, tc.value)
					}
					return
				}
				t.Fatalf("%s=%q was accepted", tc.key, tc.value)
			}
			if !strings.Contains(strings.ToLower(applyErr.Error()), strings.ToLower(tc.want)) {
				t.Errorf("%s=%q: error %q does not mention %q", tc.key, tc.value, applyErr, tc.want)
			}
		})
	}
}

// The environment must still beat the file, and lose to a flag. This is the order
// ARCHITECTURE.md 11 mandates; applyEnv is new code on that path.
func TestEnvironmentBeatsTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rocsar.toml")
	if err := os.WriteFile(path, []byte("[link]\nrate_kbps = 111\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.PathEnv, path)
	t.Setenv(config.EnvName("link.rate_kbps"), "222")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Link.RateKbps != 111 {
		t.Fatalf("file layer: got %d, want 111", cfg.Link.RateKbps)
	}
	if err := cfg.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if cfg.Link.RateKbps != 222 {
		t.Errorf("environment layer: got %d, want 222", cfg.Link.RateKbps)
	}
}

// The Ground Station must READ [client], not merely parse it.
//
// This section was written for the console and for a long time nothing consumed
// it: cmd/gs took all three endpoints from flags whose defaults were the aircraft
// address, so rocsar.toml was decorative and an operator who changed the Pi's
// address had to retyping it into a flag on every session. The keys were in
// KnownKeys the whole time, which is what made it invisible -- a key that parses
// cleanly and is never read is indistinguishable from one that works.
//
// The test cannot see cmd/gs's behaviour directly, so it asserts the thing that
// made it possible: every [client] key has a code default, which is what the
// console falls through to when no flag is given. A flag with a real default
// cannot be told from a typed one, so the flag always wins and the file is never
// reached; the defaults here are what make the file win instead.
func TestTheClientSectionHasDefaultsForTheConsoleToFallThroughTo(t *testing.T) {
	for _, k := range []string{
		"client.control_endpoint", "client.telemetry_endpoint", "client.http_endpoint",
	} {
		if !config.KnownKeys[k] {
			t.Errorf("%s is not in KnownKeys, so rocsar.toml would refuse it and the "+
				"console would fall back to a flag default", k)
		}
	}

	c := config.Defaults()
	for name, got := range map[string]string{
		"control":   c.Client.ControlEndpoint,
		"telemetry": c.Client.TelemetryEndpoint,
		"http":      c.Client.HTTPEndpoint,
	} {
		if got == "" {
			t.Errorf("the %s client endpoint has no code default", name)
		}
	}

	// And the artefact endpoint specifically. It was absent while the other two
	// were present, so the third link -- the one whose address moves with the
	// deployment -- was the one with no file home.
	if c.Client.HTTPEndpoint == "" || !strings.HasPrefix(c.Client.HTTPEndpoint, "http") {
		t.Errorf("client.http_endpoint default = %q, want an http:// URL", c.Client.HTTPEndpoint)
	}
}

// The shipped rocsar.toml must carry all three [client] keys.
//
// Defaults exist so the console starts with no file at all; the file exists so an
// operator changes the aircraft's address once. A shipped file missing one of them
// leaves that endpoint on the localhost default while its two siblings point at
// the aircraft, which is the confusing half-configured state.
func TestTheShippedTomlCarriesEveryClientEndpoint(t *testing.T) {
	// config.Load resolves against the working directory, which is test/, so the
	// shipped file is named explicitly -- the same way TestShippedConfigFileIsWellFormed
	// reads it. Pointed at the file rather than at its directory, because the
	// loader would otherwise merge a developer's own rocsar.local.toml into the
	// assertion about what ships.
	t.Setenv(config.PathEnv, filepath.Join("..", "rocsar.toml"))
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load the shipped rocsar.toml: %v", err)
	}
	// Defaults are localhost; the shipped file aims at the aircraft.
	for name, got := range map[string]string{
		"control":   cfg.Client.ControlEndpoint,
		"telemetry": cfg.Client.TelemetryEndpoint,
		"http":      cfg.Client.HTTPEndpoint,
	} {
		if strings.Contains(got, "127.0.0.1") {
			t.Errorf("the shipped rocsar.toml leaves [client] %s on its localhost default (%q); "+
				"the console will dial nothing", name, got)
		}
	}
}

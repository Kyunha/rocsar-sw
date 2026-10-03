package test

import (
	"os"
	"path/filepath"
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

// The HTTP port and the tc flower filter port are one fact in two places. A
// mismatch fails silently at run time, so it is refused at startup.
func TestHTTPPortMustMatchFlowerFilterPort(t *testing.T) {
	cfg := config.Defaults()

	cfg.HTTP.Addr = ":9999"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate accepted an http.addr port that does not match the flower filter port")
	}

	cfg.HTTP.Addr = ":5557"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate rejected the matching port: %v", err)
	}

	// With shaping off there is no filter to disagree with, so the port is free.
	cfg.HTTP.Addr = ":9999"
	cfg.Link.Shaping = false
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate rejected a free port with shaping disabled: %v", err)
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

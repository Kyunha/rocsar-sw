// Package config resolves settings from four sources in a fixed order.
//
//	explicit flag  >  ROCSAR_* environment  >  rocsar.toml  >  code default
//
// The order exists so that the same binary behaves the same way on an operator
// laptop, in a test, and on the aircraft, and so that any one of the four can be
// changed without touching the others.
//
// Two rules that are load-bearing and easy to undo by accident:
//
//  1. Every flag is registered with a nil default. If a flag carries its own
//     default, the parser cannot distinguish "the user asked for this" from
//     "nobody said anything", and it will overwrite the value in the file.
//     Resolution happens here, never in flag's own machinery.
//
//  2. Defaults live in the --help text, not in rocsar.toml. A default copied
//     into the file becomes a second home for a fact, and two homes drift.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The link rate and the GNSS port block are named rather than written inline
// because ARCHITECTURE.md makes them load-bearing in more than one place.
//
// gnssPortBase in particular: the three receivers are gnssPortBase..+2 and the
// telemetry topic prefixes are derived from them, so a change to the base has to
// move the topics with it. Written as bare literals three times, that coupling
// is invisible until a receiver's data arrives under a topic nobody subscribed
// to.
//
// They are DEFAULTS, not constants -- config precedence means rocsar.toml or the
// environment can override every one of them, and none of these is a second home
// for a fact.
const (
	defaultLinkRateKbps = 115 // the whole point of the project: 115 kbit/s, not WiFi
	picoBaudrate        = 115200
	gnssPortBase        = 2001
)

// EnvPrefix is prepended to a key to form its environment variable name:
// [link] rate_kbps becomes ROCSAR_LINK_RATE_KBPS.
const EnvPrefix = "ROCSAR_"

// Filename is the committed, shared configuration file.
const Filename = "rocsar.toml"

// LocalFilename is merged over Filename and is gitignored. It exists so a
// machine-specific override -- a different Pi address, a different data
// directory -- does not become a commit that fights the next person.
const LocalFilename = "rocsar.local.toml"

// PathEnv overrides where the configuration files are looked for.
const PathEnv = "ROCSAR_CONFIG"

// Config is the resolved configuration.
type Config struct {
	Server struct {
		ControlEndpoint   string
		TelemetryEndpoint string
	}
	Client struct {
		ControlEndpoint   string
		TelemetryEndpoint string
	}
	HTTP struct {
		Addr string
		Root string
	}
	Link struct {
		Device   string
		RateKbps uint32
		Shaping  bool
	}
	Pico struct {
		Port     string
		Baudrate int
	}
	GNSS struct {
		Ports    []int
		Selected int
		// StaleAfter is how old a fix may be before FixOK goes false.
		StaleAfter time.Duration
	}
	Camera struct {
		Device string
	}
	SDR struct {
		Program string
	}
	Telemetry struct {
		Interval time.Duration
	}
	QOS struct {
		BulkRateBps int
	}

	// Mocked names the subsystems running against a fake. It is surfaced in
	// telemetry rather than only logged: a system that fabricated a reading
	// because a device was not found is the worst failure mode this project
	// has, and hiding it in a log the operator may not have open does not
	// count as disclosure.
	Mocked map[string]bool

	// RequireHardware inverts the degrade policy. Missing hardware becomes a
	// startup failure instead of a degraded report. The default is to degrade,
	// because a telemetry server that is down tells the operator nothing; this
	// flag exists for benches and CI where failing loudly is preferred.
	RequireHardware bool
}

// KnownKeys is the set of recognised configuration keys.
//
// A key that is neither here nor under a known section is IGNORED, not an
// error. That is what lets a newer Ground Station talk to an older OBC: the GS
// carries a key the OBC has never heard of, and refusing to start would turn a
// version skew into an outage.
var KnownKeys = map[string]bool{
	"server.control_endpoint":   true,
	"server.telemetry_endpoint": true,
	"client.control_endpoint":   true,
	"client.telemetry_endpoint": true,
	"http.addr":                 true,
	"http.root":                 true,
	"link.device":               true,
	"link.rate_kbps":            true,
	"link.shaping":              true,
	"pico.port":                 true,
	"pico.baudrate":             true,
	"gnss.ports":                true,
	"gnss.selected":             true,
	"gnss.stale_after":          true,
	"camera.device":             true,
	"sdr.program":               true,
	"telemetry.interval":        true,
	"qos.bulk_rate_bps":         true,
	"require_hardware":          true,
}

// Defaults returns the configuration with every code default applied and
// nothing else. This is the bottom of the resolution stack.
func Defaults() Config {
	var c Config

	c.Server.ControlEndpoint = "tcp://*:5555"
	c.Server.TelemetryEndpoint = "tcp://*:5556"
	c.Client.ControlEndpoint = "tcp://127.0.0.1:5555"
	c.Client.TelemetryEndpoint = "tcp://127.0.0.1:5556"
	c.HTTP.Addr = ":5557"
	c.HTTP.Root = "/mnt/rocsar/data"
	c.Link.Device = "eth0"
	c.Link.RateKbps = defaultLinkRateKbps
	c.Link.Shaping = false
	c.Pico.Port = "/dev/ttyACM0"
	c.Pico.Baudrate = picoBaudrate
	c.GNSS.Ports = []int{gnssPortBase, gnssPortBase + 1, gnssPortBase + 2}
	c.GNSS.Selected = 1
	c.GNSS.StaleAfter = 2 * time.Second
	c.Camera.Device = "/dev/video0"
	c.SDR.Program = "third_party/sdr-ettus-b200mini"
	c.Telemetry.Interval = time.Second
	// 8 KiB/s, about 64 kbit/s: a little over half the 115 kbit/s radio, so
	// telemetry and commands have room without claiming precision the link lacks.
	//
	// The previous default was 64 KiB/s -- 512 kbit/s, more than four times the
	// link. It read as a constraint and constrained nothing.
	c.QOS.BulkRateBps = 8 * 1024
	c.Mocked = map[string]bool{}

	return c
}

// Load reads the configuration files and returns the result with code defaults
// applied. Flags and the environment are applied afterwards by Apply, so that
// this function is a pure function of what is on disk -- which is what makes it
// testable without a process.
//
// A missing file is not an error. rocsar.toml is optional: every value in it has
// a default, and a fresh checkout should run.
func Load() (Config, error) {
	c := Defaults()

	base, err := findBase()
	if err != nil {
		return c, err
	}

	if base != "" {
		vals, err := parseFile(base)
		if err != nil {
			return c, fmt.Errorf("read %s: %w", base, err)
		}
		if err := c.applyMap(vals); err != nil {
			return c, fmt.Errorf("%s: %w", base, err)
		}
	}

	local := filepath.Join(filepath.Dir(base), LocalFilename)
	if _, err := os.Stat(local); err == nil {
		vals, err := parseFile(local)
		if err != nil {
			return c, fmt.Errorf("read %s: %w", local, err)
		}
		if err := c.applyMap(vals); err != nil {
			return c, fmt.Errorf("%s: %w", local, err)
		}
	}

	return c, nil
}

// findBase locates rocsar.toml: $ROCSAR_CONFIG if set, else the working
// directory.
func findBase() (string, error) {
	if p := os.Getenv(PathEnv); p != "" {
		return p, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Join(wd, Filename), nil
}

// Apply layers the environment over the file-derived configuration. Call it
// after Load and before applying flags, which are the highest priority.
func (c *Config) Apply() error {
	keys := make([]string, 0, len(KnownKeys))
	for key := range KnownKeys {
		keys = append(keys, key)
	}
	// Sorted so that a shell with two bad variables always reports the same one
	// first. Map order is random, and a startup error that names a different key
	// on each run is maddening to diagnose.
	sort.Strings(keys)

	for _, key := range keys {
		env, ok := os.LookupEnv(EnvName(key))
		if !ok {
			continue
		}
		if err := c.applyEnv(key, env); err != nil {
			return fmt.Errorf("%s: %w", EnvName(key), err)
		}
	}
	return nil
}

// applyEnv layers one environment variable over the configuration.
//
// The value is handed to the TOML parser rather than converted here, so the
// environment and rocsar.toml pass through exactly one parser and cannot come to
// disagree about what a value means. That is not tidiness; it was a working
// outage.
//
// This used to hand the raw string straight to assign(), which type-asserts on
// the shapes go-toml produces. Every non-string key was therefore rejected --
// "expected a number, got string", "expected true or false, got string" -- and
// since Apply returned on the first error, setting one of them aborted startup.
// Seven of the nineteen keys were affected: both link settings, the pico baud,
// the GNSS port list, the GNSS selection, the QoS rate, and require_hardware.
// README's two worked examples were both of them.
//
// Two forms are tried, because an environment variable is a bare token and TOML
// is not: the value as written, then the value quoted. "115" and "true" and
// "[2001,2002,2003]" are TOML in the first form; "tcp://*:5555" and "2s" and
// "/dev/ttyACM0" are only TOML in the second. Whichever the key accepts wins, so
// applyEnv never has to know a key's type -- assign already does.
func (c *Config) applyEnv(key, env string) error {
	bare, bareErr := parseTOML(key + " = " + env)
	if bareErr == nil {
		if raw, ok := bare[key]; ok {
			if err := c.assign(key, raw); err == nil {
				return nil
			}
		}
	}

	quoted, err := parseTOML(key + " = " + strconv.Quote(env))
	if err != nil {
		// The bare form's complaint is the informative one: it is what the
		// operator wrote, and for the keys that are not strings it names the
		// type they wanted.
		return bareErr
	}
	raw, ok := quoted[key]
	if !ok {
		return bareErr
	}
	return c.assign(key, raw)
}

// EnvName maps a dotted configuration key to its environment variable.
func EnvName(key string) string {
	return EnvPrefix + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
}

// UnknownKeys returns the recognised-looking keys present in the file that are
// not in KnownKeys. They are ignored, but surfacing them at startup turns a
// silent typo into a visible one.
func UnknownKeys(vals map[string]any) []string {
	var out []string
	for k := range vals {
		if !KnownKeys[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

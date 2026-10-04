package config

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// A real TOML parser rather than a hand-rolled one for the subset we use.
//
// The subset looks small enough to hand-roll, and that is exactly the trap: a
// parser that quietly accepts `rate_kbps = 115kbps` as the string "115kbps", or
// reads past a `#` comment in the wrong place, is a parser that hands a
// telemetry server a configuration nobody wrote. go-toml reports the line
// number of a malformed value, which turns a silent misconfiguration into a
// startup message.

// parseFile returns every leaf as a dotted key. Sections become prefixes.
func parseFile(path string) (map[string]any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var doc map[string]any
	if err := toml.NewDecoder(f).Decode(&doc); err != nil {
		return nil, err
	}
	return flatten("", doc), nil
}

func flatten(prefix string, in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		if sub, ok := v.(map[string]any); ok {
			for k2, v2 := range flatten(key, sub) {
				out[k2] = v2
			}
			continue
		}
		out[key] = v
	}
	return out
}

// applyMap layers a set of dotted keys over the configuration.
func (c *Config) applyMap(vals map[string]any) error {
	// Sorted so that a file with two bad values always reports the same one
	// first. Map iteration order is random, and a startup error that names a
	// different key on each run is maddening to diagnose.
	for _, k := range sortedKeys(vals) {
		if err := c.set(k, vals[k]); err != nil {
			return err
		}
	}
	return nil
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// set assigns one dotted key.
//
// An unrecognised key is ignored rather than rejected, so that a Ground Station
// carrying a configuration this OBC predates does not prevent it from starting.
// A version skew should not become an outage.
func (c *Config) set(key string, raw any) error {
	if !KnownKeys[key] {
		return nil
	}
	return c.assign(key, raw)
}

func (c *Config) assign(key string, raw any) error {
	str := func() (string, error) {
		s, ok := raw.(string)
		if !ok {
			return "", fmt.Errorf("expected a string, got %T", raw)
		}
		return s, nil
	}
	boolean := func() (bool, error) {
		b, ok := raw.(bool)
		if !ok {
			return false, fmt.Errorf("expected true or false, got %T", raw)
		}
		return b, nil
	}
	integer := func() (uint32, error) {
		switch v := raw.(type) {
		case int64:
			if v < 0 {
				return 0, fmt.Errorf("must not be negative, got %d", v)
			}
			return uint32(v), nil
		case float64:
			if v < 0 || v != float64(int64(v)) {
				return 0, fmt.Errorf("expected a non-negative whole number, got %v", v)
			}
			return uint32(v), nil
		default:
			return 0, fmt.Errorf("expected a number, got %T", raw)
		}
	}
	duration := func() (time.Duration, error) {
		s, err := str()
		if err != nil {
			return 0, err
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("expected a duration like \"2s\", got %q", s)
		}
		return d, nil
	}
	portList := func() ([]int, error) {
		arr, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("expected a list of ports, got %T", raw)
		}
		out := make([]int, 0, len(arr))
		for _, e := range arr {
			n, ok := e.(int64)
			if !ok {
				return nil, fmt.Errorf("expected a port number, got %T", e)
			}
			if n < 1 || n > math.MaxUint16 {
				return nil, fmt.Errorf("port %d out of range", n)
			}
			out = append(out, int(n))
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("at least one GNSS port is required")
		}
		return out, nil
	}

	var err error
	switch key {
	case "server.control_endpoint":
		c.Server.ControlEndpoint, err = str()
	case "server.telemetry_endpoint":
		c.Server.TelemetryEndpoint, err = str()
	case "client.control_endpoint":
		c.Client.ControlEndpoint, err = str()
	case "client.telemetry_endpoint":
		c.Client.TelemetryEndpoint, err = str()

	case "http.addr":
		c.HTTP.Addr, err = str()
	case "http.root":
		c.HTTP.Root, err = str()

	case "link.device":
		c.Link.Device, err = str()
	case "link.rate_kbps":
		c.Link.RateKbps, err = integer()
	case "link.shaping":
		c.Link.Shaping, err = boolean()

	case "pico.port":
		c.Pico.Port, err = str()
	case "pico.baudrate":
		var n uint32
		n, err = integer()
		c.Pico.Baudrate = int(n)

	case "gnss.ports":
		c.GNSS.Ports, err = portList()
	case "gnss.selected":
		var n uint32
		n, err = integer()
		c.GNSS.Selected = int(n)
	case "gnss.stale_after":
		c.GNSS.StaleAfter, err = duration()

	case "camera.device":
		c.Camera.Device, err = str()
	case "sdr.program":
		c.SDR.Program, err = str()
	case "telemetry.interval":
		c.Telemetry.Interval, err = duration()
	case "qos.bulk_rate_bps":
		var n uint32
		n, err = integer()
		c.QOS.BulkRateBps = int(n)
	case "require_hardware":
		c.RequireHardware, err = boolean()

	default:
		// Unreachable: set() filters on KnownKeys first.
		return fmt.Errorf("unhandled key %q", key)
	}

	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	return nil
}

// Validate checks the combinations that are individually well-formed but
// jointly wrong. These are the mistakes that are easy to make in a TOML edit and
// expensive to find in flight.
func (c *Config) Validate() error {
	if c.Link.RateKbps == 0 {
		return fmt.Errorf("link.rate_kbps must be greater than zero")
	}
	if c.Telemetry.Interval <= 0 {
		return fmt.Errorf("telemetry.interval must be greater than zero")
	}
	if len(c.GNSS.Ports) == 0 {
		return fmt.Errorf("gnss.ports must list at least one port")
	}

	// gnss.selected is a receiver ID -- the 1-based position in gnss.ports --
	// not a port number. Comparing it against the port list, which is what this
	// did at first, rejects every valid configuration: ID 1 against ports
	// [2001 2002 2003] never matches.
	if c.GNSS.Selected < 1 || c.GNSS.Selected > len(c.GNSS.Ports) {
		return fmt.Errorf("gnss.selected is %d, but gnss.ports lists %d receiver(s) (valid IDs are 1..%d)",
			c.GNSS.Selected, len(c.GNSS.Ports), len(c.GNSS.Ports))
	}

	// The HTTP port and the tc flower filter are the same fact in two places.
	// They are configured independently -- one in the HTTP server, one in the
	// kernel -- and if they disagree the failure is silent: downloads fall into
	// the priority class, the priority class starves, and nothing reports an
	// error. See ARCHITECTURE.md 11.
	if port, ok := portOf(c.HTTP.Addr); ok && c.Link.Shaping {
		if port != BulkPort {
			return fmt.Errorf(
				"http.addr port %d does not match the traffic shaper's flower filter port %d; "+
					"bulk traffic would silently join the priority class and starve the control channel",
				port, BulkPort)
		}
	}

	return nil
}

// BulkPort is the TCP port the kernel flower filter classifies bulk traffic on.
//
// It is a constant and not a configuration key precisely because it must agree
// with [http] addr, and two independent homes for one fact is how they stop
// agreeing. Changing it means changing the filter.
const BulkPort = 5557

// portOf extracts the port from a listen address such as ":5557" or "0.0.0.0:5557".
func portOf(addr string) (int, bool) {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return 0, false
	}
	n, err := strconv.Atoi(addr[i+1:])
	if err != nil {
		return 0, false
	}
	return n, true
}

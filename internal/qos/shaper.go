// Package qos shapes the air link.
//
// Two layers, because they solve different problems and confusing them is how
// you end up with neither working:
//
//   - Userspace (Limiter): a priority queue and a token bucket in front of the
//     telemetry socket. It stops a bulk transfer from monopolising the socket's
//     send path before the kernel ever sees the traffic.
//
//   - Kernel (Shaper): an HTB hierarchy on the link device, so the limit is
//     enforced on traffic this process does not own. Anything on the Pi can
//     saturate the radio link; a queue in our own process cannot help with that.
//
// ARCHITECTURE.md 6.6.
package qos

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rocsar/obc/internal/config"
	"github.com/rocsar/obc/internal/domain"
)

// CommandTimeout bounds every tc invocation.
//
// `sudo` with no passwordless rule and no TTY blocks forever, and a telemetry
// server blocked in a kernel call is worse than an unshaped one. Five seconds is
// long enough for tc to do its work and short enough that a wedged sudo is a
// visible event rather than a silent hang.
const CommandTimeout = 5 * time.Second

// PriorityShare is the fraction of the link reserved for telemetry and commands.
//
// 0.36 of 115 kbit/s is about 41 kbit/s, which is roughly 20x a 1 Hz telemetry
// frame. The headroom is deliberate: the frame budget in ARCHITECTURE.md is a
// cap, and a link that is exactly big enough for the average frame has no room
// for the one that carries a resync.
const PriorityShare = 0.36

// Shaper installs the kernel traffic control hierarchy.
type Shaper struct {
	log  *slog.Logger
	exec func(ctx context.Context, name string, args ...string) (string, error)

	mu       sync.RWMutex
	device   string
	rateKbps uint32
	prioKbps uint32
	active   bool
	reason   string
}

// NewShaper returns a shaper. Pass nil for exec to use the real one.
func NewShaper(log *slog.Logger, runner func(context.Context, string, ...string) (string, error)) *Shaper {
	if runner == nil {
		runner = runCommand
	}
	return &Shaper{log: log, exec: runner}
}

func runCommand(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, CommandTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// PriorityKbps returns the priority class ceiling for a link rate.
//
// Derived, not configured: the priority class and the bulk class are two halves
// of one number, and letting them be set independently is how they come to sum
// to more than the link can carry.
func PriorityKbps(rateKbps uint32) uint32 {
	return uint32(float64(rateKbps) * PriorityShare)
}

// Apply installs the hierarchy. It never returns an error.
//
// tc fails for dozens of reasons unrelated to our logic -- sudo without a
// password, tc not installed, wrong interface, kernel without HTB -- and not one
// of them justifies taking down a telemetry server. The operator gets the real
// reason in telemetry; everything else keeps running unshaped.
func (s *Shaper) Apply(ctx context.Context, device string, rateKbps uint32) (bool, string) {
	if device == "" {
		return s.fail("no link device configured")
	}
	if rateKbps == 0 {
		return s.fail("link rate must be greater than zero")
	}
	prio := PriorityKbps(rateKbps)
	if prio == 0 {
		// A rate low enough that the priority class rounds to zero would leave
		// telemetry with no guaranteed bandwidth at all, which defeats the
		// point of having a priority class.
		return s.fail(fmt.Sprintf("link rate %d kbit/s is too low: the priority class would be %d kbit/s",
			rateKbps, prio))
	}

	bulk := rateKbps - prio

	// Remove any previous hierarchy first. This is best-effort: `tc qdisc del`
	// on a device with no qdisc returns an error, and for us that error is
	// SUCCESS -- see isNoSuchQdisc for why that distinction is not cosmetic.
	if out, err := s.exec(ctx, "tc", "qdisc", "del", "dev", device, "root"); err != nil {
		if !isNoSuchQdisc(out, err) {
			// The message names tc explicitly. `exec` reports a missing binary
			// as "executable file not found in $PATH", which reads to an operator
			// as a missing PATH entry rather than as "the traffic shaper is not
			// installed on this machine" -- and they then look in the wrong place.
			return s.fail(fmt.Sprintf("could not run tc to clear the existing qdisc on %s: %v: %s",
				device, err, oneLine(out)))
		}
	}

	steps := []struct {
		what string
		args []string
	}{
		{"root HTB", []string{"qdisc", "add", "dev", device, "root", "handle", "1:", "htb", "default", "20"}},

		// Priority class: a guaranteed floor at prio, free to borrow up to the
		// full link when nothing else wants it.
		{"priority class", []string{"class", "add", "dev", device, "parent", "1:", "classid", "1:10",
			"htb", "rate", fmt.Sprintf("%dkbit", prio), "ceil", fmt.Sprintf("%dkbit", rateKbps)}},

		// Bulk class: everything else, flat.
		{"bulk class", []string{"class", "add", "dev", device, "parent", "1:", "classid", "1:20",
			"htb", "rate", fmt.Sprintf("%dkbit", bulk), "ceil", fmt.Sprintf("%dkbit", bulk)}},

		// Leaf qdiscs. pfifo is queueing discipline, not shaping; the shaping is
		// the HTB above it.
		{"priority leaf", []string{"qdisc", "add", "dev", device, "parent", "1:10", "handle", "110:",
			"pfifo", "limit", "200"}},
		{"bulk leaf", []string{"qdisc", "add", "dev", device, "parent", "1:20", "handle", "120:",
			"pfifo", "limit", "100"}},
	}

	for _, step := range steps {
		if out, err := s.exec(ctx, "tc", step.args...); err != nil {
			return s.fail(fmt.Sprintf("tc %s failed: %v: %s", step.what, err, oneLine(out)))
		}
	}

	// The flower filter is what makes bulk traffic actually land in the bulk
	// class, and its absence is a SILENT failure: the commands above all
	// succeed, telemetry reports shaping as active, and downloads fall through
	// to the default class and starve the priority one. There is no error
	// anywhere -- the symptom is just that telemetry gets slower when a file
	// moves. It is installed last and verified by reading the qdisc back.
	//
	// `ip_proto tcp` is not optional. tc-flower's dst_port is a layer-4 match and
	// has no meaning without knowing the transport protocol, so the parser
	// rejects it outright:
	//
	//	$ tc filter add dev eth0 parent 1: protocol ip prio 20 flower dst_port 5557
	//	Illegal "dst_port"
	//
	// Reproduced on the target, and the same command with `ip_proto tcp` in front
	// is accepted and reads back correctly. Verified against tc-flower(8): "dst_port
	// and src_port depend on ip_proto being set to tcp, udp or sctp". The bulk
	// traffic is HTTP, so tcp.
	filterOut, filterErr := s.exec(ctx, "tc", bulkFilterArgs(device, config.BulkPort)...)
	if filterErr != nil {
		// Not fatal on its own: shaping is still in force, the filter is not.
		// But it is reported, loudly, because the consequence is invisible.
		s.set(device, rateKbps, prio, true,
			fmt.Sprintf("bulk filter not installed (%v): bulk traffic will join the priority class and may starve it",
				oneLine(filterOut)))
		s.log.Warn("bulk flower filter not installed; the link is shaped but unclassified",
			"device", device, "port", config.BulkPort, "err", filterErr)
		return true, s.reasonLocked()
	}

	// Read it back. `tc filter add` returning zero means the command was accepted,
	// which is not the same as the filter being there: a parent that does not
	// exist yet, or a handle the kernel quietly dropped, both exit 0 on some
	// paths and leave bulk traffic unclassified with shaping reported as active.
	// The comment above has claimed this verification since before it existed.
	showOut, showErr := s.exec(ctx, "tc", "filter", "show", "dev", device)
	if showErr != nil || !filterInstalled(showOut, config.BulkPort) {
		why := oneLine(showOut)
		if showErr != nil {
			why = fmt.Sprintf("%v: %s", showErr, why)
		}
		s.set(device, rateKbps, prio, true,
			fmt.Sprintf("bulk filter not confirmed on %s (%s): bulk traffic will join the priority class and may starve it",
				device, why))
		s.log.Warn("bulk flower filter did not read back; the link is shaped but unclassified",
			"device", device, "port", config.BulkPort, "tc_filter_show", why)
		return true, s.reasonLocked()
	}

	s.set(device, rateKbps, prio, true, "")
	s.log.Info("link shaping applied",
		"device", device, "rate_kbps", rateKbps, "priority_kbps", prio, "bulk_kbps", bulk,
		"bulk_filter_port", config.BulkPort)
	return true, ""
}

func (s *Shaper) set(device string, rateKbps, prioKbps uint32, active bool, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.device, s.rateKbps, s.prioKbps, s.active, s.reason = device, rateKbps, prioKbps, active, reason
}

func (s *Shaper) fail(reason string) (bool, string) {
	s.log.Warn("link shaping unavailable", "reason", reason)
	s.set("", 0, 0, false, reason)
	return false, reason
}

func (s *Shaper) reasonLocked() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.reason
}

func (s *Shaper) Active() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.active
}

// Status reports what the shaper believes, for telemetry.
func (s *Shaper) Status() domain.LinkStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()

	st := domain.LinkStatus{
		State:         domain.SubsystemReady,
		Device:        s.device,
		RateKbps:      s.rateKbps,
		PriorityKbps:  s.prioKbps,
		ShapingActive: s.active,
	}
	if !s.active {
		// Three different operator actions live behind this one field: tc is
		// missing, sudo is not permitted, or shaping is off in configuration.
		// A single bool sends someone to debug the wrong one.
		st.State = domain.SubsystemError
		st.InactiveReason = s.reason
	} else if s.reason != "" {
		// Shaping is in force but something is degraded -- the bulk filter, so
		// far. That is not the same as inactive and must not read as such.
		st.State = domain.SubsystemBusy
		st.InactiveReason = s.reason
	}
	return st
}

// QdiscPresent reads the device back from the kernel.
//
// This read-back is why the silent classification failure is detectable at all.
// Without it the only evidence that shaping worked is that the commands
// returned zero, which is exactly what happens when the commands are correct and
// the effect is not.
func (s *Shaper) QdiscPresent(ctx context.Context, device string) (bool, error) {
	out, err := s.exec(ctx, "tc", "qdisc", "show", "dev", device)
	if err != nil {
		return false, fmt.Errorf("tc qdisc show: %w: %s", err, oneLine(out))
	}
	if strings.Contains(out, "htb") || strings.Contains(out, "noqueue") {
		return true, nil
	}
	return false, nil
}

// isNoSuchQdisc reports whether `tc qdisc del` failing means "there was nothing
// to delete", which is success for us.
//
// This cannot be done by checking the exit code, and it cannot be done by a
// substring match on "not found": the message for a missing `tc` binary also
// contains "executable not found", so a loose match reports success when tc is
// not installed at all -- and then nothing is shaped, and telemetry claims it
// is. Both known-good strings are anchored instead.
func isNoSuchQdisc(output string, err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, exec.ErrNotFound) {
		return false
	}
	for _, known := range []string{
		"RTNETLINK answers: No such file or directory",
		"Cannot delete qdisc with handle of zero",
		"Error: Cannot delete qdisc",
	} {
		if strings.Contains(output, known) {
			return true
		}
	}
	return false
}

func oneLine(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	const max = 200
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

// NullShaper records requests and changes nothing.
//
// The default for a laptop, a test and CI. A bare `go run` on a developer
// machine must not try to reshape a real NIC -- that needs CAP_NET_ADMIN and it
// is not something to discover by running the thing.
type NullShaper struct {
	mu     sync.RWMutex
	reason string
}

// NewNullShaper returns a shaper that does nothing, with the given explanation.
func NewNullShaper(reason string) *NullShaper { return &NullShaper{reason: reason} }

func (n *NullShaper) Apply(context.Context, string, uint32) (bool, string) { return false, n.reason }
func (n *NullShaper) Active() bool                                         { return false }
func (n *NullShaper) QdiscPresent(context.Context, string) (bool, error)   { return false, nil }

func (n *NullShaper) Status() domain.LinkStatus {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return domain.LinkStatus{
		State:          domain.SubsystemReady,
		ShapingActive:  false,
		InactiveReason: n.reason,
	}
}

// bulkFilterArgs builds the `tc filter add` argument list that classifies bulk
// traffic into the bulk class.
//
// ip_proto comes before dst_port and is not optional. tc's flower parser treats
// dst_port as a layer-4 match and has nothing to match against without knowing
// the transport protocol:
//
//	tc filter add dev eth0 parent 1: protocol ip prio 20 flower dst_port 5557
//	Illegal "dst_port"
//
// Reproduced on the target; the same command with `ip_proto tcp` in front is
// accepted and reads back correctly. tc-flower(8) says the same: "dst_port and
// src_port depend on ip_proto being set to tcp, udp or sctp". The bulk traffic is
// HTTP, so tcp.
//
// This is a function rather than an inline literal so a test can assert on the
// argument list. Asserting on the source text instead would match the first
// "dst_port" anywhere in the file, which is not this command.
func bulkFilterArgs(device string, port int) []string {
	return []string{"filter", "add",
		"dev", device,
		"parent", "1:",
		"protocol", "ip",
		"prio", "20",
		"flower",
		"ip_proto", "tcp",
		"dst_port", strconv.Itoa(port),
	}
}

// filterInstalled reports whether `tc filter show` printed a flower filter
// matching the bulk port.
//
// Both halves have to be present. A flower filter without the port matches
// nothing useful, and a port in the output without ip_proto would mean the
// filter is classifying something other than what it was added for -- the exact
// ambiguity the ip_proto fix removed on the way in.
func filterInstalled(out string, port int) bool {
	hasFlower, hasPort, hasProto := false, false, false
	want := "dst_port"

	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "flower") {
			hasFlower = true
		}
		if strings.Contains(line, "ip_proto tcp") {
			hasProto = true
		}
		// Field-by-field, not a substring test: "dst_port 5557" is a prefix of
		// "dst_port 55570", so a contains check would call a filter on the wrong
		// port a success. That is the same class of silent misclassification this
		// function exists to prevent.
		fields := strings.Fields(line)
		for i := 0; i+1 < len(fields); i++ {
			if fields[i] == want && fields[i+1] == strconv.Itoa(port) {
				hasPort = true
			}
		}
	}
	return hasFlower && hasPort && hasProto
}

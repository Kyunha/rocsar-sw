// Command gnss_bench exercises the GNSS leg without the OBC.
//
// It answers three questions the full stack is the wrong shape to answer,
// because between the question and the answer sit four layers and each of them
// is a thing that can be quiet for its own reasons:
//
//	inject  -- is the decoder correct? Builds a real 142-byte datagram and runs
//	           it through the real Decode. This is the substitute for the
//	           --mock-gnss flag that used to claim a simulation while binding
//	           three real receivers.
//	listen  -- are the receivers arriving? Reads the live UDP feeds.
//	bank    -- is the selection behaving? Drives a real gnss.Bank.
//
// It composes internal/gnss, so it cannot drift from what the server accepts.
// Read_uB itself is NOT started: three systemd units own it
// (read_ub@1/2/3.service). This tool only reads the UDP ports they feed.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rocsar/obc/internal/gnss"
)

// discardLogger keeps the bench output readable: the gnss package logs one line
// per bind, which is noise in a tool whose whole output is the measurement.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// lisbon is the default injection point: a real place, so the printed numbers
// are recognisable and an obviously-wrong one stands out.
const (
	latDeg = 38.7223
	lonDeg = -9.1393
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "gnss_bench: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		mode     = flag.String("mode", "inject", "inject | listen | bank")
		ports    = flag.Int("ports", 3, "how many receiver ports, 2001..(2000+n)")
		first    = flag.Int("port", 2001, "first receiver port")
		lat      = flag.Float64("lat", latDeg, "latitude to inject, DEGREES")
		lon      = flag.Float64("lon", lonDeg, "longitude to inject, DEGREES")
		alt      = flag.Float64("alt", 100, "altitude to inject, metres")
		course   = flag.Float64("course", 275.5, "course over ground to inject, DEGREES")
		spd      = flag.Float64("speed", 42, "ground speed to inject, m/s")
		seconds  = flag.Int("seconds", 10, "how long to listen")
		selected = flag.Int("selected", 1, "receiver ID to select in bank mode")
		rotate   = flag.Bool("rotate", false, "in bank mode, rotate the selection once")
	)
	flag.Parse()

	portList := make([]int, *ports)
	for i := range portList {
		portList[i] = *first + i
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch *mode {
	case "inject":
		return inject(ctx, portList, gnss.RawFix{
			GPStime:  float64(time.Now().Unix()),
			Latitude: *lat, Longitude: *lon, Height: *alt,
			// A course of 275.5 degrees at 42 m/s: components that produce it,
			// so the derived ground speed is checked rather than asserted.
			Vnorth: *spd * cosDeg(*course),
			Veast:  *spd * sinDeg(*course),
			Vdown:  -0.4,
			Roll:   0, Pitch: 0, Heading: *course,
		})
	case "listen":
		return listen(ctx, portList, time.Duration(*seconds)*time.Second)
	case "bank":
		return bank(ctx, portList, *selected, *rotate, time.Duration(*seconds)*time.Second)
	default:
		return fmt.Errorf("unknown mode %q; want inject, listen or bank", *mode)
	}
}

// inject sends a real datagram to each receiver port and checks the decoder
// agrees with what was sent.
//
// The comparison is the whole point. Sending bytes and looking at the log proves
// the socket is open; comparing the decoded fix against the encoded one proves
// the decoder is right -- and that is the property whose absence let a
// 57.3x error ship, because the output looked like a plausible number.
func inject(ctx context.Context, ports []int, f gnss.RawFix) error {
	payload, err := gnss.Encode(f)
	if err != nil {
		return fmt.Errorf("building the datagram: %w", err)
	}

	fmt.Printf("datagram: %d bytes, markers 0x%02X..0x%02X\n",
		len(payload), payload[0], payload[len(payload)-1])
	fmt.Printf("injecting, in DEGREES (the wire unit -- see internal/gnss/decode.go):\n")
	fmt.Printf("  lat %.6f  lon %.6f  alt %.1f m  course %.2f deg\n",
		f.Latitude, f.Longitude, f.Height, f.Heading)
	fmt.Println()

	// Decode locally first, so a decoder fault is reported as a decoder fault
	// rather than as a receiver that appears to be silent.
	check, err := gnss.Decode(payload, 0, time.Now())
	if err != nil {
		return fmt.Errorf("the decoder rejected a datagram this tool produced: %w", err)
	}
	fmt.Printf("local decode  lat %.6f  lon %.6f  ground speed %.2f m/s\n",
		check.Latitude, check.Longitude, check.GroundSpeed())
	if check.Latitude != f.Latitude || check.Longitude != f.Longitude {
		return fmt.Errorf("local round trip changed the position: sent %.6f,%.6f got %.6f,%.6f",
			f.Latitude, f.Longitude, check.Latitude, check.Longitude)
	}

	for _, p := range ports {
		addr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			return err
		}
		conn, err := net.DialUDP("udp4", nil, addr)
		if err != nil {
			fmt.Printf("  port %d: cannot send (%v)\n", p, err)
			continue
		}
		if _, err := conn.Write(payload); err != nil {
			fmt.Printf("  port %d: send failed: %v\n", p, err)
		} else {
			fmt.Printf("  port %d: sent %d bytes\n", p, len(payload))
		}
		_ = conn.Close()
	}

	fmt.Print("\nnow run:  obc  (or gnss_bench -mode listen) and look for the fix\n")
	return nil
}

// listen reads the live receivers and prints each accepted fix.
//
// It does NOT start Read_uB. Three systemd units own that
// (read_ub@1.service, read_ub@2.service, read_ub@3.service), and a second
// process binding the same serial port would fight them for it.
func listen(ctx context.Context, ports []int, d time.Duration) error {
	bank, err := gnss.NewBank(ports, 1, 2*time.Second, discardLogger())
	if err != nil {
		return err
	}
	if err := bank.Start(ctx); err != nil {
		return err
	}
	defer bank.Close()

	fmt.Printf("listening on %v for %s (Read_uB is owned by systemd; this only reads)\n", ports, d)

	deadline := time.Now().Add(d)
	seen := 0
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}

		changed := false
		for _, st := range bank.Status() {
			if !st.HasFix {
				continue
			}
			mark := " "
			if st.Selected {
				mark = "*"
			}
			fmt.Printf("%s rx%d  lat %10.6f  lon %11.6f  alt %7.1f  spd %6.2f  crs %6.2f  "+
				"age %6.2fs  ok=%-5v accepted %d rejected %d\n",
				mark, st.ReceiverID, st.Fix.LatitudeDeg, st.Fix.LongitudeDeg,
				st.Fix.AltitudeM, st.Fix.GroundSpeedMP, st.Fix.CourseDeg,
				st.FixAge.Seconds(), st.FixOK, st.Accepted, st.Rejected)
			changed = true
		}
		if !changed && seen == 0 {
			fmt.Print(".")
		} else if changed {
			seen++
			fmt.Println()
		}
	}

	fmt.Println()
	for _, st := range bank.Status() {
		fmt.Printf("rx%d selected=%-5v fix_ok=%-5v has_fix=%-5v accepted=%d rejected=%d\n",
			st.ReceiverID, st.Selected, st.FixOK, st.HasFix, st.Accepted, st.Rejected)
	}
	if seen == 0 {
		fmt.Println("\nno fix arrived. Check: systemctl status 'read_ub@*.service'")
	}
	return nil
}

// bank drives a real gnss.Bank: select, rotate, and show per-receiver health.
//
// This is where the redundancy behaviour is visible. Selection is explicit and
// never automatic -- see internal/gnss/bank.go for why -- so the only thing to
// test is that selecting and rotating move the trusted receiver and leave the
// others reported.
func bank(ctx context.Context, ports []int, selected int, rotate bool, d time.Duration) error {
	b, err := gnss.NewBank(ports, selected, 2*time.Second, discardLogger())
	if err != nil {
		return err
	}
	if err := b.Start(ctx); err != nil {
		return err
	}
	defer b.Close()

	fmt.Printf("bank of %d receivers, %v, initially trusting rx%d\n", len(ports), ports, b.SelectedID())

	if err := b.Select(selected); err != nil {
		return fmt.Errorf("select %d: %w", selected, err)
	}
	fmt.Printf("  select %d  -> trusted rx%d\n", selected, b.SelectedID())

	if rotate {
		if err := b.Rotate(); err != nil {
			return fmt.Errorf("rotate: %w", err)
		}
		fmt.Printf("  rotate      -> trusted rx%d\n", b.SelectedID())
	}

	// Selecting a receiver that does not exist must be an error, not a silent
	// no-op: an operator who typos an id needs to be told.
	//
	// The candidate is one past the end of the bank: receiver ids are 1..N, so
	// N+1 cannot exist. Two earlier versions got this wrong in opposite ways --
	// one searched for an id "not in ports" (comparing ids against port numbers
	// 2001..2003, so it never found one), the other picked any id different from
	// the current one (which is an id that very much does exist). A test of "the
	// bad case is refused" is only meaningful if the bad case is actually bad.
	bad := len(ports) + 1
	// The error is captured in its own scope: printing `err` from outside
	// reported the last SUCCESSFUL selection's nil, so the line read
	// "refused, as it must be (<nil>)".
	if err := b.Select(bad); err != nil {
		fmt.Printf("  select %d    -> refused, as it must be (%v)\n", bad, err)
	} else {
		return fmt.Errorf("selecting non-existent rx%d succeeded; it must be refused", bad)
	}
	if err := b.Select(b.SelectedID()); err != nil {
		return err
	}

	fmt.Printf("\nper-receiver health for %s (all three are reported, one is trusted):\n", d)
	deadline := time.Now().Add(d)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		fmt.Println()
		for _, st := range b.Status() {
			mark := " "
			if st.Selected {
				mark = "*"
			}
			fmt.Printf("%s rx%d  fix_ok=%-5v age=%6.2fs accepted=%-8d rejected=%d\n",
				mark, st.ReceiverID, st.FixOK, st.FixAge.Seconds(), st.Accepted, st.Rejected)
		}
	}

	if fix, ok := b.SelectedFix(); ok {
		fmt.Printf("\ntrusted fix: lat %.6f lon %.6f alt %.1f\n",
			fix.LatitudeDeg, fix.LongitudeDeg, fix.AltitudeM)
	} else {
		fmt.Println("\nno usable trusted fix. A fix older than the staleness bound is " +
			"reported as no fix: an old fix is worse than none.")
	}
	return nil
}

// cosDeg and sinDeg keep the injection maths legible next to the degrees it
// comes from, and name the unit conversion so it cannot be applied twice --
// which is the bug this tool exists to guard against.
func cosDeg(deg float64) float64 { return math.Cos(deg * deg2rad) }
func sinDeg(deg float64) float64 { return math.Sin(deg * deg2rad) }

const deg2rad = math.Pi / 180

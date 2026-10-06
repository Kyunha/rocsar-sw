// Command pico_bench talks straight to the flight controller, with no server.
//
// It is the right tool for "what is the Pico actually doing", which is a question
// the full stack is the wrong shape to answer: between the question and the
// answer sit four layers, and each of them is a thing that can be quiet for its
// own reasons. A silent Pico through the OBC might be a dead USB cable, an
// unplugged board the port still opens for, a wedged 50 Hz loop, or a telemetry
// path that dropped a frame. This tool removes three of those four.
//
// It composes internal/pico -- the same codec, the same COBS framing, the same
// ACK correlation the server uses -- so it cannot drift from what the server
// accepts. It sends every command typed and reads every acknowledgement back,
// rather than only writing.
//
//	pico_bench                      interactive console
//	pico_bench --raw --seconds 10   passive sniffer, no commands sent
//
// Three deliberate choices, all the opposite of what looks convenient. They are
// inherited from the Python pico_debug.py this replaces, and each exists because
// the convenient version was tried:
//
//  1. MOTION IS GATED BEHIND AN EXPLICIT `arm`. A typo in an interactive console
//     that drives a loaded 5 V servo is not a recoverable mistake, and this is
//     the one place a mistyped argument reaches hardware with nothing in between.
//     Reading the IMU needs no permission; moving an antenna does.
//
//  2. SERVO IDS COME FROM THE FIRMWARE, NEVER FROM A CONSTANT HERE. The ids are
//     whichever the board is built with. `stop all` is the dangerous case: the
//     previous GUI sent `stop` for ids it assumed, and on a differently
//     configured bench that stops nothing at all -- so `stop all` here means
//     "stop whatever this board reports", never "stop 1 and 2".
//
//  3. "NO READING" IS NEVER PRINTED AS A NUMBER. An axis reports MEASURED or
//     HELD, never a bare 0. current_tick is a real encoder reading only when
//     the firmware says so; before that it is an echo of the last command, and a
//     console showing both identically tells an operator the servo is where it
//     was sent when it may be stalled against a stop.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"io"
	"log/slog"

	"github.com/rocsar/obc/internal/domain"
	"github.com/rocsar/obc/internal/pico"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "pico_bench: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		port    = flag.String("port", "/dev/ttyACM0", "flight controller serial port")
		baud    = flag.Int("baud", pico.DefaultBaudrate, "USB link rate to the Pi (NOT the ST3215 servo bus rate, which is also 115200 and unrelated)")
		raw     = flag.Bool("raw", false, "passive: report frames and send nothing")
		seconds = flag.Int("seconds", 0, "run for this long then exit (0 = until Ctrl-C)")
		ackTO   = flag.Duration("ack-timeout", pico.DefaultAckTimeout, "how long to wait for an acknowledgement")
	)
	flag.Parse()

	serial := pico.NewSerialTransport(*port, *baud)
	if !serial.Exists() {
		// Named plainly rather than as the syscall's "no such file or directory",
		// which does not say which file.
		return fmt.Errorf("no flight controller at %s. Check it is plugged in and that "+
			"the port is right -- it may have enumerated as ttyACM1", *port)
	}
	if err := serial.OpenPort(); err != nil {
		return err
	}

	link := pico.NewLink(serial, discardLogger(),
		pico.WithAckTimeout(*ackTO),
		pico.WithMock(false))
	defer link.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *raw {
		// Passive means passive: Open() would put a status_request on the wire.
		// Listen() only starts the reader -- the firmware streams telemetry
		// every control tick on its own, so sending nothing still sees frames.
		link.Listen(ctx)
		return sniff(ctx, link, *seconds)
	}

	return runConsole(ctx, link, *port, *seconds)
}

// sniff reports telemetry and sends nothing.
//
// The passive mode exists because "is it transmitting?" and "does it obey?" are
// different questions, and a console that moves an axis while you are trying to
// answer the first one answers neither.
func sniff(ctx context.Context, link *pico.Link, seconds int) error {
	fmt.Println("raw mode: reporting only. No commands will be sent.")

	// A zero Deadline means "no limit", expressed as an unset time rather than a
	// nil channel: a nil channel in a select never fires, which is correct but
	// takes a reader a moment to see.
	var deadline time.Time
	if seconds > 0 {
		deadline = time.Now().Add(time.Duration(seconds) * time.Second)
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	last := time.Time{}
	for {
		if !deadline.IsZero() && time.Now().After(deadline) {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			tel, ok := link.Telemetry()
			if !ok {
				fmt.Println("no telemetry frame has ever arrived")
				continue
			}
			unsolicited, protoErrs, age, _ := link.Diagnostics()
			fmt.Printf("\n-- age %6.2fs  unsolicited acks %d  protocol errors %d --\n",
				age.Seconds(), unsolicited, protoErrs)
			printTelemetry(tel, time.Since(last) < 2*time.Second)
			last = time.Now()
		}
	}
}

// console is the interactive mode.
func runConsole(ctx context.Context, link *pico.Link, port string, seconds int) error {
	// Prove the link before offering commands that move things. Open() already
	// does a status_request round trip and fails if nothing answers, so reaching
	// here means the device is talking.
	if err := link.Open(ctx); err != nil {
		return fmt.Errorf("the flight controller did not answer: %w", err)
	}
	fmt.Printf("connected to %s\n", port)
	fmt.Println(`commands:
  status                      report the latest frame and link health
  ids                         the servo ids THIS board reports
  set <deg>                   point both axes at a bearing
  jog <tick>                  move the selected axis to an absolute tick   (needs arm)
  zero                        teach the selected axis its centre           (needs arm)
  mount <deg>                 set the selected axis's mount offset        (needs arm)
  dir <+1|-1>                 set the selected axis's direction           (needs arm)
  heater <1|2> <on|off>       switch a heater
  target <deg>                same as set
  stop                        stop the selected axis                      (needs arm)
  stop all                    stop every axis the board reports          (needs arm)
  arm / disarm                gate motion
  raw                         report frames until interrupted
  quit`)

	// The firmware's own view of the world. Populated from telemetry, never
	// assumed -- see principle 2 in the package comment.
	ids := &firmwareIDs{}

	c := &console{
		ctx: ctx, link: link, ids: ids, armed: false,
		out:  bufio.NewWriter(os.Stdout),
		desc: port,
	}
	go c.watch()

	fmt.Print("> ")
	if err := c.repl(); err != nil {
		return err
	}

	unsolicited, protoErrs, age, haveTel := link.Diagnostics()
	fmt.Printf("\nlink: unsolicited acks %d, protocol errors %d, telemetry age %s, ever received %v\n",
		unsolicited, protoErrs, age.Round(time.Millisecond), haveTel)
	return nil
}

type console struct {
	ctx   context.Context
	link  *pico.Link
	ids   *firmwareIDs
	armed bool
	out   *bufio.Writer
	desc  string
}

// watch keeps the firmware's servo ids current from telemetry, so `ids` is
// whatever the board actually reports.
func (c *console) watch() {
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-t.C:
			if tel, ok := c.link.Telemetry(); ok {
				c.ids.observe(tel.Axes)
			}
		}
	}
}

func (c *console) say(format string, args ...any) {
	fmt.Fprintf(c.out, format+"\n", args...)
	c.out.Flush()
}

func (c *console) repl() error {
	sc := bufio.NewScanner(os.Stdin)
	lines := make(chan string)
	go func() {
		defer close(lines)
		for sc.Scan() {
			lines <- strings.TrimSpace(sc.Text())
		}
	}()

	for {
		select {
		case <-c.ctx.Done():
			return nil
		case line, ok := <-lines:
			if !ok {
				return nil // stdin closed
			}
			if line == "" {
				continue
			}
			if line == "quit" || line == "exit" {
				return nil
			}
			if quit := c.dispatch(line); quit {
				return nil
			}
			fmt.Print("> ")
		}
	}
}

// requireArm gates every command that moves an axis.
//
// The error names the command, because "permission denied" with no subject is
// worse than useless in a console.
func (c *console) requireArm(verb string) error {
	if c.armed {
		return nil
	}
	return fmt.Errorf("%s moves an antenna and needs `arm` first", verb)
}

func (c *console) dispatch(line string) (quit bool) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return false
	}
	cmd, args := strings.ToLower(fields[0]), fields[1:]

	switch cmd {
	case "arm":
		c.armed = true
		c.say("armed. motion is permitted until `disarm`.")
		return false
	case "disarm":
		c.armed = false
		c.say("disarmed.")
		return false

	case "help", "?":
		c.say("see the banner above")
		return false

	case "status":
		tel, ok := c.link.Telemetry()
		if !ok {
			c.say("no telemetry frame has ever arrived from %s", "the flight controller")
			return false
		}
		c.say("link: connected=%v", c.link.Connected())
		printTelemetry(tel, true)
		unsolicited, protoErrs, age, _ := c.link.Diagnostics()
		c.say("  unsolicited acks %d, protocol errors %d, telemetry age %s",
			unsolicited, protoErrs, age.Round(time.Millisecond))
		return false

	case "ids":
		if err := c.requireIDs(); err != nil {
			c.say("%v", err)
			return false
		}
		c.say("this board reports servo ids %v", c.ids.list())
		return false

	case "raw":
		c.say("reporting frames; Ctrl-C or `quit` to stop")
		for {
			select {
			case <-c.ctx.Done():
				return true
			case <-time.After(time.Second):
				tel, ok := c.link.Telemetry()
				if !ok {
					c.say("  (no telemetry yet)")
					continue
				}
				printTelemetryTo(c.out, tel, true)
			}
		}

	case "set", "target":
		if len(args) != 1 {
			c.say("usage: %s <degrees>", cmd)
			return false
		}
		deg, err := strconv.ParseFloat(args[0], 64)
		if err != nil {
			c.say("%v", err)
			return false
		}
		ack, err := c.link.SetTarget(c.ctx, deg)
		c.reportAck("set_target", ack, err)

	case "jog":
		if err := c.requireArm("jog"); err != nil {
			c.say("%v", err)
			return false
		}
		if len(args) != 1 {
			c.say("usage: jog <tick 0..%d>   (the selected axis)", pico.MaxServoTick)
			return false
		}
		tick, err := strconv.ParseUint(args[0], 10, 32)
		if err != nil || tick > pico.MaxServoTick {
			c.say("tick must be an integer 0..%d", pico.MaxServoTick)
			return false
		}
		id, err := c.ids.selected()
		if err != nil {
			c.say("%v", err)
			return false
		}
		ack, err := c.link.Jog(c.ctx, id, uint32(tick))
		c.reportAck(fmt.Sprintf("jog(servo %d, tick %d)", id, tick), ack, err)

	case "zero", "mount", "dir", "stop", "heater":
		c.axisCommand(cmd, args)

	default:
		c.say("unknown command %q. Type `help`.", cmd)
	}
	return false
}

// axisCommand handles the per-axis commands, all of which need arm.
func (c *console) axisCommand(cmd string, args []string) {
	if err := c.requireArm(cmd); err != nil {
		c.say("%v", err)
		return
	}

	if cmd == "stop" && len(args) == 1 && strings.EqualFold(args[0], "all") {
		// Stop whatever the BOARD reports, never a hardcoded list. See principle 2.
		if err := c.requireIDs(); err != nil {
			c.say("%v", err)
			return
		}
		for _, id := range c.ids.list() {
			ack, err := c.link.Stop(c.ctx, id)
			c.reportAck(fmt.Sprintf("stop(servo %d)", id), ack, err)
		}
		return
	}

	id, err := c.ids.selected()
	if err != nil {
		c.say("%v", err)
		return
	}

	switch cmd {
	case "zero", "stop":
		var (
			ack *domain.Ack
			err error
		)
		if cmd == "zero" {
			ack, err = c.link.Zero(c.ctx, id)
		} else {
			ack, err = c.link.Stop(c.ctx, id)
		}
		c.reportAck(cmd, ack, err)

	case "mount":
		if len(args) != 1 {
			c.say("usage: mount <degrees>")
			return
		}
		deg, err := strconv.ParseFloat(args[0], 64)
		if err != nil {
			c.say("%v", err)
			return
		}
		ack, err := c.link.Mount(c.ctx, id, deg)
		c.reportAck("mount", ack, err)

	case "dir":
		if len(args) != 1 {
			c.say("usage: dir <+1|-1>")
			return
		}
		m, err := strconv.ParseFloat(args[0], 64)
		if err != nil {
			c.say("%v", err)
			return
		}
		ack, err := c.link.SetDirection(c.ctx, id, m)
		c.reportAck("dir", ack, err)

	case "heater":
		// Heaters do not move an antenna, so they are not arm-gated.
		if len(args) != 2 {
			c.say("usage: heater <1|2> <on|off>")
			return
		}
		n, err := strconv.ParseUint(args[0], 10, 32)
		if err != nil {
			c.say("%v", err)
			return
		}
		on := strings.EqualFold(args[1], "on") || args[1] == "1"
		ack, err := c.link.SetHeater(c.ctx, uint32(n), on)
		c.reportAck("heater", ack, err)
	}
}

// reportAck prints an outcome.
//
// The three outcomes are distinguished on purpose: an acknowledgement that
// reports failure, a timeout, and "not connected" are different faults and an
// operator who cannot tell them apart will debug the wrong thing.
func (c *console) reportAck(what string, ack *domain.Ack, err error) {
	switch {
	case err != nil:
		switch {
		case errors.Is(err, pico.ErrAckTimeout):
			c.say("%s: TIMED OUT after %s -- the link is up and the device did not answer. "+
				"That is a wedged flight controller, not a cable.", what, pico.DefaultAckTimeout)
		case errors.Is(err, pico.ErrNotConnected):
			c.say("%s: NOT CONNECTED -- there is nothing to talk to. That is a cable or an "+
				"unplugged board.", what)
		default:
			c.say("%s: %v", what, err)
		}
	case ack == nil:
		c.say("%s: no acknowledgement and no error", what)
	case !ack.Success:
		c.say("%s: REFUSED by the flight controller (%s)", what, ack.Error)
	default:
		c.say("%s: acknowledged (sequence %d)", what, ack.CommandSequence)
	}
}

func (c *console) requireIDs() error {
	if len(c.ids.list()) == 0 {
		return errors.New("no servo ids known yet; the board has not reported telemetry. " +
			"Wait a moment, or type `status`")
	}
	return nil
}

// firmwareIDs is what the board says its axes are.
type firmwareIDs struct {
	order []uint32
	seen  map[uint32]bool
	first uint32
}

func (f *firmwareIDs) observe(axes []domain.Axis) {
	for _, a := range axes {
		if f.seen == nil {
			f.seen = map[uint32]bool{}
		}
		if f.seen[a.ServoID] {
			continue
		}
		f.seen[a.ServoID] = true
		f.order = append(f.order, a.ServoID)
		if f.first == 0 {
			f.first = a.ServoID
		}
	}
}

func (f *firmwareIDs) list() []uint32 {
	out := append([]uint32(nil), f.order...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// selected is the axis a single-axis command applies to: the lowest id the board
// reports, which is the physical axis 0.
func (f *firmwareIDs) selected() (uint32, error) {
	if len(f.order) == 0 {
		return 0, errors.New("no servo ids known yet; the board has not reported telemetry")
	}
	return f.first, nil
}

// printTelemetry renders one frame, marking any reading that is not a
// measurement. See principle 3.
func printTelemetry(tel domain.PicoTelemetry, fresh bool) {
	marker := " "
	if fresh {
		marker = "*"
	}
	fmt.Printf("%s pico: heading %.2f deg (target %.2f)  imu=%s  heaters %s/%s\n",
		marker, tel.GondolaHeadingDeg, tel.TargetHeadingDeg,
		imuState(tel.IMUPresent), onOff(tel.Heater1State), onOff(tel.Heater2State))
	for _, a := range tel.Axes {
		fmt.Printf("    servo %-3d tick %-5d angle %8.3f deg  load %6d  %3d C  %s%s\n",
			a.ServoID, a.CurrentTick, a.CurrentAngleDeg, a.Load, a.TemperatureC,
			feedbackWord(a.FeedbackState), errorSuffix(a.FeedbackError))
	}
}

func printTelemetryTo(w *bufio.Writer, tel domain.PicoTelemetry, fresh bool) {
	marker := " "
	if fresh {
		marker = "*"
	}
	fmt.Fprintf(w, "%s heading %.2f (target %.2f) imu=%s heaters %s/%s\n",
		marker, tel.GondolaHeadingDeg, tel.TargetHeadingDeg,
		imuState(tel.IMUPresent), onOff(tel.Heater1State), onOff(tel.Heater2State))
	for _, a := range tel.Axes {
		fmt.Fprintf(w, "    servo %-3d tick %-5d angle %8.3f load %6d %3dC %s%s\n",
			a.ServoID, a.CurrentTick, a.CurrentAngleDeg, a.Load, a.TemperatureC,
			feedbackWord(a.FeedbackState), errorSuffix(a.FeedbackError))
	}
	w.Flush()
}

// feedbackWord is the whole point of principle 3: a word, never a bare number.
func feedbackWord(s domain.FeedbackState) string {
	switch s {
	case domain.FeedbackMeasured:
		return "MEASURED"
	case domain.FeedbackHeld:
		return "HELD (stale)"
	default:
		return "UNKNOWN (never measured)"
	}
}

// errorSuffix surfaces the ST3215's own status byte. A servo can answer a
// well-formed frame while reporting overheat or an overloaded regulator, and this
// byte used to be parsed by nothing and discarded -- so a servo in trouble read
// as a clean one.
func errorSuffix(code int32) string {
	if code == 0 {
		return ""
	}
	return fmt.Sprintf("  !! SERVO STATUS 0x%02x", code)
}

func imuState(present bool) string {
	if present {
		return "MEASURED"
	}
	return "HELD (no IMU fitted, or not answering)"
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// discardLogger keeps the bench output to the measurement. The pico package
// logs a line per connect, which is noise in a tool whose output IS the answer.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

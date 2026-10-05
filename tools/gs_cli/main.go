// Command gs_cli is the operator console for the OBC, from a terminal.
//
// Everything else in tools/ talks to hardware directly. This is the one tool that
// speaks to the server, and it exists because a laptop with no webkit still has to
// be able to see and drive the system.
//
// The link itself lives in internal/client, which the Wails console in cmd/gs
// also uses. This is the presentation and the argument parsing; the sockets, the
// framing, the validation and the artefact transfer are the shared package's. One
// client against the OBC rather than two, because two would be two things to
// disagree with the server about the frame layout -- and the frame layout has
// already cost an afternoon once, see internal/client.
//
// It shares api/rocsar/v1 with the OBC, so there is one schema and one set of
// generated bindings across both sides rather than a Go and a Python copy of the
// same contract that can disagree.
//
//	gs_cli watch                 follow telemetry
//	gs_cli status                one telemetry frame and exit
//	gs_cli cmd query             send one command by name
//	gs_cli fetch <artefact>      download an artefact, resuming if interrupted
//	gs_cli ls                    list what is on the OBC
//
// Deliberately not here: an arm gate, a job queue, a state machine, or anything
// that makes this harder to run in a hurry. It is a console.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	rocsarv1 "github.com/rocsar/obc/api/rocsar/v1"
	"github.com/rocsar/obc/internal/client"
)

// firstFlag returns the index of the first argument that looks like a flag, or -1.
//
// Scanning rather than testing args[1]: `fetch photo.jpg -o out.jpg` puts the flag
// third, and checking only the second meant the re-parse never ran for exactly the
// invocation this was written to fix.
func firstFlag(args []string, from int) int {
	for i := from; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") && len(args[i]) > 1 {
			return i
		}
	}
	return -1
}

// flagValue adapts an already-bound flag value to a second FlagSet, so the same
// destination can be written by either parse.
type flagValue struct{ p any }

func (v flagValue) String() string { return "" }
func (v flagValue) Set(s string) error {
	switch p := v.p.(type) {
	case *string:
		*p = s
	case *bool:
		*p = s == "true" || s == "1"
	}
	return nil
}

// console is this tool's view of the shared client: the client plus the handful of
// settings that are about output rather than about the link.
type console struct {
	cli *client.Client

	// axisIDs are the servo ids the flight controller last reported, in the order
	// it reported them. Held so `stop all` can be expanded against them.
	//
	// Not a hardcoded {1, 2}. The firmware's ANTENNA_0_SERVO_ID and
	// ANTENNA_1_SERVO_ID are overridable at build time
	// (-DANTENNA_0_SERVO_ID=3), so a bench build numbers its axes differently and
	// a fixed list would silently stop the wrong thing -- or nothing. The board
	// is the only authority on its own ids, which is why client.BuildRequests
	// refuses `stop all` and leaves the expansion to the caller.
	axisIDs []uint32
}

// noteAxes records the ids from a telemetry frame.
func (c *console) noteAxes(f *rocsarv1.TelemetryFrame) {
	pico := f.GetPico()
	if pico == nil {
		return
	}
	ids := make([]uint32, 0, len(pico.GetAntennas()))
	for _, a := range pico.GetAntennas() {
		ids = append(ids, a.GetServoId())
	}
	if len(ids) > 0 {
		c.axisIDs = ids
	}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "gs_cli: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		control   = flag.String("control", "tcp://192.168.1.50:5555", "OBC control socket (ROUTER)")
		telemetry = flag.String("telemetry", "tcp://192.168.1.50:5556", "OBC telemetry socket (PUB)")
		httpAddr  = flag.String("http", "http://192.168.1.50:5557", "OBC artefact server")
		out       = flag.String("o", "", "write the fetched artefact here instead of stdout")
		resume    = flag.Bool("resume", true, "continue a partial download instead of starting over")
		raw       = flag.Bool("raw", false, "print the full telemetry frame as JSON")
		topic     = flag.String("topic", "telemetry", "telemetry topic to subscribe to")
	)
	flag.Usage = usage
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		usage()
		return errors.New("no command given")
	}

	// Accept flags after the command as well as before it.
	//
	// Go's flag package stops parsing at the first non-flag argument, so
	// `gs_cli fetch x.jpg -o out.jpg` silently ignored -o and wrote the
	// photograph to stdout. For a tool whose whole job is being used in a hurry,
	// having the obvious invocation quietly do the wrong thing is worse than a
	// little re-parsing here. Flags may now appear on either side of the command.
	if i := firstFlag(args, 1); i >= 0 {
		before := args[:i]
		fs := flag.NewFlagSet("gs_cli", flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		for _, f := range []struct {
			name string
			ptr  any
		}{
			{"control", control}, {"telemetry", telemetry}, {"http", httpAddr},
			{"topic", topic}, {"o", out},
		} {
			fs.Var(flagValue{f.ptr}, f.name, "")
		}
		for _, b := range []struct {
			name string
			ptr  *bool
		}{{"resume", resume}, {"raw", raw}} {
			fs.BoolVar(b.ptr, b.name, *b.ptr, "")
		}
		if err := fs.Parse(args[i:]); err != nil {
			return err
		}
		args = append(before, fs.Args()...)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	c := &console{cli: client.New(client.Config{
		Control:   *control,
		Telemetry: *telemetry,
		HTTP:      *httpAddr,
		Topic:     *topic,
	})}

	switch args[0] {
	case "watch":
		return c.watch(ctx, *raw)
	case "status":
		return c.status(ctx, *raw)
	case "cmd":
		if len(args) < 2 {
			return errors.New("cmd needs a command name; try `gs_cli commands`")
		}
		return c.command(ctx, args[1], args[2:])
	case "commands":
		printCommands()
		return nil
	case "ls":
		return c.list(ctx, "")
	case "fetch":
		if len(args) < 2 {
			return errors.New("fetch needs an artefact name; try `gs_cli ls`")
		}
		return c.fetch(ctx, args[1], *out, *resume)
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `gs_cli <command>

  watch                follow telemetry, one line per frame
  status               print one telemetry frame and exit
  cmd <name> [args]    send one command
  commands             list the command names cmd accepts
  ls [path]            list the artefacts on the OBC
  fetch <artefact>     download an artefact over HTTP

flags:
`)
	flag.PrintDefaults()
}

// printCommands lists what `cmd` accepts.
//
// The names come from the shared client rather than from a table kept here, so
// this listing cannot drift from what cmd actually accepts -- the window in
// cmd/gs reads the same list.
func printCommands() {
	help := map[string]string{
		"query":          "the OBC's own status",
		"photo":          "capture one photograph",
		"gnss":           "<receiver-id>       select a GNSS receiver",
		"gnss-rotate":    "trust the next receiver instead of naming one",
		"heading":        "<degrees>          point both antenna axes at a bearing",
		"jog":            "<servo-id> <tick>   move one axis to an absolute tick",
		"zero":           "[<servo-id>]        centre one axis, or both",
		"mount":          "<servo-id> <deg>    set an axis's mount offset",
		"dir":            "<servo-id> <+1|-1>  set an axis's direction",
		"heater":         "<1|2> <on|off>     switch a heater",
		"stop":           "<servo-id>       stop one axis (use `status` first for ids)",
		"pico-status":    "ask the flight controller to re-announce itself",
		"sdr-probe":      "run uhd_usrp_probe",
		"sdr-get-params": "read parameters/params.json as JSON",
		"sdr-connect":    "start the acquisition program",
		"sdr-reset-usb":  "power-cycle the SDR's USB port",
		"link":           "<kbit>             set the link rate limit",
		"reboot":         "restart the OBC",
	}
	for _, name := range client.Names() {
		if h, ok := help[name]; ok {
			fmt.Printf("  %-28s %s\n", name, h)
			continue
		}
		fmt.Printf("  %s\n", name)
	}
	fmt.Print(`
There is no arm gate. That was removed deliberately: it added a step between an
operator and a command without making anything safer, because the one consumer
that had it was a bench tool. The window in cmd/gs does require confirmation for
motion, for reasons that do not transfer to a terminal -- see GUI_ARCHITECTURE.md
section 10.`)
}

// ---------------------------------------------------------------------------
// Telemetry
// ---------------------------------------------------------------------------

// watch follows telemetry until interrupted.
func (c *console) watch(ctx context.Context, raw bool) error {
	s, err := c.cli.Subscribe()
	if err != nil {
		return err
	}
	defer s.Close()

	fmt.Printf("following %s; Ctrl-C to stop\n\n", c.cli.Config().Telemetry)
	return c.pump(ctx, s, raw, true)
}

// status prints one frame.
func (c *console) status(ctx context.Context, raw bool) error {
	s, err := c.cli.Subscribe()
	if err != nil {
		return err
	}
	defer s.Close()

	return c.pump(ctx, s, raw, false)
}

func (c *console) pump(ctx context.Context, s *client.Subscription, raw, forever bool) error {
	for {
		frame, err := s.Recv(client.SubTimeout())
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if raw {
			b, _ := jsonFrame(frame)
			fmt.Println(string(b))
		} else {
			printTelemetry(frame)
		}
		c.noteAxes(frame)
		if !forever {
			return nil
		}
	}
}

// ---------------------------------------------------------------------------
// Commands
// ---------------------------------------------------------------------------

// build turns a command line into requests, expanding `all` where the shared
// builder cannot.
//
// Only `stop all` needs this, and only because the axis ids are a property of
// the firmware build rather than of the schema. One request per axis, in the
// order the board reported them.
func (c *console) build(name string, args []string) ([]*rocsarv1.CommandRequest, error) {
	if name == "stop" && len(args) == 1 && strings.EqualFold(args[0], "all") {
		if len(c.axisIDs) == 0 {
			// Not an error about the command: the operator has not heard from a
			// flight controller yet, so there is nothing to stop and no way to
			// know what the axes are called. `gs_cli status` gets a frame in one
			// round trip.
			return nil, fmt.Errorf(
				"no axis ids seen yet: run `gs_cli status` first, then `stop <servo-id>` " +
					"for each axis it reports")
		}
		out := make([]*rocsarv1.CommandRequest, 0, len(c.axisIDs))
		for _, id := range c.axisIDs {
			req, err := client.BuildRequests(name, []string{strconv.FormatUint(uint64(id), 10)})
			if err != nil {
				return nil, err
			}
			out = append(out, req...)
		}
		return out, nil
	}
	return client.BuildRequests(name, args)
}

// command sends one request and waits for its reply.
func (c *console) command(ctx context.Context, name string, args []string) error {
	reqs, err := c.build(name, args)
	if err != nil {
		return err
	}

	for i, req := range reqs {
		resp, err := c.cli.Send(ctx, req)
		if err != nil {
			if i == 0 {
				return err
			}
			// A multi-axis command that got partway must say which axis it reached,
			// or the operator retries `zero` and cannot tell what is already centred.
			return fmt.Errorf("%w (axis %d of %d had already been sent)", err, i, len(reqs))
		}
		fmt.Printf("%s: success=%v error=%s\n", name, resp.GetSuccess(), resp.GetError())
		if m := resp.GetMessage(); m != "" {
			fmt.Printf("  %s\n", m)
		}
		if n := resp.GetArtefactName(); n != "" {
			fmt.Printf("  artefact   %s (%d bytes, %s) -- gs_cli fetch %s\n",
				n, resp.GetArtefactSizeBytes(), resp.GetArtefactKind(), n)
		}
		if !resp.GetSuccess() {
			return fmt.Errorf("the OBC refused: %s", resp.GetError())
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Artefacts
// ---------------------------------------------------------------------------

func (c *console) list(ctx context.Context, path string) error {
	listing, err := c.cli.List(ctx, path)
	if err != nil {
		return err
	}
	if len(listing.Files) == 0 {
		fmt.Println("no artefacts")
		return nil
	}
	for _, e := range listing.Files {
		if e.Directory {
			fmt.Printf("%-44s %-8s %10s\n", e.Name+"/", "dir", "-")
			continue
		}
		fmt.Printf("%-44s %-8s %10d\n", e.Name, e.Kind, e.SizeBytes)
	}
	return nil
}

// fetch downloads an artefact.
//
// Every line of narration goes to stderr, and that is not tidiness. With no -o,
// the artefact itself goes to stdout -- `gs_cli fetch photo.jpg > out.jpg` is the
// obvious invocation -- so a single status line printed to stdout lands in the
// middle of the file and produces something that is not a JPEG, with no error
// anywhere to explain it. The tool had exactly that bug: it printed
// "<name>: <n> bytes" to stdout while fetching to stdout.
func (c *console) fetch(ctx context.Context, name, out string, resume bool) error {
	x, err := c.cli.Fetch(ctx, name, out, resume, client.FetchHooks{
		Note: func(s string) { fmt.Fprintln(os.Stderr, s) },
	})
	if err != nil {
		return err
	}

	where := out
	if where == "" {
		where = "stdout"
	} else {
		where = filepath.Base(where)
	}
	fmt.Fprintf(os.Stderr, "wrote %s: %d bytes in %.1fs (%.1f kB/s)\n",
		where, x.Bytes, x.Elapsed.Seconds(), x.Rate())
	return nil
}

// jsonFrame is kept here rather than in internal/client so the client package has
// no opinion about how a console prints.
func jsonFrame(f *rocsarv1.TelemetryFrame) ([]byte, error) {
	return json.Marshal(f)
}

// Command gs_cli is the operator console for the OBC, from a terminal.
//
// Everything else in tools/ talks to hardware directly. This is the one tool that
// speaks to the server, and it is here because the Ground Station -- a PySide6
// application on a laptop -- is not written yet, and until it is, there is no way
// to see or drive the system except by hand.
//
// It shares api/rocsar/v1 with the OBC, so there is one schema and one set of
// generated bindings across both sides rather than a Go and a Python copy of the
// same contract that can disagree. tools/gs_probe.py proved the wire works from
// pyzmq; this proves it from the same binding the server uses.
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
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
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

	c := &client{control: *control, telemetry: *telemetry, http: *httpAddr, topic: *topic}

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
		return c.list(ctx)
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
  ls                   list the artefacts on the OBC
  fetch <artefact>     download an artefact over HTTP

flags:
`)
	flag.PrintDefaults()
}

func printCommands() {
	fmt.Println(`commands, by the name "cmd" takes:

  query                        the OBC's own status
  photo                        capture one photograph
  gnss <receiver-id>           select a GNSS receiver
  heading <degrees>            point both antenna axes at a bearing
  jog <servo-id> <tick>        move one axis to an absolute tick
  zero                         centre both axes
  mount <servo-id> <degrees>   set an axis's mount offset
  dir <servo-id> <+1|-1>       set an axis's direction
  heater <1|2> <on|off>        switch a heater
  stop <servo-id|all>          stop one axis, or every axis
  pico-status                  ask the flight controller to re-announce itself
  sdr-probe                    run uhd_usrp_probe
  sdr-connect                  start the acquisition program
  sdr-reset-usb                power-cycle the SDR's USB port
  link <kbit>                  set the link rate limit
  reboot                       restart the OBC

There is no arm gate. That was removed deliberately: it added a step between an
operator and a command without making anything safer, because the one consumer
that had it was a bench tool.`)
}

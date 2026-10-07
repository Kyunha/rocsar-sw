// Command gs is the ROCSAR Ground Station: the operator console on a laptop.
//
// It is the second binary, not a mode of the first. The OBC stays headless on
// the Pi; this dials it over the same three links tools/gs_cli uses -- ZeroMQ
// DEALER for commands, ZeroMQ SUB for telemetry, HTTP for artefacts -- and
// shares the client implementation with it (internal/client) so the two cannot
// disagree about the wire.
//
// This file is the composition root and nothing else, like cmd/obc/main.go:
// configuration becomes a client.Config, the App wires Stream + Queue + view,
// and a plain HTTP server serves the frontend and the WebSocket bridge. The
// console is a pure web app: the operator opens the printed URL in their own
// browser. There is no embedded browser, no window management and no CGO. Read
// GUI_ARCHITECTURE.md before changing anything here.
package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/rocsar/obc/internal/client"
	"github.com/rocsar/obc/internal/config"
)

//go:embed all:frontend/dist
var assets embed.FS

// version identifies the binary in logs and crash traces. "dev" unless stamped
// at build time:
//
//	go build -ldflags "-X main.version=$(git rev-parse --short HEAD)"
//
// Binary provenance questions ("which build produced this layout?") end here
// instead of in a guessing thread.
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "gs: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Flag defaults are the EMPTY STRING, not the aircraft address.
	//
	// This is the whole of GUI_ARCHITECTURE.md 11's requirement and the reason it
	// is easy to get wrong: a flag with a real default cannot be told apart from
	// one the operator typed, so the flag always wins and rocsar.toml is never
	// read. An empty default means "not given", and an unflagged run falls
	// through to the file, then the environment, then the code default -- the
	// same four-level order the OBC uses.
	var (
		control   = flag.String("control", "", "OBC control socket (ROUTER); overrides [client] in rocsar.toml")
		telemetry = flag.String("telemetry", "", "OBC telemetry socket (PUB); overrides [client] in rocsar.toml")
		httpAddr  = flag.String("http", "", "OBC artefact server; overrides [client] in rocsar.toml")
		showVer   = flag.Bool("version", false, "print the build version and exit")
	)
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(),
			"ROCSAR Ground Station.\n\n"+
				"Endpoints come from the [client] section of rocsar.toml, which ships\n"+
				"pointing at the aircraft. A local mock OBC takes explicit endpoints:\n\n"+
				"  gs --control tcp://127.0.0.1:5555 --telemetry tcp://127.0.0.1:5556 \\\n"+
				"     --http http://127.0.0.1:5557\n\n"+
				"Precedence, highest first: flag > ROCSAR_* environment > rocsar.local.toml\n"+
				"> rocsar.toml > built-in default. See ARCHITECTURE.md section 11.\n\n"+
				"Flags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showVer {
		fmt.Println(version)
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.Apply(); err != nil {
		return err
	}

	// Flags beat the file and the environment, so they are applied last and only
	// when actually given.
	if *control != "" {
		cfg.Client.ControlEndpoint = *control
	}
	if *telemetry != "" {
		cfg.Client.TelemetryEndpoint = *telemetry
	}
	if *httpAddr != "" {
		cfg.Client.HTTPEndpoint = *httpAddr
	}

	app := NewApp(client.Config{
		Control:   cfg.Client.ControlEndpoint,
		Telemetry: cfg.Client.TelemetryEndpoint,
		HTTP:      cfg.Client.HTTPEndpoint,
		Topic:     "telemetry",
	})

	srv := newServer(app, assets)
	baseURL, err := srv.run(ctx)
	if err != nil {
		return err
	}
	app.hub = srv.hub

	fmt.Printf("Ground station: %s\n", baseURL)
	fmt.Println("Open that URL in your browser. Ctrl-C to stop.")

	app.startup(ctx)

	<-ctx.Done()
	return app.Disconnect()
}

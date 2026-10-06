// Command gs is the ROCSAR Ground Station: the operator console on a laptop.
//
// It is the second binary, not a mode of the first. The OBC stays headless on
// the Pi; this dials it over the same three links tools/gs_cli uses -- ZeroMQ
// DEALER for commands, ZeroMQ SUB for telemetry, HTTP for artefacts -- and
// shares the client implementation with it (internal/client) so the two cannot
// disagree about the wire.
//
// This file is the composition root and nothing else, like cmd/obc/main.go:
// flags become a client.Config, the App wires Stream + Queue + view, and a
// plain HTTP server serves the frontend and the WebSocket bridge. The console
// is a pure web app: the operator opens the printed URL in their own browser.
// There is no embedded browser, no window management and no CGO. Read
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
	var (
		control   = flag.String("control", "tcp://192.168.1.50:5555", "OBC control socket (ROUTER)")
		telemetry = flag.String("telemetry", "tcp://192.168.1.50:5556", "OBC telemetry socket (PUB)")
		httpAddr  = flag.String("http", "http://192.168.1.50:5557", "OBC artefact server")
		showVer   = flag.Bool("version", false, "print the build version and exit")
	)
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(),
			"ROCSAR Ground Station.\n\n"+
				"The defaults point at the aircraft. A local mock OBC takes explicit\n"+
				"endpoints instead:\n\n"+
				"  gs --control tcp://127.0.0.1:5555 --telemetry tcp://127.0.0.1:5556 \\\n"+
				"     --http http://127.0.0.1:5557\n\n"+
				"Configuration from rocsar.toml arrives in step 7; until then these\n"+
				"flags are the whole story. See GUI_ARCHITECTURE.md section 11.\n\n"+
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

	app := NewApp(client.Config{
		Control:   *control,
		Telemetry: *telemetry,
		HTTP:      *httpAddr,
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

	if err := app.Connect(app.initial); err != nil {
		fmt.Fprintf(os.Stderr, "connect: %v\n", err)
	}

	<-ctx.Done()
	return app.Disconnect()
}

// Command gs is the ROCSAR Ground Station: the operator console on a laptop.
//
// It is the second binary, not a mode of the first. The OBC stays headless on
// the Pi; this dials it over the same three links tools/gs_cli uses -- ZeroMQ
// DEALER for commands, ZeroMQ SUB for telemetry, HTTP for artefacts -- and
// shares the client implementation with it (internal/client) so the two cannot
// disagree about the wire.
//
// This file is the composition root and nothing else, like cmd/obc/main.go:
// flags become a client.Config, the App wires Stream + Queue + view, and Wails
// serves the window. Read GUI_ARCHITECTURE.md before changing anything here.
package main

import (
	"embed"
	"flag"
	"fmt"
	"os"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"

	"github.com/rocsar/obc/internal/client"
)

//go:embed all:frontend/dist
var assets embed.FS

// version identifies the binary in logs and crash traces. "dev" unless stamped
// at build time:
//
//	wails build -ldflags "-X main.version=$(git rev-parse --short HEAD)"
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

	app := NewApp(client.Config{
		Control:   *control,
		Telemetry: *telemetry,
		HTTP:      *httpAddr,
		Topic:     "telemetry",
	})

	// 1400x900 is the [gui] default in GUI_ARCHITECTURE.md section 11: the
	// window opens at the size the configuration will eventually own.
	err := wails.Run(&options.App{
		Title:     "ROCSAR Ground Station",
		Width:     1400,
		Height:    900,
		MinWidth:  1000,
		MinHeight: 700,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 18, G: 20, B: 24, A: 1},
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		Bind: []interface{}{
			app,
		},
	})
	if err != nil {
		return err
	}
	return nil
}

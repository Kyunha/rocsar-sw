// Command obc is the ROCSAR On-Board Computer.
//
// It is the composition root and nothing else: the one place that decides which
// implementation is real and which is a double. Every other package takes its
// dependencies as interfaces, which is why this file is where "is the Pico
// plugged in" is answered and nowhere else.
//
// Read ARCHITECTURE.md before changing anything here. It is the design this
// program implements.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/protobuf/proto"

	rocsarv1 "github.com/rocsar/obc/api/rocsar/v1"
	"github.com/rocsar/obc/internal/camera"
	"github.com/rocsar/obc/internal/command"
	"github.com/rocsar/obc/internal/config"
	"github.com/rocsar/obc/internal/domain"
	"github.com/rocsar/obc/internal/gnss"
	"github.com/rocsar/obc/internal/pico"
	"github.com/rocsar/obc/internal/qos"
	"github.com/rocsar/obc/internal/sdr"
	"github.com/rocsar/obc/internal/storage"
	"github.com/rocsar/obc/internal/telemetry"
	"github.com/rocsar/obc/internal/transport"
)

func main() {
	if err := run(); err != nil {
		// The exit code is what systemd reads. A telemetry server that fails to
		// start must say so in a way Restart=on-failure can act on.
		fmt.Fprintf(os.Stderr, "obc: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	flags := flag.NewFlagSet("obc", flag.ContinueOnError)

	// Every flag defaults to nil so that flag's own machinery can never overwrite
	// a value that came from rocsar.toml or the environment. Resolution happens
	// in config.Apply, never here. See ARCHITECTURE.md 11.
	var (
		configPath  = flags.String("config", "", "path to rocsar.toml (default: $ROCSAR_CONFIG, else ./rocsar.toml)")
		mockPico    = flags.Bool("mock-pico", false, "simulate the flight controller")
		mockCamera  = flags.Bool("mock-camera", false, "simulate the camera")
		mockSDR     = flags.Bool("mock-sdr", false, "simulate the SDR")
		noShaping   = flags.Bool("no-link-shaping", false, "do not touch tc")
		showVersion = flags.Bool("version", false, "print the schema version and exit")
	)
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(),
			"ROCSAR On-Board Computer.\n\n"+
				"Configuration precedence, highest first:\n"+
				"  flag  >  ROCSAR_* environment  >  rocsar.toml  >  built-in default\n\n"+
				"Defaults are NOT written into rocsar.toml: a default there is a second\n"+
				"home for a fact, and two homes drift. The values below are the defaults.\n\n"+
				"Flags:\n")
		flags.PrintDefaults()
		fmt.Fprintf(flags.Output(),
			"\nHardware is real by default. A mock is only ever used when asked for by\n"+
				"flag, and every subsystem running against one is named in the telemetry\n"+
				"frame -- a system that fabricated a reading because a device was not found\n"+
				"is the worst failure mode this project has.\n")
	}

	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	if *showVersion {
		fmt.Println("rocsar obc, schema rocsar.v1")
		return nil
	}

	if *configPath != "" {
		if err := os.Setenv(config.PathEnv, *configPath); err != nil {
			return err
		}
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	// Signal handling before anything is opened, so a Ctrl-C during startup is a
	// clean exit rather than a half-built system.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.Apply(); err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	// Flags beat the file and the environment, so they are applied last and
	// directly.
	if *noShaping {
		cfg.Link.Shaping = false
	}
	mocked := map[string]bool{}
	if *mockPico {
		mocked["pico"] = true
	}
	if *mockCamera {
		mocked["camera"] = true
	}
	if *mockSDR {
		mocked["sdr"] = true
	}

	if len(mocked) > 0 {
		names := make([]string, 0, len(mocked))
		for k := range mocked {
			names = append(names, k)
		}
		log.Warn("SIMULATED SUBSYSTEMS -- the telemetry from these is fabricated",
			"subsystems", names)
	}

	log.Info("ROCSAR On-Board Computer starting",
		"control", cfg.Server.ControlEndpoint,
		"telemetry", cfg.Server.TelemetryEndpoint,
		"http", cfg.HTTP.Addr,
		"data", cfg.HTTP.Root)

	// ---------------------------------------------------------------------
	// Storage first: an unwritable data directory is the one fatal-at-startup
	// failure. Everything written under it is flight data, and losing it
	// silently is worse than not starting. See ARCHITECTURE.md 8.
	// ---------------------------------------------------------------------
	store := storage.New(cfg.HTTP.Root)
	if err := store.Check(); err != nil {
		return fmt.Errorf("data directory: %w", err)
	}

	// ---------------------------------------------------------------------
	// GNSS
	// ---------------------------------------------------------------------
	gnssBank, err := gnss.NewBank(cfg.GNSS.Ports, cfg.GNSS.Selected, cfg.GNSS.StaleAfter, log)
	if err != nil {
		return err
	}
	if err := gnssBank.Start(ctx); err != nil {
		return err
	}
	defer gnssBank.Close()

	// ---------------------------------------------------------------------
	// Flight controller
	// ---------------------------------------------------------------------
	var picoPort domain.Pico = pico.NewMock()
	if mocked["pico"] {
		log.Warn("using a simulated flight controller")
	} else {
		// The Link is a protocol layer. It does not open the port and it does not
		// close it: NewSerialTransport returns an unopened port precisely so the
		// composition root decides when the device is touched, and Link.Shutdown
		// stops the protocol without reaching past its transport.
		//
		// That separation is only worth anything if both halves are actually
		// called. OpenPort was missing, so the link handshook with a port it never
		// opened and every start degraded with "serial transport is not open" --
		// which reads like a missing device and is not one.
		serial := pico.NewSerialTransport(cfg.Pico.Port, cfg.Pico.Baudrate)

		switch {
		case !serial.Exists():
			msg := fmt.Sprintf("no flight controller at %s", cfg.Pico.Port)
			if cfg.RequireHardware {
				return errors.New(msg)
			}
			log.Error(msg + "; continuing degraded")

		default:
			// A node that exists but will not open is a different fault from a
			// node that is not there: wrong permissions, already in use, or it
			// enumerated as something else. It gets its own message because the
			// operator action is different.
			if err := serial.OpenPort(); err != nil {
				msg := fmt.Sprintf("cannot open flight controller at %s: %v", cfg.Pico.Port, err)
				if cfg.RequireHardware {
					return errors.New(msg)
				}
				log.Error(msg + "; continuing degraded")
				break
			}
			// Registered before the link so the port is closed after it, not
			// before. Both are idempotent, so shutdown order is not load-bearing,
			// but a port closed out from under a running read loop is a race
			// nobody needs to have.
			defer func() { _ = serial.Close() }()

			link := pico.NewLink(serial, log)
			if err := link.Open(ctx); err != nil {
				// Degrade rather than abort, unless the operator asked otherwise.
				// A missing flight controller must be visible in telemetry, not
				// fatal -- except when --require-hardware says it should be.
				if cfg.RequireHardware {
					return fmt.Errorf("flight controller at %s: %w", cfg.Pico.Port, err)
				}
				log.Error("flight controller did not answer; continuing degraded",
					"port", cfg.Pico.Port, "err", err)
			} else {
				log.Info("flight controller connected", "port", cfg.Pico.Port, "baud", cfg.Pico.Baudrate)
			}
			picoPort = link
		}
	}

	// ---------------------------------------------------------------------
	// Camera
	// ---------------------------------------------------------------------
	var cam domain.Camera = camera.NewMock(cfg.Camera.Device, "photos", store)
	if mocked["camera"] {
		log.Warn("using a simulated camera")
	} else {
		real := camera.NewCapture(cfg.Camera.Device, "photos", store, 85)
		cam = real
	}

	// ---------------------------------------------------------------------
	// SDR
	// ---------------------------------------------------------------------
	var sdrPort domain.Sdr = sdr.NewMock()
	if mocked["sdr"] {
		log.Warn("using a simulated SDR")
	} else {
		sdrPort = sdr.NewService(cfg.SDR.Program, store.Root(), log)
	}

	// ---------------------------------------------------------------------
	// Link shaping
	// ---------------------------------------------------------------------
	var shaper domain.LinkShaper = qos.NewNullShaper("link shaping disabled by configuration")
	if cfg.Link.Shaping {
		tc := qos.NewShaper(log, nil)
		applied, reason := tc.Apply(ctx, cfg.Link.Device, cfg.Link.RateKbps)
		if applied {
			shaper = tc
			if reason != "" {
				log.Warn("link shaping applied with a caveat", "reason", reason)
			}
		} else {
			// Reported, not fatal. tc fails for dozens of reasons unrelated to
			// our logic and none of them justify taking down a telemetry server.
			log.Warn("link shaping unavailable; continuing unshaped", "reason", reason)
		}
	}

	// ---------------------------------------------------------------------
	// Telemetry
	// ---------------------------------------------------------------------
	engine := telemetry.NewEngine(telemetry.Providers{
		System: func() telemetry.SystemSnapshot {
			return telemetry.SystemSnapshot{
				State:   domain.SubsystemReady,
				Uptime:  time.Since(startedAt),
				CPUTemp: cpuTemperature(),
				Mocked:  mockedNames(mocked),
			}
		},
		GNSS:     gnssBank.Status,
		Pico:     picoPort.Telemetry,
		PicoConn: picoPort.Connected,
		PicoAck: func() *domain.Ack {
			if link, ok := picoPort.(*pico.Link); ok {
				if ack := link.LastAck(); ack != nil {
					return &domain.Ack{
						CommandSequence: ack.GetCommandSequence(),
						Success:         ack.GetSuccess(),
						Error:           domain.ErrorCode(ack.GetError()),
					}
				}
			}
			return nil
		},
		SDR: func() telemetry.SDRSnapshot {
			return telemetry.SDRSnapshot{
				State:   sdrPort.State(),
				Running: sdrPort.Running(),
				PID:     sdrPort.PID(),
				LastLog: sdrPort.LastLog(),
			}
		},
		Camera:     cam.State,
		CameraDev:  cam.Device,
		CameraLast: func() (string, uint64) { return "", cam.PhotosTaken() },
		Link:       shaper.Status,
	}, time.Now)

	// ---------------------------------------------------------------------
	// Transport
	// ---------------------------------------------------------------------
	dispatcher := command.New(command.Deps{
		Pico:   picoPort,
		GNSS:   gnssBank,
		Camera: cam,
		SDR:    sdrPort,
		Link:   shaper,
		Log:    log,
	})

	zmq := transport.NewZMQ(log, func(req *rocsarv1.CommandRequest) *rocsarv1.CommandResponse {
		return dispatcher.Handle(ctx, req)
	})
	if err := zmq.Start(ctx, cfg.Server.ControlEndpoint, cfg.Server.TelemetryEndpoint); err != nil {
		return err
	}
	defer zmq.Stop()

	files := transport.NewHTTP(cfg.HTTP.Addr, store, log, cfg.QOS.BulkRateBps)
	if err := files.Start(ctx); err != nil {
		return err
	}
	defer files.Stop()

	pipeline := telemetry.NewPipeline(engine, cfg.Telemetry.Interval, func(snap telemetry.Snapshot) {
		frame := transport.EncodeTelemetry(snap)
		body, err := proto.Marshal(frame)
		if err != nil {
			log.Error("telemetry frame did not serialise", "err", err)
			return
		}
		zmq.Publish(qos.TopicTelemetry, body)
	}, log)

	go pipeline.Run(ctx)

	log.Info("ROCSAR On-Board Computer operational",
		"mocked", mockedNames(mocked),
		"telemetry_hz", 1/cfg.Telemetry.Interval.Seconds())

	<-ctx.Done()
	log.Info("shutting down")

	// Shutdown in reverse construction order, and only through interfaces.
	// A type assertion to *sdr.Service here would panic on the mock path, which
	// is precisely the path a developer runs most often.
	if closer, ok := sdrPort.(interface{ Close() error }); ok {
		if err := closer.Close(); err != nil {
			log.Warn("stopping the SDR failed", "err", err)
		}
	}
	if closer, ok := picoPort.(domain.Shutdown); ok {
		_ = closer.Shutdown(context.Background())
	}

	log.Info("stopped")
	return nil
}

var startedAt = time.Now()

func mockedNames(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		if v {
			out = append(out, k)
		}
	}
	return out
}

// cpuTemperature reads the SoC temperature.
//
// Returns 0 on any failure rather than a placeholder. A telemetry field that
// invents a plausible temperature is worse than one that reports nothing, so the
// consumer's job is to know that 0 means "unavailable" -- and on a Pi that is
// never a real temperature.
func cpuTemperature() float64 {
	const path = "/sys/class/thermal/thermal_zone0/temp"
	body, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var milli int64
	if _, err := fmt.Sscanf(string(body), "%d", &milli); err != nil {
		return 0
	}
	return float64(milli) / 1000
}

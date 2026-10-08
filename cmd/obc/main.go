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
	store := storage.New(cfg.HTTP.Root, cfg.HTTP.Device)
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
	picoPort, closePicoPort, err := openPico(ctx, cfg, log, mocked["pico"])
	if err != nil {
		return err
	}
	// Registered before anything that depends on the port being open, so it runs
	// after them: LIFO unwinding stops the link first and closes the device
	// second. A port closed out from under a running read loop is a race nobody
	// needs to have.
	defer closePicoPort()

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
		sdrPort = sdr.NewService(cfg.SDR.Program, store.Root(), cfg.SDR.DataDir, log)
	}

	// ---------------------------------------------------------------------
	// Link shaping
	//
	// This is now the ONLY limiter. The token bucket that used to pace the
	// artefact HTTP path inside this process is gone, so if the kernel tree is not
	// on the device the link is unbounded and nothing else here would know.
	// That is why the verify loop below exists rather than being a nicety.
	// ---------------------------------------------------------------------
	var shaper domain.LinkShaper = qos.NewNullShaper("link shaping disabled by configuration")
	if !cfg.Link.Shaping {
		// Still tell it what the cap would have been, so telemetry and the Ground
		// Station can show the operator what this link is meant to be limited to
		// rather than reporting a rate of zero.
		if null, ok := shaper.(*qos.NullShaper); ok {
			null.Configure(cfg.Link.Device, cfg.Link.RateKbps)
		}
		log.Error("link shaping is OFF in configuration, so the link is not limited at all",
			"device", cfg.Link.Device, "would_have_been_kbps", cfg.Link.RateKbps,
			"note", "nothing else in this program bounds the link now; the in-process limiter was removed")
	} else {
		tc := qos.NewShaper(log, nil)
		applied, reason := tc.Apply(ctx, cfg.Link.Device, cfg.Link.RateKbps)
		if applied {
			shaper = tc
			if reason != "" {
				log.Warn("link shaping applied with a caveat", "reason", reason)
			}
		} else {
			// Reported, not fatal -- ARCHITECTURE.md 8 says never refuse to serve
			// telemetry. But this used to mean "artefact bytes unbounded" and now
			// means "the link is unbounded", so it is an error and says so.
			log.Error("link shaping could not be installed; THE LINK IS UNBOUNDED",
				"reason", reason, "device", cfg.Link.Device,
				"note", "traffic control needs CAP_NET_ADMIN; grant it to the obc unit")
			// Keep the shaper so the verify loop can try again: a capability
			// granted while the OBC is running, or a device that appears later, is
			// then recoverable without a restart.
			shaper = tc
		}
		go verifyShaping(ctx, shaper, log)
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
				// The reason the last acquisition stopped, when it stopped badly.
				// This was the one field the frame carried and no provider ever
				// filled, so a crashed acquisition read as an idle SDR that
				// happened to have a PID.
				LastError: sdrPort.LastError(),
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
	// The host half of the heater dead-man. The firmware turns off anything it
	// has not heard about within HEATER_AUTO_OFF_MS, so this is what keeps a
	// heater on across the seconds between one operator command and the next.
	heaterKeeper := command.NewHeaterKeeper(picoPort, log, command.HeaterRefreshInterval)

	var dispatcher *command.Dispatcher
	zmq := transport.NewZMQ(log, func(req *rocsarv1.CommandRequest) *rocsarv1.CommandResponse {
		return dispatcher.Handle(ctx, req)
	})

	pipeline := telemetry.NewPipeline(engine, cfg.Telemetry.Interval, func(snap telemetry.Snapshot) {
		frame := transport.EncodeTelemetry(snap)
		body, err := proto.Marshal(frame)
		if err != nil {
			log.Error("telemetry frame did not serialise", "err", err)
			return
		}
		zmq.Publish(qos.TopicTelemetry, body)
	}, log)

	dispatcher = command.New(command.Deps{
		Pico:                 picoPort,
		GNSS:                 gnssBank,
		Camera:               cam,
		SDR:                  sdrPort,
		Link:                 shaper,
		Heaters:              heaterKeeper,
		Log:                  log,
		SetTelemetryInterval: pipeline.SetInterval,
	})
	go dispatcher.Run(ctx)

	if err := zmq.Start(ctx, cfg.Server.ControlEndpoint, cfg.Server.TelemetryEndpoint); err != nil {
		return err
	}
	defer zmq.Stop()

	files := transport.NewHTTP(cfg.HTTP.Addr, store, log)
	if err := files.Start(ctx); err != nil {
		return err
	}
	defer files.Stop()

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

// openPico decides which flight controller the system runs against.
//
// Three outcomes, and only three:
//
//   - asked to simulate: a mock, and the caller names it in the telemetry frame;
//   - the device is there: the real link, opened and proved with a
//     status_request round trip;
//   - the device is absent, will not open, or will not answer: the real link,
//     unopened. It reports Connected() == false, holds no telemetry, and refuses
//     every command with ErrNotConnected.
//
// The third case is the easy one to get wrong, and it was. This block used to
// seed the variable with a mock and overwrite it on the happy path, so every
// degraded branch left the mock installed: an absent Pico reported
// pico_connected: true and answered jog, heading and heater commands with
// "acknowledged by the flight controller" against no hardware -- and because the
// mock was reached by a `break` rather than by --mock-pico, "pico" was absent
// from the mocked set, so nothing in telemetry said so either. That is the
// failure mode ARCHITECTURE.md 9 exists to prevent, running in the opposite
// direction, and it is exactly what --mock-gnss used to do.
//
// So a mock is constructed in exactly one place: when it is asked for by name.
// Everywhere else the real Link is installed, opened or not, because an unopened
// Link already *is* the degraded adapter.
//
// The returned func closes the serial port if this call opened one, and is never
// nil. Closing is separate from opening because the Link deliberately does not
// own the port: it is a protocol layer, and Link.Shutdown stops the protocol
// without reaching past its transport.
func openPico(ctx context.Context, cfg config.Config, log *slog.Logger, mock bool) (domain.Pico, func(), error) {
	noop := func() {}

	if mock {
		log.Warn("using a simulated flight controller")
		return pico.NewMock(), noop, nil
	}

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

	// Built before the switch and returned whichever way the switch goes. There
	// is no branch in which the caller is handed anything but this or a mock
	// that was asked for.
	link := pico.NewLink(serial, log)

	switch {
	case !serial.Exists():
		msg := fmt.Sprintf("no flight controller at %s", cfg.Pico.Port)
		if cfg.RequireHardware {
			return nil, noop, errors.New(msg)
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
				return nil, noop, errors.New(msg)
			}
			log.Error(msg + "; continuing degraded")
			break
		}

		if err := link.Open(ctx); err != nil {
			// Degrade rather than abort, unless the operator asked otherwise.
			// A missing flight controller must be visible in telemetry, not
			// fatal -- except when --require-hardware says it should be.
			if cfg.RequireHardware {
				return nil, func() { _ = serial.Close() }, fmt.Errorf("flight controller at %s: %w", cfg.Pico.Port, err)
			}
			log.Error("flight controller did not answer; continuing degraded",
				"port", cfg.Pico.Port, "err", err)
		} else {
			log.Info("flight controller connected", "port", cfg.Pico.Port, "baud", cfg.Pico.Baudrate)
		}
	}

	return link, func() { _ = serial.Close() }, nil
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

// verifyShapingInterval is how often the kernel is asked whether the cap is still
// on the device.
//
// Slow on purpose. The failure it detects -- NetworkManager, systemd-networkd, a
// DHCP renewal or a hand-run tc replacing the root qdisc -- takes seconds to
// matter and is not high frequency, while every read is a netlink round trip on
// the same box that is trying to fly an aircraft. Ten seconds is well inside the
// time telemetry itself goes stale at, so an unbounded window is never longer
// than the window in which the console is already warning the operator.
const verifyShapingInterval = 10 * time.Second

// verifyShaping re-reads the traffic control state and repairs it if it is gone.
//
// This is not defensive bookkeeping; with the in-process limiter removed it is
// the difference between a capped link and an uncapped one. Active and Status
// answer from flags written when the tree was installed, so without this a
// qdisc replaced by anything else would leave telemetry reporting
// shaping_active = true over a link nothing is limiting.
//
// It also covers a start that failed. An OBC without CAP_NET_ADMIN reports
// itself unshaped and keeps serving telemetry, and if the capability is granted
// while it is running -- or the device appears, or the rate was clamped -- the
// next tick installs it without a restart.
func verifyShaping(ctx context.Context, shaper domain.LinkShaper, log *slog.Logger) {
	ticker := time.NewTicker(verifyShapingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			active, reason := shaper.Verify(ctx)
			if !active {
				log.Error("the link is NOT being limited", "reason", reason,
					"note", "telemetry, commands and artefact downloads are all unbounded")
			}
		}
	}
}

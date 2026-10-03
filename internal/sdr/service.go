// Package sdr drives the vendored Ettus B200mini acquisition program.
//
// The shape is obc_rocsar's sdr_service.go, cleaned up: parameters/params.json is
// read and written, ./connect is started detached with its output to a log, and
// uhd_usrp_probe and uhubctl are shelled out to for diagnosis.
//
// os/exec appears here and in internal/qos, and nowhere else -- enforced by
// test/layering_test.go. Every other part of the OBC reaches hardware through an
// interface, so this is the only place where "run a program" is a normal thing
// to do.
package sdr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rocsar/obc/internal/domain"
	"github.com/rocsar/obc/internal/storage"
)

// CommandTimeout bounds every external invocation.
//
// uhubctl and uhd_usrp_probe both talk to USB devices that can be in a state
// where they never answer. Without a bound the OBC blocks in a kernel call and
// stops being a telemetry server.
const CommandTimeout = 30 * time.Second

// Errors.
var (
	ErrAlreadyRunning = errors.New("sdr: an acquisition is already running")
	ErrNoProgram      = errors.New("sdr: acquisition program not found")
	ErrInvalidParams  = errors.New("sdr: parameter out of range")
)

// Service manages the acquisition program.
type Service struct {
	programDir string
	logDir     string
	log        *slog.Logger

	// runner is injected so the whole service is testable without a USB device.
	runner func(ctx context.Context, dir, name string, args []string) (stdout string, err error)
	start  func(ctx context.Context, dir, name string, args []string, out *os.File) (pid int, err error)

	mu        sync.Mutex
	pid       int
	running   bool
	startedAt time.Time
	lastLog   string
	lastErr   string
	lastOut   string
	state     domain.SubsystemState
}

var _ domain.Sdr = (*Service)(nil)

// NewService returns a service driving the program at programDir, writing logs
// under logDir (a subdirectory of the data directory).
func NewService(programDir, logDir string, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		programDir: programDir,
		logDir:     logDir,
		log:        log,
		runner:     runCommand,
		start:      startDetached,
		state:      domain.SubsystemDisconnected,
	}
}

// programPath is the acquisition binary.
func (s *Service) programPath() string { return filepath.Join(s.programDir, "connect") }

// paramsPath is the parameter file the program reads.
func (s *Service) paramsPath() string {
	return filepath.Join(s.programDir, "parameters", "params.json")
}

// Params reads the current parameters.
func (s *Service) Params(ctx context.Context) (domain.SdrParams, error) {
	var out domain.SdrParams

	body, err := os.ReadFile(s.paramsPath())
	if err != nil {
		return out, fmt.Errorf("sdr: read params.json: %w", err)
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("sdr: parse params.json: %w", err)
	}
	return out, nil
}

// SetParams applies a partial update and writes the file atomically.
//
// The validation is the point of this method. params.json is a frozen contract
// with C++ we do not maintain; a value the program cannot use is a bricked SDR
// discovered on the bench, and a partially written file is a bricked SDR with no
// way back. So: validate first, write second, atomically.
func (s *Service) SetParams(ctx context.Context, patch domain.SdrParamsPatch) error {
	current, err := s.Params(ctx)
	if err != nil {
		return err
	}

	apply := &current
	if patch.PRFHz != nil {
		if err := inRange("PRF", *patch.PRFHz, 1, 1e7); err != nil {
			return err
		}
		apply.PRFHz = *patch.PRFHz
	}
	if patch.SampleRateHz != nil {
		if err := inRange("FS", *patch.SampleRateHz, 1e5, 1e9); err != nil {
			return err
		}
		apply.SampleRateHz = *patch.SampleRateHz
	}
	if patch.TxFreqHz != nil {
		if err := inRange("TX_FREQ", *patch.TxFreqHz, 1e6, 6e9); err != nil {
			return err
		}
		apply.TxFreqHz = *patch.TxFreqHz
	}
	if patch.NormalizedGainTx != nil {
		if err := inRange("NORMALIZED_GAIN_TX", *patch.NormalizedGainTx, 0, 1); err != nil {
			return err
		}
		apply.NormalizedGainTx = *patch.NormalizedGainTx
	}
	if patch.NormalizedGainRx != nil {
		if err := inRange("NORMALIZED_GAIN_RX", *patch.NormalizedGainRx, 0, 1); err != nil {
			return err
		}
		apply.NormalizedGainRx = *patch.NormalizedGainRx
	}
	if patch.PulseDurationS != nil {
		if err := inRange("PULSE_DURATION", *patch.PulseDurationS, 1e-9, 1e-3); err != nil {
			return err
		}
		apply.PulseDurationS = *patch.PulseDurationS
	}
	if patch.BandwidthHz != nil {
		if err := inRange("BW", *patch.BandwidthHz, 1e5, 1e9); err != nil {
			return err
		}
		apply.BandwidthHz = *patch.BandwidthHz
	}
	if patch.SessionDurationS != nil {
		if *patch.SessionDurationS == 0 || *patch.SessionDurationS > 86400 {
			return fmt.Errorf("%w: SESSION_DURATION must be 1..86400 seconds", ErrInvalidParams)
		}
		apply.SessionDurationS = *patch.SessionDurationS
	}

	// Marshal through a generic map so the keys the vendored C++ expects survive
	// even if this struct does not name them all. Encoding domain.SdrParams
	// directly would silently DROP every key we have not modelled, and the
	// program has no default for a missing one.
	raw := map[string]any{}
	if err := json.Unmarshal(mustRead(s.paramsPath()), &raw); err != nil {
		return fmt.Errorf("sdr: parse params.json: %w", err)
	}
	raw["PRF"] = apply.PRFHz
	raw["FS"] = apply.SampleRateHz
	raw["TX_FREQ"] = apply.TxFreqHz
	raw["NORMALIZED_GAIN_TX"] = apply.NormalizedGainTx
	raw["NORMALIZED_GAIN_RX"] = apply.NormalizedGainRx
	raw["PULSE_DURATION"] = apply.PulseDurationS
	raw["BW"] = apply.BandwidthHz
	raw["SESSION_DURATION"] = apply.SessionDurationS

	body, err := json.MarshalIndent(raw, "", "    ")
	if err != nil {
		return fmt.Errorf("sdr: encode params.json: %w", err)
	}

	store := storage.New(filepath.Dir(s.paramsPath()))
	if err := store.WriteFileAtomic("params.json", body, 0o644); err != nil {
		return fmt.Errorf("sdr: %w", err)
	}

	s.log.Info("SDR parameters updated", "prf", apply.PRFHz, "fs", apply.SampleRateHz, "tx", apply.TxFreqHz)
	return nil
}

func inRange(name string, v, lo, hi float64) error {
	if v < lo || v > hi {
		return fmt.Errorf("%w: %s = %g is outside %g..%g", ErrInvalidParams, name, v, lo, hi)
	}
	return nil
}

func mustRead(path string) []byte {
	b, _ := os.ReadFile(path)
	return b
}

// Connect starts the acquisition program, detached.
//
// It refuses to start a second one while a first is running, and says which PID
// is holding it. Starting two acquisitions against one SDR produces two
// half-written files and an unusable device, so the refusal names the process
// rather than just declining.
func (s *Service) Connect(ctx context.Context) error {
	s.mu.Lock()
	if s.running && s.pid > 0 && processAlive(s.pid) {
		held := s.pid
		s.mu.Unlock()
		return fmt.Errorf("%w: pid %d since %s", ErrAlreadyRunning, held,
			s.startedAt.Format(time.RFC3339))
	}
	s.mu.Unlock()

	program := s.programPath()
	if _, err := os.Stat(program); err != nil {
		return fmt.Errorf("%w: %s", ErrNoProgram, program)
	}

	logDir, err := storage.New(s.logDir).Sub("sdr")
	if err != nil {
		return err
	}

	name := fmt.Sprintf("connect-%s.log", time.Now().Format("20060102-150405"))
	logPath := filepath.Join(logDir.Root(), name)

	f, err := os.Create(logPath)
	if err != nil {
		return fmt.Errorf("sdr: create log %s: %w", logPath, err)
	}

	// The child is started with the program's own directory as its working
	// directory. The vendored C++ writes its output relative to CWD
	// (./Data/rx_data_<time>.bin), so inheriting the OBC's directory puts every
	// capture somewhere the HTTP listing does not serve.
	pid, err := s.start(ctx, s.programDir, program, nil, f)
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("sdr: start %s: %w", program, err)
	}
	_ = f.Close() // the child holds its own descriptor

	s.mu.Lock()
	s.pid = pid
	s.running = true
	s.startedAt = time.Now()
	s.lastLog = logPath
	s.lastErr = ""
	s.lastOut = ""
	s.state = domain.SubsystemBusy
	s.mu.Unlock()

	s.log.Info("SDR acquisition started", "pid", pid, "log", logPath, "cwd", s.programDir)
	return nil
}

// Running reports whether an acquisition started by this service is alive.
//
// Checked with a signal-0 probe rather than trusting the recorded PID: the
// process may have finished, crashed, or been killed, and a stale `running`
// flag means every subsequent Connect is refused with "already running".
func (s *Service) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running {
		return false
	}
	alive := s.pid > 0 && processAlive(s.pid)
	if !alive && s.state == domain.SubsystemBusy {
		s.state = domain.SubsystemReady
	}
	s.running = alive
	return alive
}

func (s *Service) PID() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int64(s.pid)
}

func (s *Service) LastLog() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastLog
}

func (s *Service) LastOutput() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastOut
}

// Probe returns uhd_usrp_probe output verbatim.
//
// Verbatim on purpose: the operator reads this to diagnose hardware, and a
// summarised or filtered version hides the line that says what is wrong.
func (s *Service) Probe(ctx context.Context) (string, error) {
	out, err := s.runner(ctx, "", "uhd_usrp_probe", nil)
	if err != nil {
		return out, fmt.Errorf("sdr: uhd_usrp_probe: %w", err)
	}
	return out, nil
}

// ResetUSB power-cycles the SDR's USB port.
func (s *Service) ResetUSB(ctx context.Context) error {
	// Two toggles with a pause: one power cycle is not always enough for the
	// Ettus to re-enumerate, and a single toggle leaves it in a state where the
	// next probe fails for a different reason.
	out, err := s.runner(ctx, "", "sh", []string{"-c", "uhubctl -a toggle -l 2; sleep 4; uhubctl -a toggle -l 2"})
	if err != nil {
		return fmt.Errorf("sdr: uhubctl: %w: %s", err, strings.TrimSpace(out))
	}
	s.log.Warn("SDR USB port power-cycled")
	return nil
}

func (s *Service) State() domain.SubsystemState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Stop terminates a running acquisition.
func (s *Service) Stop(ctx context.Context) error {
	s.mu.Lock()
	pid := s.pid
	s.mu.Unlock()

	if pid <= 0 || !processAlive(pid) {
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("sdr: signal %d: %w", pid, err)
	}

	// Bounded wait, then SIGKILL. An acquisition that ignores SIGTERM would
	// otherwise keep the device busy forever.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		return fmt.Errorf("sdr: kill %d: %w", pid, err)
	}
	return nil
}

// Close satisfies domain.Shutdown.
func (s *Service) Close() error { return s.Stop(context.Background()) }

func (s *Service) SetState(st domain.SubsystemState) {
	s.mu.Lock()
	s.state = st
	s.mu.Unlock()
}

// processAlive reports whether a PID is running.
//
// Signal 0 performs the permission and existence checks without delivering
// anything. EPERM means the process exists but belongs to someone else, which
// for our purposes is alive -- treating it as dead would let a second
// acquisition start against a device that is already busy.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// runCommand runs a foreground command and returns its combined output.
func runCommand(ctx context.Context, dir, name string, args []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, CommandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// startDetached starts a long-running process and returns its PID without
// waiting.
//
// The child is put in its own process group so a Ctrl-C on the OBC's console
// does not take the acquisition down mid-capture, and its stdio is redirected to
// the log file rather than inherited.
func startDetached(ctx context.Context, dir, name string, args []string, out *os.File) (int, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return 0, err
	}
	return cmd.Process.Pid, nil
}

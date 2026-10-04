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

// loadParams reads params.json ONCE and returns both views of it.
//
// One read, and this matters. The first version read the file twice -- once
// through Params() to get the values it validated, and again through a mustRead()
// that discarded its error to get the map it wrote. Two reads of a file that
// something else may be editing means the values validated are not necessarily
// the values written.
//
// raw is the file as it stands, including every key this package does not model.
// That map is what gets written back, which is why T_MIN_US, T_MAX_US,
// START_OFFSET_S, TX_ANTENNA and RX_ANTENNA survive a partial update: encoding
// the typed struct instead would drop all five, and load_config()'s j.at() would
// throw on the next ./connect with no way back from the air.
func (s *Service) loadParams() (domain.SdrParams, map[string]any, error) {
	var typed domain.SdrParams

	body, err := os.ReadFile(s.paramsPath())
	if err != nil {
		return typed, nil, fmt.Errorf("sdr: read params.json: %w", err)
	}

	raw := map[string]any{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return typed, nil, fmt.Errorf("sdr: parse params.json: %w", err)
	}

	// The typed view is decoded from the same bytes, not a second read. It is
	// used for reporting and for validation; the map is what gets written.
	if err := json.Unmarshal(body, &typed); err != nil {
		return typed, nil, fmt.Errorf("sdr: parse params.json: %w", err)
	}

	if err := checkRequired(raw); err != nil {
		return typed, nil, err
	}

	return typed, raw, nil
}

// Params reads the current parameters.
func (s *Service) Params(ctx context.Context) (domain.SdrParams, error) {
	typed, _, err := s.loadParams()
	return typed, err
}

// checkRequired fails when a key load_config() will j.at() on is absent.
func checkRequired(raw map[string]any) error {
	var missing []string
	for _, name := range RequiredNames() {
		if _, ok := raw[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrMissingKeys, strings.Join(missing, ", "))
}

// ErrMissingKeys is returned when params.json lacks a key the C++ requires.
var ErrMissingKeys = errors.New("sdr: params.json is missing keys that connect.cpp requires")

// SetParams applies a partial update and writes the file atomically.
//
// Validation is against the table in params_contract.go, whose bounds come from
// the Ettus datasheet and say so per entry. The write is atomic because a
// half-written params.json is a bricked SDR.
func (s *Service) SetParams(ctx context.Context, patch domain.SdrParamsPatch) error {
	current, raw, err := s.loadParams()
	if err != nil {
		return err
	}

	// Validate the INCOMING value before applying it, so the error names what
	// was asked for rather than what it would have become.
	for _, v := range []struct {
		key    string
		ptr    *float64
		assign func(float64)
	}{
		{"PRF", patch.PRFHz, func(f float64) { current.PRFHz = f }},
		{"FS", patch.SampleRateHz, func(f float64) { current.SampleRateHz = f }},
		{"TX_FREQ", patch.TxFreqHz, func(f float64) { current.TxFreqHz = f }},
		{"NORMALIZED_GAIN_TX", patch.NormalizedGainTx, func(f float64) { current.NormalizedGainTx = f }},
		{"NORMALIZED_GAIN_RX", patch.NormalizedGainRx, func(f float64) { current.NormalizedGainRx = f }},
		{"BW", patch.BandwidthHz, func(f float64) { current.BandwidthHz = f }},
	} {
		if v.ptr == nil {
			continue
		}
		if err := ValidateValue(v.key, *v.ptr); err != nil {
			return err
		}
		v.assign(*v.ptr)
	}
	if patch.SessionDurationS != nil {
		if err := ValidateValue("SESSION_DURATION", float64(*patch.SessionDurationS)); err != nil {
			return err
		}
		current.SessionDurationS = *patch.SessionDurationS
	}

	// Overlay onto the file as it stands. Absent from the overlay means absent
	// from the patch means "leave alone", and every key we do not name is
	// carried through untouched.
	raw["PRF"] = current.PRFHz
	raw["FS"] = current.SampleRateHz
	raw["TX_FREQ"] = current.TxFreqHz
	raw["NORMALIZED_GAIN_TX"] = current.NormalizedGainTx
	raw["NORMALIZED_GAIN_RX"] = current.NormalizedGainRx
	raw["BW"] = current.BandwidthHz
	raw["SESSION_DURATION"] = current.SessionDurationS

	// The whole file is rewritten, so the diff is the whole file.
	//
	// Two things change cosmetically and neither affects the program, which reads
	// by key with j.at() and does not care about order:
	//
	//   - Go marshals a map in sorted key order, so the keys come out
	//     alphabetically instead of grouped as they were.
	//   - Floats are re-formatted: 3.64e-05 becomes 0.0000364.
	//
	// Preserving the original ordering would mean carrying an ordered key list
	// through the read, patch and write for no gain the program can see. It is
	// noted here so a large-looking diff on a one-line change is not a surprise.
	body, err := json.MarshalIndent(raw, "", "    ")
	if err != nil {
		return fmt.Errorf("sdr: encode params.json: %w", err)
	}

	// Re-check after the overlay: a patch cannot remove a key, but a file that
	// was already broken should not be written back looking healthy.
	if err := checkRequired(raw); err != nil {
		return err
	}

	store := storage.New(filepath.Dir(s.paramsPath()))
	if err := store.WriteFileAtomic("params.json", body, 0o644); err != nil {
		return fmt.Errorf("sdr: %w", err)
	}

	s.log.Info("SDR parameters updated",
		"prf", current.PRFHz, "fs", current.SampleRateHz, "tx", current.TxFreqHz)
	return nil
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

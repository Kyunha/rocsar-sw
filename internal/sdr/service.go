// Package sdr drives the Ettus B200mini acquisition program.
//
// The shape is obc_rocsar's sdr_service.go, cleaned up: parameters/params.json is
// read and written, connect is started detached with its output to a log, and
// uhd_usrp_probe and uhubctl are shelled out to for diagnosis.
//
// The program itself is found on PATH, because the deployed system installs it
// there rather than shipping it beside the OBC binary. sdr.program names the
// directory holding parameters/ and Data/ -- not the executable -- and that
// directory is also the working directory the child is started in, because the
// C++ resolves both its configuration and its capture paths against CWD. The
// two path derivations are checked against each other on every start; see
// programParamsPath for why that is not optional.
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

// programName is the acquisition binary's own name, and the string looked up on
// PATH. The deployed system installs it there (a symlink into
// /usr/local/bin), so the OBC no longer has to ship next to it.
const programName = "connect"

// programConfigRelPath is where the program looks for its own configuration,
// relative to its working directory.
//
// Taken verbatim from connect.cpp:
//
//	const Config cfg = load_config("./../sdr-ettus-b200mini/parameters/params.json");
//
// That line is a hardcoded relative path in C++ this repository does not
// maintain, so it -- not paramsPath() below -- decides which file a capture
// actually uses. TestConfigPathIsHardcodedRelativeToCWD pins both copies to
// connect.cpp: if that line changes, that test fails rather than this constant
// quietly going stale.
const programConfigRelPath = "../sdr-ettus-b200mini/parameters/params.json"

// programPath is the acquisition binary, looked up on PATH.
//
// PATH first, then next to the parameters. The program is installed on PATH
// (a symlink into /usr/local/bin) so a binary scp'd onto the OBC can drive a
// program built and installed separately, and the fallback keeps a checkout
// with the vendored tree still working without installing anything.
func (s *Service) programPath() (string, error) {
	if path, err := exec.LookPath(programName); err == nil {
		return path, nil
	}
	local := filepath.Join(s.programDir, programName)
	if _, err := os.Stat(local); err != nil {
		return "", fmt.Errorf("%w: %s is not on PATH and there is no %s",
			ErrNoProgram, programName, local)
	}
	return local, nil
}

// LookProgram reports where the acquisition binary would be run from, without
// starting it. Exported for sdr_bench, so an operator can confirm the OBC will
// find the program they think it will before they are on the aircraft.
func LookProgram(programDir string) (string, error) {
	return (&Service{programDir: programDir}).programPath()
}

// paramsPath is the parameter file the program reads.
func (s *Service) paramsPath() string {
	return filepath.Join(s.programDir, "parameters", "params.json")
}

// ParamsPath is where this Service reads and writes parameters/params.json.
//
// Exported so a tool reports on the same file the server edits. sdr_bench used
// to build the path itself, which is a second home for a fact that has to match
// the server's exactly -- and the C++ has a third opinion, see programParamsPath.
func (s *Service) ParamsPath() string { return s.paramsPath() }

// CheckProgramConfig reports whether the program and this Service would agree on
// which params.json is authoritative, without starting anything.
//
// The check belongs here rather than only inside Connect so an operator can run
// it on the bench, where a disagreement is cheap to fix, rather than
// discovering it in the air.
func (s *Service) CheckProgramConfig() error {
	_, err := s.programParamsPath()
	return err
}

// programParamsPath returns the params.json the program will itself open, and
// refuses if that is not the file this Service reads and writes.
//
// Both sides derive a path from s.programDir, and they must derive the SAME
// one. Go builds <programDir>/parameters/params.json; the C++ resolves
// programConfigRelPath against the working directory, which reaches
// <programDir> again only because the directory is named sdr-ettus-b200mini.
// Put programDir anywhere else and the two disagree: SetParams would report a
// successful write to one file while the program kept using another, so an
// operator's gain and PRF changes would appear to save and do nothing.
//
// The only honest fix is upstream -- the C++ taking a path argument. Until
// then this is checked on every start rather than discovered on the bench.
func (s *Service) programParamsPath() (string, error) {
	cwd, err := filepath.Abs(s.programDir)
	if err != nil {
		return "", fmt.Errorf("sdr: resolve %s: %w", s.programDir, err)
	}
	program := filepath.Clean(filepath.Join(cwd, programConfigRelPath))
	ours, err := filepath.Abs(s.paramsPath())
	if err != nil {
		return "", fmt.Errorf("sdr: resolve %s: %w", s.paramsPath(), err)
	}
	if program == ours || sameFile(program, ours) {
		return program, nil
	}
	return "", fmt.Errorf("sdr: the program would read %s but this OBC reads and "+
		"writes %s.\n  connect.cpp hardcodes %q relative to its working directory, so "+
		"sdr.program must name a directory called sdr-ettus-b200mini; %s does not "+
		"resolve to one.\n  Parameter edits would report success and have no effect on "+
		"a capture.",
		program, ours, programConfigRelPath, s.programDir)
}

// sameFile reports whether two paths are the same existing file, following
// symlinks. False if either is missing or unreadable, which is the safe answer:
// the caller then insists on the paths matching as strings.
func sameFile(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
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

	program, err := s.programPath()
	if err != nil {
		return err
	}
	if _, err := s.programParamsPath(); err != nil {
		return err
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
	// directory. The vendored C++ resolves its configuration against CWD
	// (programConfigRelPath) and writes its capture relative to it
	// (Data/rx_data_<time>.bin), so the directory is load-bearing even though
	// the binary itself now comes from PATH. Inheriting the OBC's own directory
	// would put every capture somewhere the HTTP listing does not serve.
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

	// program and cwd are both logged, and they are different facts. The binary
	// may have come from /usr/local/bin while its configuration came from a
	// directory under /root, and an operator reading only one of them would
	// guess wrong about which params.json a capture used.
	s.log.Info("SDR acquisition started",
		"pid", pid, "program", program, "cwd", s.programDir, "log", logPath)
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

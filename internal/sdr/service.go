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

	// ErrNoDataDir is returned when the directory the program writes captures to
	// cannot be used. Distinct from ErrInvalidParams because the operator did
	// nothing wrong to the radio; the disk is wrong.
	ErrNoDataDir = errors.New("sdr: capture directory is not usable")

	// ErrNoSpace is returned when a destination has less free space than the
	// capture the configured parameters will produce.
	ErrNoSpace = errors.New("sdr: not enough free space for the capture")
)

// Service manages the acquisition program.
type Service struct {
	programDir string
	logDir     string
	// dataDir is where the program writes its capture, from the program's own
	// SSD_PATH. Checked before every acquisition. See programCaptureRelPath.
	dataDir string
	log     *slog.Logger

	// runner is injected so the whole service is testable without a USB device.
	runner func(ctx context.Context, dir, name string, args []string) (stdout string, err error)
	// start is injected for the same reason, and it also has to report when the
	// child is reaped: see exitInfo and watchExit for why the PID alone is not
	// enough to know whether an acquisition is still alive.
	start func(ctx context.Context, dir, name string, args []string, out *os.File) (pid int, exited <-chan exitInfo, err error)

	mu        sync.Mutex
	pid       int
	running   bool
	stopping  bool
	startedAt time.Time
	lastLog   string
	lastErr   string
	lastOut   string
	state     domain.SubsystemState
}

var _ domain.Sdr = (*Service)(nil)

// NewService returns a service driving the program at programDir, writing logs
// under logDir (a subdirectory of the data directory) and captures to dataDir
// (the program's own SSD_PATH).
func NewService(programDir, logDir, dataDir string, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		programDir: programDir,
		logDir:     logDir,
		dataDir:    dataDir,
		log:        log,
		runner:     runCommand,
		start:      startDetached,
		state:      domain.SubsystemDisconnected,
	}
}

// programCaptureRelPath is where the program writes a second copy of the
// capture, relative to its working directory.
//
// Taken verbatim from connect.cpp, which writes the SSD copy first and then this
// one unconditionally:
//
//	std::string filename = "./../sdr-ettus-b200mini/Data/raw_data/rx_data_" + ...
//
// The SSD copy is wrapped in a try/catch that logs and continues; this one is
// not, and write_buffer_to_disk throws when the directory is absent. The
// exception escapes the RX thread and calls std::terminate, so a missing
// Data/raw_data/ aborts the process *after* a successful session -- which
// watchExit reports as a failed acquisition. Both destinations are therefore
// checked before the child starts, and the existence of this one is what makes
// "keep a second copy on the SD card" an option rather than a crash.
const programCaptureRelPath = "Data/raw_data"

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
// actually uses. Both copies of that fact live in C++ and here; see
// programCaptureRelPath for the sibling arrangement and for what it costs.
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
// raw is the file as it stands, including every key this package does not model,
// and it is that map -- not the typed struct -- which gets written back. An
// unmodelled key therefore survives a partial update rather than being dropped for
// load_config()'s j.at() to throw on at the next ./connect.
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

// checkSweepWindow enforces the constraint that no per-key range can express.
//
// config.hpp throws "T_MIN_US must be less than T_MAX_US" from inside main(),
// before any radio is initialised. Writing a file that trips it is the worst
// outcome available: the SDR is not merely wrong, it will not start, and the
// only recovery is editing params.json by hand on the aircraft.
//
// The check runs on the merged result rather than on the incoming patch,
// because the usual edit moves one edge and reads the other from the file --
// setting T_MAX_US to 50 with a shipped T_MIN_US of 0 is a valid request, and
// setting it to 0 is not. Checking the patch alone would catch neither reliably
// and would miss the second entirely.
func checkSweepWindow(p domain.SdrParams) error {
	if p.SweepMinUs < p.SweepMaxUs {
		return nil
	}
	return fmt.Errorf("%w: T_MIN_US (%g us) must be less than T_MAX_US (%g us); "+
		"connect.cpp throws on this at startup, before any radio is initialised",
		ErrInvalidParams, p.SweepMinUs, p.SweepMaxUs)
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
		{"T_MIN_US", patch.SweepMinUs, func(f float64) { current.SweepMinUs = f }},
		{"T_MAX_US", patch.SweepMaxUs, func(f float64) { current.SweepMaxUs = f }},
		{"START_OFFSET_S", patch.StartOffsetS, func(f float64) { current.StartOffsetS = f }},
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

	// Antenna ports are strings, so ValidateValue has nothing to say about them:
	// it takes a float64 and the legal set belongs to UHD, not to this package.
	// They are carried through as written.
	if patch.TxAntenna != nil {
		current.TxAntenna = *patch.TxAntenna
	}
	if patch.RxAntenna != nil {
		current.RxAntenna = *patch.RxAntenna
	}

	// The pair check has to run on the RESULT, not on the patch, because only one
	// half is usually being changed and the other half comes from the file.
	if err := checkSweepWindow(current); err != nil {
		return err
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
	raw["T_MIN_US"] = current.SweepMinUs
	raw["T_MAX_US"] = current.SweepMaxUs
	raw["START_OFFSET_S"] = current.StartOffsetS
	raw["TX_ANTENNA"] = current.TxAntenna
	raw["RX_ANTENNA"] = current.RxAntenna

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

	store := storage.New(filepath.Dir(s.paramsPath()), "")
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
	// The refusal tests the flag, not the PID. Both used to be tested, and the
	// PID half was what made this permanent: signal 0 succeeds against an
	// unreaped child, so a crashed acquisition held the device for the rest of
	// the OBC's life. watchExit owns the flag and clears it when the child is
	// reaped, so this is now released by the child's death and nothing else.
	s.mu.Lock()
	if s.running {
		held, since := s.pid, s.startedAt
		s.mu.Unlock()
		return fmt.Errorf("%w: pid %d since %s", ErrAlreadyRunning, held,
			since.Format(time.RFC3339))
	}
	s.mu.Unlock()

	program, err := s.programPath()
	if err != nil {
		return err
	}
	if _, err := s.programParamsPath(); err != nil {
		return err
	}

	// Both places the capture goes are checked before the child starts, not
	// after it exits. Every failure here is one the program handles silently or
	// fatally: an unwritable SSD directory is swallowed by a try/catch and the
	// session is reported as a success, and a missing Data/raw_data/ throws out
	// of the RX thread and aborts a session that had already collected its data.
	// Neither is worth discovering at the end of a run.
	if err := s.checkCaptureDestinations(ctx); err != nil {
		return err
	}

	logDir, err := storage.New(s.logDir, "").Sub("sdr")
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
	// directory. The program's C++ resolves its configuration against CWD
	// (programConfigRelPath) and its second capture copy against CWD too
	// (programCaptureRelPath), so the directory is load-bearing even though the
	// binary itself comes from PATH. Inheriting the OBC's own directory would
	// put that copy somewhere the HTTP listing does not serve.
	//
	// The primary copy does NOT depend on this: SSD_PATH is absolute, and it is
	// the one that reaches the disk the operator intends. That is why the
	// working directory matters for two of the program's three paths rather than
	// all of them, which was not true when this comment was written.
	pid, exited, err := s.start(ctx, s.programDir, program, nil, f)
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("sdr: start %s: %w", program, err)
	}
	_ = f.Close() // the child holds its own descriptor

	s.mu.Lock()
	s.pid = pid
	s.running = true
	s.stopping = false
	s.startedAt = time.Now()
	s.lastLog = logPath
	s.lastErr = ""
	s.lastOut = ""
	s.state = domain.SubsystemBusy
	s.mu.Unlock()

	// One watcher per acquisition, and it is the only thing that turns the child
	// dying into a state change. Started after the state above is published, so
	// the goroutine cannot observe the new PID as unset.
	go s.watchExit(pid, exited)

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
// A plain read of the flag watchExit maintains, and deliberately NOT a
// signal-0 probe. The probe used to be the only thing that could notice a dead
// child, which meant the answer depended on the child having been reaped, and
// nothing reaped it -- see startDetached. Making the watcher the single writer
// removes the dependency entirely: the flag is true exactly while the child is
// alive, whether or not anyone has got round to collecting it.
func (s *Service) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
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

// LastError is the reason the last acquisition is not running, or "" if none.
//
// Cleared at the start of every Connect and written only by watchExit, so it
// always describes the run the PID names rather than some older one.
func (s *Service) LastError() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
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
	// Marked before the signal so watchExit can tell a stop we asked for from a
	// death nobody caused. Without it, every deliberate stop reports
	// "killed by terminated" as a failure.
	if s.running {
		s.stopping = true
	}
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
// waiting, plus a channel that receives once the process has been reaped.
//
// The child is put in its own process group so a Ctrl-C on the OBC's console
// does not take the acquisition down mid-capture, and its stdio is redirected to
// the log file rather than inherited.
//
// # Why this reaps, which the first version did not
//
// The first version called Start and returned the PID, and nothing ever called
// Wait. On Linux that leaves the child a ZOMBIE the moment it exits, and
// processAlive cannot tell a zombie from a running acquisition: signal 0
// succeeds against a zombie because the PID is still in the process table, so
// the existence check that is supposed to release Connect never released it.
//
// The observed failure was that one crashed connect -- a UHD error, or a
// session that ended -- made every subsequent sdr-connect refuse with "an
// acquisition is already running: pid N since <the original start time>", for
// the remaining life of the OBC process. The refusal names the right PID and
// the right time and both of them were true, which is what made it read as a
// real acquisition rather than a bug.
//
// Wait() in a goroutine is the whole fix: it reaps the child at the instant it
// exits, so the PID disappears from the table and processAlive is honest again.
// The channel is buffered so the goroutine can finish and release its resources
// even if nobody is reading by then, which is the case whenever the OBC is
// shutting down.
func startDetached(ctx context.Context, dir, name string, args []string, out *os.File) (int, <-chan exitInfo, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return 0, nil, err
	}

	pid := cmd.Process.Pid
	exited := make(chan exitInfo, 1)
	go func() {
		exited <- exitInfo{pid: pid, err: cmd.Wait()}
	}()
	return pid, exited, nil
}

// exitInfo is the result of reaping one acquisition.
//
// Err is nil for a clean exit, which is the NORMAL case and not a failure:
// connect computes total_pulses as session_duration * PRF and returns when the
// session is over. Reading a non-nil Err as "the run went wrong" and a nil Err
// as "it never started" are both wrong, so both are distinguished here rather
// than inferred from a PID.
type exitInfo struct {
	pid int
	err error
}

// watchExit is the single writer of the death transition.
//
// Everything about a dead acquisition flows from here: the running flag, the
// state, and the last error. That is deliberate. The previous design inferred
// all of it from a signal-0 probe inside Running(), which is why a dead child
// stayed BUSY forever -- the probe had a zombie to look at.
//
// The pid guard is load-bearing. Two acquisitions cannot overlap, but a reap
// that lands after the operator has already started a new one must not
// overwrite the new one's state with the old one's exit; without it, a connect
// refused for one slow reap would show the previous run's error and report the
// new one as not running.
func (s *Service) watchExit(pid int, exited <-chan exitInfo) {
	info := <-exited

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.pid != pid {
		return // a newer acquisition owns the state now
	}

	s.running = false
	switch {
	case s.stopping:
		// We asked for this. A deliberate Stop is not a failure and must not
		// leave the operator looking at an error they caused on purpose.
		s.lastErr = ""
		s.state = domain.SubsystemReady
		s.log.Info("SDR acquisition stopped", "pid", pid, "log", s.lastLog)
	case info.err == nil:
		// A session that ran to completion. Ready, and no error: the operator
		// asked for session_duration seconds and got them. connect computes
		// total_pulses as session_duration * PRF and returns when it is done,
		// so this is the ordinary end of a capture rather than an event.
		s.lastErr = ""
		s.state = domain.SubsystemReady
		s.log.Info("SDR acquisition finished", "pid", pid,
			"ran_for", time.Since(s.startedAt).Round(time.Millisecond).String(),
			"log", s.lastLog)
	default:
		s.lastErr = fmt.Sprintf("connect (pid %d) exited: %s; see %s",
			pid, exitReason(info.err), s.lastLog)
		s.state = domain.SubsystemError
		s.log.Error("SDR acquisition failed", "pid", pid, "err", info.err, "log", s.lastLog)
	}
}

// exitReason renders a Wait error as one clause.
//
// exec.ExitError's own string is "exit status 1" or "signal: killed", which
// reads as a fragment when it lands mid-sentence in the last_error field. A
// non-zero exit code and a signal are also different facts to an operator: the
// first is the program giving up, the second is something killing it.
func exitReason(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if status, ok := ee.Sys().(syscall.WaitStatus); ok {
			if status.Signaled() {
				return fmt.Sprintf("killed by %s", status.Signal())
			}
			return fmt.Sprintf("exit status %d", status.ExitStatus())
		}
		return ee.Error()
	}
	return err.Error()
}

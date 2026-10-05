// Package main is the Wails console. See cmd/gs/main.go for what it is; this
// file is what it does.
//
// The App owns one Stream and one Queue and nothing else. Every bound method
// below is a thin wrapper over internal/client (sockets, validation) and
// internal/gsview (rendering): there is no wire format, no socket and no
// protobuf import in this file, and the layering test enforces all three
// absences. A bound method that grew logic would be presentation swallowing
// the client, so the rule is that a method either delegates or does not exist.
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/rocsar/obc/internal/client"
	"github.com/rocsar/obc/internal/gsview"
	"github.com/rocsar/obc/internal/transport"
)

// Events emitted to the frontend. The names are a contract with
// frontend/src/main.ts; renaming one breaks the window silently, because a
// subscription to a name nobody emits is just a quiet panel.
const (
	eventTelemetry = "telemetry:frame"
	eventLink      = "link:state"
	eventCommand   = "command:result"
	eventProgress  = "download:progress"
	eventDone      = "download:done"
	eventLog       = "log"
)

// App is the bound application.
type App struct {
	ctx     context.Context
	initial client.Config

	mu     sync.Mutex
	cli    *client.Client
	stream *client.Stream
	queue  *client.Queue
	done   chan struct{}
	wg     sync.WaitGroup
	latest *gsview.View

	dlMu sync.Mutex
	dl   *activeDownload

	// emitFn is the seam that makes the App testable without a window.
	// runtime.EventsEmit kills the process when called on a bare context
	// (log.Fatalf, unrecoverable), so a headless test cannot exercise any path
	// that reaches it. It defaults to the Wails runtime and is overridden only
	// in tests -- same pattern as the newStream/newQueue seams in
	// internal/client.
	emitFn func(event string, data ...interface{})
}

// activeDownload is the running transfer, if any. A pointer, not a cancel func,
// because funcs do not compare: completion clears the slot only when it still
// points at itself, so a finished download cannot cancel its successor.
type activeDownload struct {
	cancel context.CancelFunc
}

// NewApp returns an unconnected console. Connection happens at startup so the
// window can render a connection panel before anything is attempted.
func NewApp(cfg client.Config) *App {
	return &App{initial: cfg}
}

// startup connects with the startup endpoints. Wails calls it before the
// frontend loads, so bound methods never run before ctx exists.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.logf("gs %s starting", version)
	if err := a.Connect(a.initial); err != nil {
		a.logf("connect: %v", err)
	}
}

// Version reports the build version stamped at compile time ("dev" if the
// build did not stamp one). The header shows it so provenance questions end
// at a glance instead of in a guessing thread.
func (a *App) Version() string { return version }

// shutdown releases everything. Idempotent: Disconnect guards on connection
// state, and the client's own Close methods are safe to call twice.
func (a *App) shutdown(_ context.Context) {
	_ = a.Disconnect()
}

// ---------------------------------------------------------------------------
// Connection
// ---------------------------------------------------------------------------

// Connect replaces the current connection, if any, with one to cfg. It returns
// once the sockets exist, not once the link is proven: dialling proves nothing
// (a refused dial fails fast, a blackholed one waits out its bound), so link
// health arrives afterwards through link:state events rather than through this
// return value.
func (a *App) Connect(cfg client.Config) error {
	a.teardown()

	a.mu.Lock()
	cli := client.New(cfg)
	stream := cli.NewStream(a.ctx)
	queue := cli.NewQueue()
	done := make(chan struct{})
	a.cli, a.stream, a.queue, a.done = cli, stream, queue, done
	a.wg.Add(1)
	go a.serve(stream, queue, done)
	a.mu.Unlock()

	a.logf("connecting: control=%s telemetry=%s http=%s", cfg.Control, cfg.Telemetry, cfg.HTTP)
	a.emit(eventLink, a.LinkState())
	return nil
}

// Disconnect drops the connection and stops delivery. Idempotent: calling it
// twice is not an error, because shutdown paths should never have to ask
// whether they are first.
func (a *App) Disconnect() error {
	a.teardown()
	a.emit(eventLink, a.LinkState())
	return nil
}

// teardown forgets the connection and releases its sockets, waiting for the
// fan-out to exit. At most one stale frame event can escape after it: serve
// checks done before delivering, but a frame already inside onFrame finishes
// first. One stale panel update on disconnect beats the alternative, which is
// holding mu across the wait while onFrame needs it -- a deadlock wearing a
// teardown path.
func (a *App) teardown() {
	a.mu.Lock()
	done, stream, queue := a.done, a.stream, a.queue
	a.stream, a.queue, a.done, a.latest = nil, nil, nil, nil
	a.mu.Unlock()

	if done == nil {
		return
	}
	close(done)
	// Closed before waiting, so a Recv blocked inside the reader fails at
	// once instead of running to its socket timeout.
	if stream != nil {
		stream.Close()
	}
	if queue != nil {
		queue.Close()
	}
	a.wg.Wait()

	a.dlMu.Lock()
	a.dl = nil
	a.dlMu.Unlock()
}

// serve delivers frames until done closes. One goroutine per connection; it
// touches only its arguments and the latest slot, so Disconnect can tear the
// connection down without synchronising against it beyond the wait.
func (a *App) serve(stream *client.Stream, queue *client.Queue, done <-chan struct{}) {
	defer a.wg.Done()
	for {
		select {
		case <-done:
			return
		case fr, ok := <-stream.Frames():
			if !ok {
				return
			}
			a.onFrame(stream, queue, fr)
		}
	}
}

func (a *App) onFrame(stream *client.Stream, queue *client.Queue, fr client.Frame) {
	view := gsview.Build(transport.DecodeTelemetry(fr.Telemetry))

	a.mu.Lock()
	a.latest = &view
	link := a.linkLocked(stream, queue)
	a.mu.Unlock()

	a.emit(eventTelemetry, FrameEvent{View: view, Gaps: fr.Gaps, Restart: fr.Restart, Link: link})
}

// FrameEvent is one delivered instant: the rendered view, what the stream
// noticed about it, and the link health at that moment.
type FrameEvent struct {
	View    gsview.View `json:"view"`
	Gaps    uint64      `json:"gaps"`
	Restart bool        `json:"restart"`
	Link    LinkState   `json:"link"`
}

// LinkState is client.LinkState with frontend-hostile units converted:
// time.Time stays RFC3339, but a time.Duration would cross as integer
// nanoseconds, so age crosses as seconds-float instead. The conversion lives
// here, once, rather than in every panel.
type LinkState struct {
	ControlConnected   bool    `json:"control_connected"`
	TelemetryConnected bool    `json:"telemetry_connected"`
	LastFrameAgeS      float64 `json:"last_frame_age_s"`
	FramesReceived     uint64  `json:"frames_received"`
	SequenceGaps       uint64  `json:"sequence_gaps"`
	FramesDiscarded    uint64  `json:"frames_discarded"`
	CommandsInFlight   int     `json:"commands_in_flight"`
	LastError          string  `json:"last_error"`
}

// linkLocked merges the two halves. Call with mu held; both State calls are
// non-blocking by construction (counters only, never sockets).
func (a *App) linkLocked(stream *client.Stream, queue *client.Queue) LinkState {
	var ts, qs client.LinkState
	if stream != nil {
		ts = stream.State()
	}
	if queue != nil {
		qs = queue.State()
	}
	merged := client.Combine(ts, qs)
	return LinkState{
		ControlConnected:   merged.ControlConnected,
		TelemetryConnected: merged.TelemetryConnected,
		LastFrameAgeS:      merged.LastFrameAge.Seconds(),
		FramesReceived:     merged.FramesReceived,
		SequenceGaps:       merged.SequenceGaps,
		FramesDiscarded:    merged.FramesDiscarded,
		CommandsInFlight:   merged.CommandsInFlight,
		LastError:          merged.LastError,
	}
}

// LinkState reports the current link health. Poll it after commands and on a
// timer; frames also carry a copy, but a dead link emits no frames, which is
// exactly when this method matters most.
func (a *App) LinkState() LinkState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.linkLocked(a.stream, a.queue)
}

// Snapshot returns the latest rendered frame, or nil before the first one.
// First paint calls this; everything after arrives as telemetry:frame events.
func (a *App) Snapshot() *gsview.View {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.latest
}

// ---------------------------------------------------------------------------
// Commands
// ---------------------------------------------------------------------------

// CommandResult is one submitted batch: the last response, or the first
// failure. It mirrors the reply rather than interpreting it: success is the
// OBC's word, not ours.
type CommandResult struct {
	RequestID string        `json:"request_id"`
	Success   bool          `json:"success"`
	Error     string        `json:"error"`
	Message   string        `json:"message"`
	Artefact  *ArtefactMeta `json:"artefact"`
}

// ArtefactMeta is the take_photo handover: a name, never bytes. Bytes travel
// over HTTP through DownloadArtefact.
type ArtefactMeta struct {
	Name      string `json:"name"`
	SizeBytes uint64 `json:"size_bytes"`
	Kind      string `json:"kind"`
}

// runCommand validates, submits and reports. Every motion and non-motion
// command below funnels through here, so argument validation lives in exactly
// one place (client.BuildRequests, shared with gs_cli) and result shaping in
// exactly one other (here).
func (a *App) runCommand(name string, args ...string) CommandResult {
	reqs, err := client.BuildRequests(name, args)
	if err != nil {
		// Validation failed before anything was sent: no request id exists to
		// echo, and success is unambiguously false. Emitted like any other
		// result -- a refused button press with no record trains the operator
		// to press it again.
		out := CommandResult{Error: "ERROR_INVALID_PARAMETER", Message: err.Error()}
		a.emit(eventCommand, out)
		return out
	}
	return a.submitAll(name, reqs)
}

// submitAll sends one batch sequentially and emits a command:result event as
// well as returning it: the caller gets its answer from the promise, and the
// log panel gets the same record without subscribing to every button.
func (a *App) submitAll(name string, reqs []*client.Request) CommandResult {
	q := a.getQueue()
	if q == nil {
		out := CommandResult{Error: "ERROR_NOT_CONNECTED", Message: client.ErrNotConnected.Error()}
		a.emit(eventCommand, out)
		return out
	}

	var out CommandResult
	for i, req := range reqs {
		resp, err := q.Submit(a.ctx, req)
		if err != nil {
			out = CommandResult{Error: errorName(err), Message: err.Error()}
			if i > 0 {
				// A multi-axis batch that got partway must say which axis it
				// reached, or the operator retries and cannot tell what already
				// moved. Same rule as gs_cli, same words where they fit.
				out.Message = fmt.Sprintf("%s (axis %d of %d had already been sent)", out.Message, i, len(reqs))
			}
			break
		}
		out = CommandResult{
			RequestID: resp.GetRequestId(),
			Success:   resp.GetSuccess(),
			Error:     resp.GetError().String(),
			Message:   resp.GetMessage(),
		}
		if n := resp.GetArtefactName(); n != "" {
			out.Artefact = &ArtefactMeta{Name: n, SizeBytes: resp.GetArtefactSizeBytes(), Kind: resp.GetArtefactKind()}
		}
		if !resp.GetSuccess() {
			break
		}
	}
	a.emit(eventCommand, out)
	a.emitLink()
	return out
}

// errorName maps a submit error onto the wire enum's spelling for display. A
// sentinel (link down, queue full) is not a wire error, so it renders as the
// closest wire meaning rather than inventing a new one.
func errorName(err error) string {
	switch {
	case errors.Is(err, client.ErrNotConnected):
		return "ERROR_NOT_CONNECTED"
	case errors.Is(err, client.ErrQueueFull):
		return "ERROR_BUSY"
	default:
		return "ERROR_HARDWARE_FAULT"
	}
}

// getQueue returns the live queue, or nil when disconnected. The pointer is
// copied under lock and used after it: Submit is safe for concurrent use by
// construction (one in flight enforced inside), so holding mu across a 20 s
// exchange -- which would freeze Snapshot and LinkState behind it -- is never
// necessary.
func (a *App) getQueue() *client.Queue {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.queue
}

// QueryStatus asks the OBC for the status line it otherwise volunteers in
// telemetry. Mostly a connectivity check with a human-readable answer.
func (a *App) QueryStatus() CommandResult { return a.runCommand("query") }

// TakePhoto captures one photograph. The result carries the artefact's name
// and size; the bytes arrive through DownloadArtefact, never through here.
func (a *App) TakePhoto() CommandResult { return a.runCommand("photo") }

// SelectReceiver trusts one GNSS receiver by 1-based id.
func (a *App) SelectReceiver(receiverID uint32) CommandResult {
	return a.runCommand("gnss", strconv.FormatUint(uint64(receiverID), 10))
}

// RotateReceiver trusts the next receiver instead of naming one.
func (a *App) RotateReceiver() CommandResult { return a.runCommand("gnss-rotate") }

// SetHeading points both antenna axes at an absolute bearing in degrees.
func (a *App) SetHeading(deg float64) CommandResult {
	return a.runCommand("heading", strconv.FormatFloat(deg, 'f', -1, 64))
}

// Jog moves one axis to an absolute tick, 0..4095, in manual mode. Out of
// range is refused client-side by the shared builder, before anything travels.
func (a *App) Jog(servoID uint32, tick uint32) CommandResult {
	return a.runCommand("jog",
		strconv.FormatUint(uint64(servoID), 10),
		strconv.FormatUint(uint64(tick), 10))
}

// ZeroServo centres one axis. ZeroAll centres both, as two commands -- one per
// axis, because the PicoCommand oneof carries a single servo_id.
func (a *App) ZeroServo(servoID uint32) CommandResult {
	return a.runCommand("zero", strconv.FormatUint(uint64(servoID), 10))
}

// ZeroAll centres both axes.
func (a *App) ZeroAll() CommandResult { return a.runCommand("zero") }

// MountOffset sets one axis's mount offset in degrees.
func (a *App) MountOffset(servoID uint32, offsetDeg float64) CommandResult {
	return a.runCommand("mount",
		strconv.FormatUint(uint64(servoID), 10),
		strconv.FormatFloat(offsetDeg, 'f', -1, 64))
}

// SetDirection sets one axis's direction multiplier, +1 or -1 only. Anything
// else is refused here, before the shared builder would refuse it anyway --
// the point is that no float that is not exactly ±1 ever becomes a string that
// parses.
func (a *App) SetDirection(servoID uint32, multiplier float64) CommandResult {
	var m string
	switch multiplier {
	case 1:
		m = "+1"
	case -1:
		m = "-1"
	default:
		return CommandResult{Error: "ERROR_INVALID_PARAMETER",
			Message: fmt.Sprintf("direction is %v, want +1 or -1", multiplier)}
	}
	return a.runCommand("dir", strconv.FormatUint(uint64(servoID), 10), m)
}

// SetHeater switches heater 1 or 2 on or off.
func (a *App) SetHeater(heaterID uint32, on bool) CommandResult {
	state := "off"
	if on {
		state = "on"
	}
	return a.runCommand("heater", strconv.FormatUint(uint64(heaterID), 10), state)
}

// StopServo stops one axis. StopAll stops every axis the board reports,
// resolved from live telemetry rather than assumed -- a previous GUI sent stop
// for ids it assumed, and on a differently configured bench that stops nothing
// at all.
func (a *App) StopServo(servoID uint32) CommandResult {
	return a.runCommand("stop", strconv.FormatUint(uint64(servoID), 10))
}

// StopAll stops every axis in the latest frame.
func (a *App) StopAll() CommandResult {
	a.mu.Lock()
	latest := a.latest
	a.mu.Unlock()

	if latest == nil || latest.Pico == nil {
		return CommandResult{Error: "ERROR_NOT_CONNECTED",
			Message: "no flight-controller telemetry yet; nothing is known to stop"}
	}
	// One stop per reported axis, sequentially through the queue's single
	// in-flight slot.
	var out CommandResult
	for _, axis := range latest.Pico.Antennas {
		out = a.runCommand("stop", strconv.FormatUint(uint64(axis.ServoID), 10))
		if !out.Success {
			return out
		}
	}
	return out
}

// PicoStatusRequest asks the flight controller to re-announce itself. Answered
// by its own telemetry; nothing to wait for beyond the link being open.
func (a *App) PicoStatusRequest() CommandResult { return a.runCommand("pico-status") }

// SdrProbe returns uhd_usrp_probe output verbatim, truncated by the dispatcher
// to what fits in a reply.
func (a *App) SdrProbe() CommandResult { return a.runCommand("sdr-probe") }

// SdrGetParams reads parameters/params.json via the OBC and returns it as JSON
// with the params.json key names. The Ground Station shows the values as
// placeholder hints while blank inputs keep meaning "leave alone".
//
// An OBC that predates the command answers ERROR_INVALID_COMMAND (unknown
// oneof payload, refused at dispatch, never a hang). That error is returned
// verbatim rather than translated: the frontend distinguishes "the OBC is too
// old for this" from every other failure and falls back to blank hints.
func (a *App) SdrGetParams() (string, error) {
	q := a.getQueue()
	if q == nil {
		return "", client.ErrNotConnected
	}
	reqs, err := client.BuildRequests("sdr-get-params", nil)
	if err != nil {
		return "", err
	}
	resp, err := q.Submit(a.ctx, reqs[0])
	if err != nil {
		return "", err
	}
	if !resp.GetSuccess() {
		return "", errors.New(resp.GetMessage())
	}
	return resp.GetMessage(), nil
}

// SetSdrParams applies a partial SDR parameter update. Nil fields are left
// alone; an all-nil patch is refused before anything travels, because a value
// the program cannot use bricks the SDR rather than failing at runtime.
func (a *App) SetSdrParams(patch client.SdrParamsPatch) CommandResult {
	req, err := client.BuildSetParamsRequest(patch)
	if err != nil {
		out := CommandResult{Error: "ERROR_INVALID_PARAMETER", Message: err.Error()}
		a.emit(eventCommand, out)
		return out
	}
	return a.submitAll("sdr-set-params", []*client.Request{req})
}

// SdrConnect starts the acquisition program. Refused while one is running,
// naming the PID that holds it.
func (a *App) SdrConnect() CommandResult { return a.runCommand("sdr-connect") }

// SdrResetUSB power-cycles the SDR's USB port.
func (a *App) SdrResetUSB() CommandResult { return a.runCommand("sdr-reset-usb") }

// SetLinkLimit sets the link rate limit in kbit/s. Refused below the priority
// floor, which would leave telemetry with no guaranteed bandwidth at all.
func (a *App) SetLinkLimit(rateKbps uint32) CommandResult {
	return a.runCommand("link", strconv.FormatUint(uint64(rateKbps), 10))
}

// ---------------------------------------------------------------------------
// Artefacts
// ---------------------------------------------------------------------------

// ListArtefacts lists one directory on the OBC. Empty path is the root.
func (a *App) ListArtefacts(path string) ([]client.Entry, error) {
	cli := a.getClient()
	if cli == nil {
		return nil, client.ErrNotConnected
	}
	listing, err := cli.List(a.ctx, path)
	if err != nil {
		return nil, err
	}
	return listing.Files, nil
}

// DownloadArtefact fetches one artefact over HTTP with resume, reporting
// through download:progress events and finishing with download:done. It
// returns once the transfer has STARTED, not once it finished: at 8 KiB/s a
// capture is an hour, and a bound method that blocks for an hour is a window
// that never answers. One transfer at a time; a second call while one runs is
// refused rather than queued.
//
// The destination is an in-window input, not a native file dialog: GTK's file
// chooser aborts the whole process with SIGABRT when GSettings schemas are
// missing from the environment, which is uncatchable from Go and unfixable
// from here. A text input cannot abort anything. See GUI_ARCHITECTURE.md 9.
func (a *App) DownloadArtefact(name, destPath string) error {
	if err := client.SafeName(name); err != nil {
		return err
	}
	if destPath == "" {
		return errors.New("no destination given")
	}
	cli := a.getClient()
	if cli == nil {
		return client.ErrNotConnected
	}

	path, err := expandHome(destPath)
	if err != nil {
		return err
	}

	dlCtx, cancel := context.WithCancel(a.ctx)
	dl := &activeDownload{cancel: cancel}
	a.dlMu.Lock()
	if a.dl != nil {
		a.dlMu.Unlock()
		cancel()
		return errors.New("a download is already running; cancel it first")
	}
	a.dl = dl
	a.dlMu.Unlock()

	go a.download(cli, dlCtx, dl, name, path)
	return nil
}

// expandHome resolves a leading ~/ against the user's home directory. The
// download destination is typed, not dialogued, so ~/rocsar/<name> has to work
// as written.
func expandHome(path string) (string, error) {
	if path == "~" || len(path) > 2 && path[:2] == "~/" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot resolve ~: %w", err)
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}

// CancelDownload aborts the running transfer, if any. Idempotent: no active
// download is not an error, because a cancel button that errors when there is
// nothing to cancel trains the operator to ignore it.
func (a *App) CancelDownload() error {
	a.dlMu.Lock()
	dl := a.dl
	a.dl = nil
	a.dlMu.Unlock()
	if dl != nil {
		dl.cancel()
	}
	return nil
}

func (a *App) download(cli *client.Client, ctx context.Context, dl *activeDownload, name, path string) {
	defer func() {
		a.dlMu.Lock()
		if a.dl == dl {
			a.dl = nil
		}
		a.dlMu.Unlock()
	}()

	var last time.Time
	var lastBytes int64
	xfer, err := cli.Fetch(ctx, name, path, true, client.FetchHooks{
		Progress: func(_ string, have, total int64) {
			now := time.Now()
			rate := 0.0
			if !last.IsZero() && now.After(last) {
				rate = float64(have-lastBytes) / 1024 / now.Sub(last).Seconds()
			}
			last, lastBytes = now, have
			a.emit(eventProgress, DownloadProgress{Name: name, Bytes: have, Total: total, RateKBs: rate})
		},
		Note: func(s string) { a.logf("download %s: %s", name, s) },
	})
	// Fetch returns the transfer on success; on failure the error carries how
	// far it got. Either way the window hears about it through the event, not
	// through a promise that resolved an hour ago.
	out := DownloadDone{Name: name}
	if err != nil {
		out.Error = err.Error()
	} else {
		out.Bytes = xfer.Bytes
		out.ElapsedS = xfer.Elapsed.Seconds()
		out.RateKBs = xfer.Rate()
	}
	a.emit(eventDone, out)
}

// DownloadProgress is one throttled progress report. Total is -1 when the
// server did not say; the rate is measured between reports, not claimed from
// configuration the client cannot read.
type DownloadProgress struct {
	Name    string  `json:"name"`
	Bytes   int64   `json:"bytes"`
	Total   int64   `json:"total"`
	RateKBs float64 `json:"rate_kbs"`
}

// DownloadDone closes a transfer. Error empty means success.
type DownloadDone struct {
	Name     string  `json:"name"`
	Bytes    int64   `json:"bytes"`
	ElapsedS float64 `json:"elapsed_s"`
	RateKBs  float64 `json:"rate_kbs"`
	Error    string  `json:"error"`
}

// previewCapBytes bounds in-window previews. A 30 MB SAR bin through the JSON
// bridge would freeze the window for the duration; the cap is load-bearing,
// not aesthetic. The UI offers preview only for small camera files, and this
// refuses everything over the cap regardless of what the button said.
//
// Enforced by the transfer, not after it. This used to fetch the whole file and
// then test xfer.Bytes, which is the freeze the cap exists to prevent followed by
// an error explaining why the window is frozen.
const previewCapBytes = 5 << 20

// PreviewArtefact fetches one artefact and returns it as a data URL for
// in-window display. The bytes travel the same resume path as a download, to a
// temp file that is removed after reading: a preview is point-in-time, and a
// leftover that looks like a saved artefact is worse than no file at all.
func (a *App) PreviewArtefact(name string) (string, error) {
	if err := client.SafeName(name); err != nil {
		return "", err
	}
	cli := a.getClient()
	if cli == nil {
		return "", client.ErrNotConnected
	}

	dir, err := os.MkdirTemp("", "gs-preview-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, filepath.Base(name))

	_, err = cli.Fetch(a.ctx, name, path, true, client.FetchHooks{Limit: previewCapBytes})
	if err != nil {
		if errors.Is(err, client.ErrTooLarge) {
			return "", fmt.Errorf("%s is larger than the %d byte preview limit; fetch it instead",
				name, previewCapBytes)
		}
		return "", err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return "data:" + previewMIME(name) + ";base64," + base64.StdEncoding.EncodeToString(body), nil
}

// previewMIME names the bytes for an <img> or text pane. Extension-based, on
// purpose: content sniffing a fetched artefact would be a second opinion about
// what the file is, and the listing's kind already said.
func previewMIME(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".txt", ".log":
		return "text/plain"
	default:
		return "application/octet-stream"
	}
}

// ---------------------------------------------------------------------------

func (a *App) getClient() *client.Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cli
}

func (a *App) emitLink() {
	a.mu.Lock()
	link := a.linkLocked(a.stream, a.queue)
	a.mu.Unlock()
	a.emit(eventLink, link)
}

func (a *App) emit(event string, data ...interface{}) {
	if a.emitFn != nil {
		a.emitFn(event, data...)
		return
	}
	if a.ctx == nil {
		return
	}
	runtime.EventsEmit(a.ctx, event, data...)
}

// Endpoints returns the endpoints this console is pointed at, for the
// connection panel to display and edit. A copy: the live connection keeps its
// own.
func (a *App) Endpoints() client.Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cli == nil {
		return a.initial
	}
	return a.cli.Config()
}

func (a *App) logf(format string, args ...interface{}) {
	line := fmt.Sprintf(format, args...)
	fmt.Fprintln(os.Stderr, "gs:", line)
	a.emit(eventLog, line)
}

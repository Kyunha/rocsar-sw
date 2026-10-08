package main

// Headless tests for the binding layer: no window, no display, no browser.
//
// The App's two untestable dependencies -- the WebSocket hub (a real browser
// connection) and the save-file dialog (same) -- are injected through emitFn.
// Everything else runs for real: commands cross ZeroMQ to a transport server
// bound on loopback, telemetry arrives as framed protobuf, and artefact bytes
// come from the OBC's own file handler over a test HTTP server. A fake that
// stubbed any of those would test the stub.
//
// Naming the wire types requires importing the generated bindings, which is
// allowed here and only here: TestCmdGsProductionHasNoProtobuf forbids the
// import in every non-test file under cmd/gs.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	rocsarv1 "github.com/rocsar/obc/api/rocsar/v1"
	"github.com/rocsar/obc/internal/client"
	"github.com/rocsar/obc/internal/domain"
	"github.com/rocsar/obc/internal/qos"
	"github.com/rocsar/obc/internal/storage"
	"github.com/rocsar/obc/internal/telemetry"
	"github.com/rocsar/obc/internal/transport"
)

// freePort asks the OS for an unused localhost port. Asked, not hardcoded: a
// hardcoded port that is already in use fails the suite for a reason that has
// nothing to do with the code.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// harness is a fake OBC assembled from the real server pieces: the actual
// ZeroMQ transport with a canned handler, and the actual file handler over a
// test data directory. The frame layout, the envelope and the HTTP listing
// shape are all exercised rather than stubbed.
type harness struct {
	t      *testing.T
	ctx    context.Context
	cancel context.CancelFunc
	zmq    *transport.ZMQ
	http   *httptest.Server
	dir    string

	mu        sync.Mutex
	sequence  uint64
	selected  uint32
	received  int
	endpoints client.Config
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{ctx: ctx, cancel: cancel, selected: 1}

	control := fmt.Sprintf("tcp://127.0.0.1:%d", freePort(t))
	publish := fmt.Sprintf("tcp://127.0.0.1:%d", freePort(t))
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	h.zmq = transport.NewZMQ(log, h.answer)
	if err := h.zmq.Start(ctx, control, publish); err != nil {
		t.Fatalf("fake transport: %v", err)
	}

	h.dir = t.TempDir()
	store := storage.New(h.dir, "")
	if err := store.WriteFileAtomic("photos/fake.jpg", []byte("fake-jpeg-bytes"), 0o644); err != nil {
		t.Fatalf("seed artefact: %v", err)
	}
	h.http = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drip-feed one path so progress wiring is observable: three bytes at
		// 150 ms apart against the 100 ms floor guarantees at least two
		// progress calls. A fast localhost transfer finishes inside one Read
		// and correctly produces none, which is what the floor is for.
		if r.URL.Path == "/drip.bin" {
			w.Header().Set("Content-Length", "3")
			w.WriteHeader(http.StatusOK)
			fl, ok := w.(http.Flusher)
			if !ok {
				t.Error("test server does not flush")
				return
			}
			for i := 0; i < 3; i++ {
				_, _ = w.Write([]byte("x"))
				fl.Flush()
				time.Sleep(150 * time.Millisecond)
			}
			return
		}
		transport.NewFileHandler(store, log).ServeHTTP(w, r)
	}))

	h.endpoints = client.Config{Control: control, Telemetry: publish, HTTP: h.http.URL, Topic: "telemetry"}
	return h
}

func (h *harness) answer(req *rocsarv1.CommandRequest) *rocsarv1.CommandResponse {
	h.mu.Lock()
	h.received++
	h.mu.Unlock()

	ok := func(msg string) *rocsarv1.CommandResponse {
		return &rocsarv1.CommandResponse{
			RequestId: req.GetRequestId(), Success: true,
			Error: rocsarv1.ErrorCode_ERROR_NONE, Message: msg,
		}
	}
	switch p := req.GetPayload().(type) {
	case *rocsarv1.CommandRequest_QueryStatus:
		return ok("fake obc status")
	case *rocsarv1.CommandRequest_GnssSelect:
		h.mu.Lock()
		h.selected = p.GnssSelect.GetReceiverId()
		h.mu.Unlock()
		return ok(fmt.Sprintf("receiver %d is now trusted", p.GnssSelect.GetReceiverId()))
	case *rocsarv1.CommandRequest_TakePhoto:
		return &rocsarv1.CommandResponse{
			RequestId: req.GetRequestId(), Success: true,
			Error: rocsarv1.ErrorCode_ERROR_NONE, Message: "captured",
			ArtefactName:      strptr("photos/fake.jpg"),
			ArtefactSizeBytes: uint64ptr(16),
			ArtefactKind:      strptr("camera"),
		}
	case *rocsarv1.CommandRequest_SdrGetParams:
		return ok(`{"PRF":2750,"FS":31251000,"TX_FREQ":5800000000,"SESSION_DURATION":60}`)
	default:
		return ok(fmt.Sprintf("fake obc answered %T", p))
	}
}

func strptr(s string) *string    { return &s }
func uint64ptr(v uint64) *uint64 { return &v }

// publish emits one frame with the next sequence number.
func (h *harness) publish(t *testing.T) {
	t.Helper()
	h.mu.Lock()
	h.sequence++
	seq := h.sequence
	h.mu.Unlock()

	snap := telemetry.Snapshot{
		Sequence: seq, GeneratedAt: time.Now(),
		System: telemetry.SystemSnapshot{State: domain.SubsystemReady, Uptime: time.Duration(seq) * time.Second},
	}
	body, err := proto.Marshal(transport.EncodeTelemetry(snap))
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	h.zmq.Publish(qos.TopicTelemetry, body)
}

func (h *harness) close() {
	h.cancel()
	h.zmq.Stop()
	h.http.Close()
}

// recorder captures emitted events.
type recorder struct {
	mu     sync.Mutex
	events map[string][]interface{}
}

func newRecorder() *recorder { return &recorder{events: map[string][]interface{}{}} }

func (r *recorder) emit(event string, data ...interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events[event] = append(r.events[event], data...)
}

func (r *recorder) count(event string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events[event])
}

func (r *recorder) last(event string) interface{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	got := r.events[event]
	if len(got) == 0 {
		return nil
	}
	return got[len(got)-1]
}

// testApp builds an App wired to the harness with injected events.
func (h *harness) testApp(t *testing.T, rec *recorder) *App {
	t.Helper()
	app := NewApp(h.endpoints)
	app.emitFn = rec.emit
	app.startup(context.Background())
	t.Cleanup(func() { app.shutdown() })
	return app
}

func eventually(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for: %s", timeout, msg)
}

// The frame path end to end: publish real frames, receive a rendered snapshot
// and a telemetry:frame event carrying gaps, restart state and link health.
func TestAppDeliversTelemetryFrames(t *testing.T) {
	h := newHarness(t)
	defer h.close()
	rec := newRecorder()
	app := h.testApp(t, rec)

	for i := 0; i < 8; i++ {
		h.publish(t)
		time.Sleep(20 * time.Millisecond)
	}

	eventually(t, 15*time.Second, func() bool { return app.Snapshot() != nil },
		"a rendered snapshot")
	snap := app.Snapshot()
	if snap == nil {
		t.Fatal("Snapshot is nil after delivery")
	}
	if snap.System.State != "READY" {
		t.Errorf("system state rendered %q, want READY", snap.System.State)
	}

	eventually(t, 15*time.Second, func() bool { return rec.count(eventTelemetry) > 0 },
		"a telemetry:frame event")
	raw := rec.last(eventTelemetry)
	ev, ok := raw.(FrameEvent)
	if !ok {
		t.Fatalf("telemetry:frame payload is %T, want FrameEvent", raw)
	}
	if ev.View.Sequence == 0 {
		t.Error("frame event carries no view")
	}
	if !app.LinkState().TelemetryConnected {
		t.Errorf("link state after delivery: %+v, want telemetry connected", app.LinkState())
	}
}

// Every bound command below crosses the wire to the fake and comes back shaped
// as a CommandResult; the validation failure never leaves the process.
func TestAppBoundCommandsRoundTrip(t *testing.T) {
	h := newHarness(t)
	defer h.close()
	rec := newRecorder()
	app := h.testApp(t, rec)

	if r := app.QueryStatus(); !r.Success || r.Error != "ERROR_NONE" {
		t.Errorf("query: %+v, want success", r)
	}
	if r := app.SelectReceiver(2); !r.Success {
		t.Fatalf("gnss select: %+v", r)
	}
	h.mu.Lock()
	selected := h.selected
	h.mu.Unlock()
	if selected != 2 {
		t.Errorf("fake saw receiver %d selected, want 2", selected)
	}
	if r := app.RotateReceiver(); !r.Success {
		t.Errorf("gnss rotate: %+v", r)
	}
	if r := app.SetHeading(12.5); !r.Success {
		t.Errorf("heading: %+v", r)
	}
	if r := app.TakePhoto(); !r.Success || r.Artefact == nil || r.Artefact.Name != "photos/fake.jpg" {
		t.Errorf("photo: %+v, want artefact metadata not bytes", r)
	}

	// Params read returns the JSON the dispatcher put in message, verbatim.
	// The keys are asserted, not just parsability: the GUI maps them to form
	// placeholders, so a renamed key is a silent blank field.
	raw, err := app.SdrGetParams()
	if err != nil {
		t.Fatalf("sdr-get-params: %v", err)
	}
	var params map[string]any
	if err := json.Unmarshal([]byte(raw), &params); err != nil {
		t.Fatalf("params reply is not JSON: %v (%q)", err, raw)
	}
	if params["PRF"] != 2750.0 || params["SESSION_DURATION"] != 60.0 {
		t.Errorf("params = %v, want the harness values", params)
	}

	// A tick outside the ST3215 range is refused before anything travels: the
	// fake must see no new command for it.
	before := h.receivedCount()
	if r := app.Jog(1, 9999); r.Success || r.Error != "ERROR_INVALID_PARAMETER" {
		t.Errorf("jog 9999: %+v, want a client-side refusal", r)
	}
	if got := h.receivedCount(); got != before {
		t.Errorf("refused jog reached the fake: %d commands, was %d", got, before)
	}

	prf := 2750.0
	if r := app.SetSdrParams(client.SdrParamsPatch{PRFHz: &prf}); !r.Success {
		t.Errorf("sdr params: %+v, want success", r)
	}
	if r := app.SetSdrParams(client.SdrParamsPatch{}); r.Success || r.Error != "ERROR_INVALID_PARAMETER" {
		t.Errorf("empty params patch: %+v, want a refusal", r)
	}

	eventually(t, 10*time.Second, func() bool { return rec.count(eventCommand) >= 8 },
		"command:result events for the eight submissions")
}

// Artefacts list from the real file handler and download through the real
// fetch path with resume, progress events and a completion event.
func TestAppListsAndDownloadsArtefacts(t *testing.T) {
	h := newHarness(t)
	defer h.close()
	rec := newRecorder()
	dst := filepath.Join(t.TempDir(), "saved.jpg")
	app := h.testApp(t, rec)

	entries, err := app.ListArtefacts("")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// The seed lives one level down: the root shows the directory, and the
	// directory shows the photograph. Both levels are asserted because the
	// artefact browser drills down, and a listing that flattens would send the
	// fetch after a name the server never listed.
	if len(entries) != 1 || entries[0].Name != "photos" || !entries[0].Directory {
		t.Fatalf("root listing = %+v, want the photos directory", entries)
	}
	entries, err = app.ListArtefacts("photos")
	if err != nil {
		t.Fatalf("list photos: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "fake.jpg" {
		t.Fatalf("photos listing = %+v, want the seeded photograph", entries)
	}

	if err := app.DownloadArtefact("photos/fake.jpg", dst); err != nil {
		t.Fatalf("download start: %v", err)
	}
	eventually(t, 15*time.Second, func() bool { return rec.count(eventDone) > 0 },
		"a download:done event")
	raw := rec.last(eventDone)
	done, ok := raw.(DownloadDone)
	if !ok {
		t.Fatalf("download:done payload is %T, want DownloadDone", raw)
	}
	if done.Error != "" {
		t.Fatalf("download failed: %s", done.Error)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "fake-jpeg-bytes" {
		t.Errorf("downloaded %q, want the seeded bytes", got)
	}
	// Progress wiring needs a slow transfer: a 16-byte localhost fetch
	// finishes inside one Read, under the 100 ms floor, and correctly
	// produces no progress at all. The drip path above forces two calls.
	rec2 := newRecorder()
	app2 := NewApp(h.endpoints)
	app2.emitFn = rec2.emit
	app2.startup(context.Background())
	defer app2.shutdown()
	if err := app2.DownloadArtefact("drip.bin", filepath.Join(t.TempDir(), "drip.bin")); err != nil {
		t.Fatalf("drip download start: %v", err)
	}
	eventually(t, 15*time.Second, func() bool { return rec2.count(eventProgress) >= 2 },
		"two download:progress events on a drip-fed transfer")

	// Cancelling with nothing running is not an error.
	if err := app.CancelDownload(); err != nil {
		t.Errorf("cancel when idle: %v", err)
	}
}

// Disconnect stops delivery and clears the snapshot: the window must show
// "no data", never the last frame as if it were live.
func TestAppDisconnectStopsDelivery(t *testing.T) {
	h := newHarness(t)
	defer h.close()
	rec := newRecorder()
	app := h.testApp(t, rec)

	for i := 0; i < 8; i++ {
		h.publish(t)
		time.Sleep(20 * time.Millisecond)
	}
	eventually(t, 15*time.Second, func() bool { return app.Snapshot() != nil },
		"a snapshot before disconnect")

	if err := app.Disconnect(); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if app.Snapshot() != nil {
		t.Error("Snapshot renders a frame after disconnect; want nil (no data, not the last frame as if live)")
	}
	st := app.LinkState()
	if st.TelemetryConnected || st.ControlConnected {
		t.Errorf("link state after disconnect: %+v, want both down", st)
	}
	// Idempotent: shutdown paths never ask whether they are first.
	if err := app.Disconnect(); err != nil {
		t.Errorf("second disconnect: %v", err)
	}

	n := rec.count(eventTelemetry)
	time.Sleep(300 * time.Millisecond)
	if got := rec.count(eventTelemetry); got != n {
		t.Errorf("%d telemetry events arrived after disconnect; delivery did not stop", got-n)
	}
}

func (h *harness) receivedCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.received
}

// Preview fetches through the resume path and returns a data URL, refusing
// over the cap: a multi-megabyte SAR bin through the JSON bridge would freeze
// the window, and the UI must never be able to ask for one by accident.
func TestAppPreviewArtefact(t *testing.T) {
	h := newHarness(t)
	defer h.close()
	rec := newRecorder()
	app := h.testApp(t, rec)

	url, err := app.PreviewArtefact("photos/fake.jpg")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	const prefix = "data:image/jpeg;base64,"
	if !strings.HasPrefix(url, prefix) {
		t.Fatalf("preview = %q..., want a JPEG data URL", url[:min(32, len(url))])
	}
	body, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(url, prefix))
	if err != nil {
		t.Fatalf("preview body is not base64: %v", err)
	}
	if string(body) != "fake-jpeg-bytes" {
		t.Errorf("preview decoded to %q, want the seeded bytes", body)
	}

	// Over the cap is refused, not truncated: a partial image renders as a
	// corrupt one, which is worse than an explicit refusal.
	big := make([]byte, (5<<20)+1)
	if err := os.WriteFile(filepath.Join(h.dir, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := app.PreviewArtefact("big.bin"); err == nil {
		t.Error("6 MB preview accepted; the cap exists to keep bins off the bridge")
	}

	if _, err := app.PreviewArtefact("../escape.jpg"); err == nil {
		t.Error("traversal accepted for preview")
	}

	// No connection, no fetch attempt.
	plain := NewApp(h.endpoints)
	plain.ctx = context.Background()
	if _, err := plain.PreviewArtefact("photos/fake.jpg"); !errors.Is(err, client.ErrNotConnected) {
		t.Errorf("preview while disconnected = %v, want ErrNotConnected", err)
	}
}

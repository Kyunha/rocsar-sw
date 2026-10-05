package command

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	rocsarv1 "github.com/rocsar/obc/api/rocsar/v1"
	"github.com/rocsar/obc/internal/domain"
)

// heaterCall is one SetHeater as the fake saw it.
type heaterCall struct {
	heaterID uint32
	on       bool
}

// fakePico is a flight controller that only exists to answer heater commands.
//
// Everything else is the embedded nil interface: a dispatcher test that calls
// into an unimplemented method fails loudly instead of quietly passing, which
// is the behaviour wanted from a fake.
type fakePico struct {
	domain.Pico

	mu        sync.Mutex
	calls     []heaterCall
	connected bool
	ack       *domain.Ack
	err       error
}

func (f *fakePico) Connected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}

func (f *fakePico) SetHeater(_ context.Context, heaterID uint32, on bool) (*domain.Ack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, heaterCall{heaterID: heaterID, on: on})
	return f.ack, f.err
}

// heaterCalls returns a snapshot of what was asked for.
func (f *fakePico) heaterCalls() []heaterCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]heaterCall(nil), f.calls...)
}

// set makes the fake answer the way the next test needs.
func (f *fakePico) set(connected bool, ack *domain.Ack, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connected, f.ack, f.err = connected, ack, err
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func heaterRequest(heaterID uint32, on bool) *rocsarv1.CommandRequest {
	return &rocsarv1.CommandRequest{
		RequestId: "req-1",
		Payload: &rocsarv1.CommandRequest_Pico{
			Pico: &rocsarv1.PicoCommand{
				Payload: &rocsarv1.PicoCommand_Heater{
					Heater: &rocsarv1.HeaterCommand{HeaterId: heaterID, State: on},
				},
			},
		},
	}
}

func TestTheKeeperRefreshesOnlyWhatWasAskedFor(t *testing.T) {
	pico := &fakePico{connected: true, ack: &domain.Ack{Success: true}}
	keeper := NewHeaterKeeper(pico, discardLog(), time.Second)

	keeper.Note(1, true)
	keeper.Refresh(context.Background())

	calls := pico.heaterCalls()
	if len(calls) != 1 || calls[0] != (heaterCall{heaterID: 1, on: true}) {
		t.Fatalf("refresh with heater 1 wanted = %v, want one call for (1, on)", calls)
	}

	// The off settles it: the next refresh must be silence, not a re-assert of
	// the old intent. Silence is also what keeps a heater off when the operator
	// never asked for one.
	pico.mu.Lock()
	pico.calls = nil
	pico.mu.Unlock()
	keeper.Note(1, false)
	keeper.Refresh(context.Background())
	if calls := pico.heaterCalls(); len(calls) != 0 {
		t.Fatalf("refresh after off = %v, want no calls", calls)
	}
}

func TestTheKeeperSaysNothingWhenNobodyWantsHeat(t *testing.T) {
	pico := &fakePico{connected: true, ack: &domain.Ack{Success: true}}
	keeper := NewHeaterKeeper(pico, discardLog(), time.Second)

	keeper.Refresh(context.Background())
	keeper.Note(3, true) // a heater this build does not have
	keeper.Refresh(context.Background())

	if calls := pico.heaterCalls(); len(calls) != 0 {
		t.Fatalf("refresh with nothing wanted = %v, want no calls", calls)
	}
	if keeper.Wanted(3) || keeper.Wanted(0) {
		t.Fatal("an id outside 1..2 must be inert, not remembered")
	}
}

func TestTheKeeperKeepsTryingAfterAFailure(t *testing.T) {
	pico := &fakePico{connected: true, err: errors.New("serial write failed")}
	keeper := NewHeaterKeeper(pico, discardLog(), time.Second)
	keeper.Note(2, true)

	keeper.Refresh(context.Background())
	keeper.Refresh(context.Background())
	if len(pico.heaterCalls()) != 2 {
		t.Fatalf("a failed refresh = %d attempts, want it to keep trying", len(pico.heaterCalls()))
	}
	if !keeper.Wanted(2) {
		t.Fatal("a transport failure must not forget what the operator asked for")
	}

	// A refusal is also not a forget: the firmware's window is the backstop, and
	// dropping the intent here would mean the heater stays off for good after
	// one bad ACK.
	pico.set(true, &domain.Ack{Success: false, Error: domain.ErrInvalidHeater}, nil)
	keeper.Refresh(context.Background())
	if !keeper.Wanted(2) {
		t.Fatal("a refusal must not forget what the operator asked for")
	}
}

func TestTheKeeperStopsWithItsContext(t *testing.T) {
	pico := &fakePico{connected: true, ack: &domain.Ack{Success: true}}
	keeper := NewHeaterKeeper(pico, discardLog(), 5*time.Millisecond)
	keeper.Note(1, true)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		keeper.Run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for len(pico.heaterCalls()) < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(pico.heaterCalls()) < 2 {
		cancel()
		t.Fatalf("keeper made %d refreshes in 2s, want a ticking loop", len(pico.heaterCalls()))
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
	atStop := len(pico.heaterCalls())
	time.Sleep(30 * time.Millisecond)
	if grew := len(pico.heaterCalls()) - atStop; grew > 1 {
		t.Fatalf("keeper made %d more refreshes after cancellation, want none", grew)
	}
}

func TestAnAcknowledgedOnIsWhatTheKeeperKeepsAlive(t *testing.T) {
	pico := &fakePico{connected: true, ack: &domain.Ack{Success: true}}
	keeper := NewHeaterKeeper(pico, discardLog(), time.Second)
	d := New(Deps{Pico: pico, Heaters: keeper, Log: discardLog()})
	ctx := context.Background()

	resp := d.Handle(ctx, heaterRequest(1, true))
	if !resp.GetSuccess() {
		t.Fatalf("heater on failed: %s", resp.GetMessage())
	}
	if !keeper.Wanted(1) {
		t.Fatal("an acknowledged on must enter the keep-alive")
	}

	// Refused: the board said no, so there is nothing to keep alive. If this
	// were noted anyway, the keeper would fight the board every interval.
	pico.set(true, &domain.Ack{Success: false, Error: domain.ErrInvalidHeater}, nil)
	if resp := d.Handle(ctx, heaterRequest(2, true)); resp.GetSuccess() {
		t.Fatal("a refused on must not report success")
	}
	if keeper.Wanted(2) {
		t.Fatal("a refused on must not enter the keep-alive")
	}

	// Error: the command may not have arrived. Noting it would risk reviving a
	// heater the board never switched on, so it stays out; the safe direction
	// is a heater that ends up off.
	pico.set(true, nil, errors.New("timeout waiting for ack"))
	if resp := d.Handle(ctx, heaterRequest(2, true)); resp.GetSuccess() {
		t.Fatal("a failed on must not report success")
	}
	if keeper.Wanted(2) {
		t.Fatal("an unacknowledged on must not enter the keep-alive")
	}
}

func TestAnOffIsRecordedEvenWhenTheLinkIsDown(t *testing.T) {
	pico := &fakePico{connected: true, ack: &domain.Ack{Success: true}}
	keeper := NewHeaterKeeper(pico, discardLog(), time.Second)
	d := New(Deps{Pico: pico, Heaters: keeper, Log: discardLog()})
	ctx := context.Background()

	if resp := d.Handle(ctx, heaterRequest(1, true)); !resp.GetSuccess() {
		t.Fatalf("setup failed: %s", resp.GetMessage())
	}
	if !keeper.Wanted(1) {
		t.Fatal("setup: the on should have been noted")
	}

	// The operator asks for off exactly when the cable has come out. Nothing
	// reaches the board, but the intent must land in the keeper: the moment the
	// link returns, a keeper still holding "on" would switch the heater back on
	// against a live operator request. The firmware's window covers the gap
	// either way.
	pico.set(false, nil, nil)
	resp := d.Handle(ctx, heaterRequest(1, false))
	if resp.GetSuccess() {
		t.Fatal("an off with the link down must report failure")
	}
	if keeper.Wanted(1) {
		t.Fatal("an off must be recorded even when it could not be delivered")
	}
}

func TestAnOffWithoutAKeeperStillAnswers(t *testing.T) {
	pico := &fakePico{connected: true, ack: &domain.Ack{Success: true}}
	d := New(Deps{Pico: pico, Log: discardLog()})

	if resp := d.Handle(context.Background(), heaterRequest(1, false)); !resp.GetSuccess() {
		t.Fatalf("a dispatcher without a keeper must still dispatch: %s", resp.GetMessage())
	}
}

func TestTheDispatcherRunsItsKeeper(t *testing.T) {
	pico := &fakePico{connected: true, ack: &domain.Ack{Success: true}}
	keeper := NewHeaterKeeper(pico, discardLog(), 5*time.Millisecond)
	keeper.Note(1, true)
	d := New(Deps{Pico: pico, Heaters: keeper, Log: discardLog()})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		d.Run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for len(pico.heaterCalls()) < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if len(pico.heaterCalls()) < 2 {
		t.Fatalf("dispatcher.Run forwarded %d refreshes, want the keeper ticking", len(pico.heaterCalls()))
	}
}

// TestThePairingConstantIsTheDocumentedOne pins the number the firmware test
// reads out of this file. firmware/tests/test_firmware_heater.py asserts that
// two of these fit into HEATER_AUTO_OFF_MS; this asserts the constant still
// exists with the value the docs claim, so a rename fails here rather than in
// a Python regex.
func TestThePairingConstantIsTheDocumentedOne(t *testing.T) {
	if HeaterRefreshInterval != 10*time.Second {
		t.Fatalf("HeaterRefreshInterval = %s, want 10s (the firmware window is 20s)",
			HeaterRefreshInterval)
	}
}

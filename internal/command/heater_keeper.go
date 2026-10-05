package command

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/rocsar/obc/internal/domain"
)

// HeaterRefreshInterval is how often an acknowledged heater-on is re-asserted.
//
// The firmware turns its heaters off HEATER_AUTO_OFF_MS (20 s) after the last
// acknowledged on, whatever anyone intended; a refresh inside that window
// restarts it. Two refreshes must fit inside one window so that a single lost
// keep-alive -- one scheduling hiccup, one busy serial read -- does not flicker
// a heater that is meant to be on. firmware/tests/test_firmware_heater.py
// reads this constant and asserts the pairing against the firmware's own, so
// neither half can be changed without the other failing a test that says why.
const HeaterRefreshInterval = 10 * time.Second

// HeaterKeeper is the host half of the heater dead-man.
//
// It holds intent, not state: "the operator asked for this heater to be on,
// and nobody has asked for it to be off since." Every HeaterRefreshInterval
// that intent is re-asserted against the board, which restarts the firmware's
// window. Every way this half can fail -- crash, cut cable, reboot, a Ground
// Station that never came up -- leaves the firmware half to do the turning off,
// which is the direction that is safe to fail in.
//
// The asymmetry is deliberate and lives in what the dispatcher notes: an ON is
// recorded only once the board has acknowledged it, so an intent that never
// arrived cannot be resurrected by the keep-alive later; an OFF is recorded
// even when the round trip fails, because dropping a keep-alive can only cool.
type HeaterKeeper struct {
	pico     domain.Pico
	log      *slog.Logger
	interval time.Duration

	mu     sync.Mutex
	wanted [3]bool // indexed by heater id; [0] unused so a bad id is inert
}

// NewHeaterKeeper returns a keeper that re-asserts wanted heaters every
// interval. The interval is a parameter rather than the constant so a test can
// watch the ticker without waiting ten seconds; production passes
// HeaterRefreshInterval.
func NewHeaterKeeper(pico domain.Pico, log *slog.Logger, interval time.Duration) *HeaterKeeper {
	if log == nil {
		log = slog.Default()
	}
	if interval <= 0 {
		interval = HeaterRefreshInterval
	}
	return &HeaterKeeper{pico: pico, log: log, interval: interval}
}

// Note records the operator's latest word on one heater.
//
// Ids this build does not have are dropped rather than remembered: there is
// nothing to keep alive, and a later refresh loop over a remembered id would be
// a command the board will refuse every ten seconds.
func (k *HeaterKeeper) Note(heaterID uint32, on bool) {
	if k == nil || heaterID < 1 || heaterID >= uint32(len(k.wanted)) {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.wanted[heaterID] = on
}

// Wanted reports whether a keep-alive should be sent for one heater.
func (k *HeaterKeeper) Wanted(heaterID uint32) bool {
	if k == nil || heaterID < 1 || heaterID >= uint32(len(k.wanted)) {
		return false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.wanted[heaterID]
}

// Refresh re-asserts every heater that is still wanted, once.
//
// A heater that is not wanted is not switched off here: OFF is an operator
// command that goes through the dispatcher like any other, and this loop's
// whole job is to say nothing at all when nobody wants heat. A failed refresh
// does not clear the intent either -- the next tick tries again, and if the
// link stays down the firmware's window closes regardless of what we wanted.
func (k *HeaterKeeper) Refresh(ctx context.Context) {
	if k == nil || k.pico == nil {
		return
	}
	for heaterID := uint32(1); heaterID < uint32(len(k.wanted)); heaterID++ {
		if !k.Wanted(heaterID) {
			continue
		}
		ack, err := k.pico.SetHeater(ctx, heaterID, true)
		switch {
		case err != nil:
			k.log.Warn("heater keep-alive failed", "heater_id", heaterID, "err", err)
		case ack == nil:
			k.log.Warn("heater keep-alive got no answer", "heater_id", heaterID)
		case !ack.Success:
			k.log.Warn("heater keep-alive refused", "heater_id", heaterID, "error_code", ack.Error)
		default:
			k.log.Info("heater keep-alive", "heater_id", heaterID)
		}
	}
}

// Run refreshes on a ticker until ctx is done.
//
// It blocks. The composition root starts it in its own goroutine, and because
// the deadline is the context's, stopping the OBC stops the keep-alive with it
// -- which is safe: a stopped keep-alive is exactly the failure the firmware's
// window is there to survive.
func (k *HeaterKeeper) Run(ctx context.Context) {
	if k == nil {
		return
	}
	ticker := time.NewTicker(k.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			k.Refresh(ctx)
		}
	}
}

package telemetry

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Pipeline owns the clock.
//
// The Engine is pure and does not know what a second is. The Pipeline ticks, asks
// the Engine for a frame, and hands it to a sink. That is the whole class
// boundary: a sink is `func(Snapshot)`, so testing the pipeline needs a fake
// clock and a slice append, not a server.
type Pipeline struct {
	engine *Engine
	sink   func(Snapshot)
	log    *slog.Logger

	mu       sync.RWMutex
	interval time.Duration

	frames uint64
}

// NewPipeline returns a pipeline publishing every interval.
func NewPipeline(e *Engine, interval time.Duration, sink func(Snapshot), log *slog.Logger) *Pipeline {
	if interval <= 0 {
		interval = time.Second
	}
	if log == nil {
		log = slog.Default()
	}
	return &Pipeline{engine: e, interval: interval, sink: sink, log: log}
}

// Run ticks until the context is cancelled.
//
// The timer is stopped on the way out, and the final frame is not sent: a
// shutdown-time frame reports a system that is on its way down, and the client
// would have to distinguish it from a live one.
//
// A self-resetting timer is used rather than a ticker so that a change to the
// interval takes effect on the next tick without a separate signal channel.
func (p *Pipeline) Run(ctx context.Context) {
	t := time.NewTimer(p.getInterval())
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			snap := p.engine.Build()
			p.frames++
			p.sink(snap)
			t.Reset(p.getInterval())
		}
	}
}

// Frames reports how many frames were published.
func (p *Pipeline) Frames() uint64 { return p.frames }

// Interval is the publish period.
func (p *Pipeline) Interval() time.Duration {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.interval
}

// SetInterval changes the publish period at runtime.
//
// The new interval takes effect on the next tick. A non-positive duration is
// ignored, so a bad command cannot stop the pipeline.
func (p *Pipeline) SetInterval(d time.Duration) {
	if d <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.interval = d
}

func (p *Pipeline) getInterval() time.Duration {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.interval
}

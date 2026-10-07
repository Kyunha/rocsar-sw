package gnss

import (
	"math"
	"testing"
	"time"
)

// The estimator's whole reason for existing is that differencing does not work
// at GNSS altitude noise levels, so these start from a noisy series rather than a
// clean one. A test with clean input would pass for both implementations and
// prove nothing.
func TestItRecoversABalloonAscentFromNoisyAltitude(t *testing.T) {
	e := NewClimbEstimator()
	base := time.Date(2026, 10, 3, 11, 0, 0, 0, time.UTC)

	// 5 m/s, which is a real stratospheric ascent rate, with +/-2 m of altitude
	// noise -- about what a good antenna delivers.
	noise := []float64{1.2, -0.8, 1.9, -1.5, 0.4, -1.1, 1.7, -2.0, 0.6, -0.3,
		1.4, -1.0, 0.9, -1.6, 1.1, -0.5, 1.8, -1.3, 0.2, -0.9, 1.0, -0.7,
		0.5, -1.2, 1.6, -0.4}
	for i, n := range noise {
		e.Observe(base.Add(time.Duration(i)*time.Second), float64(i*5)+n)
	}

	last := base.Add(time.Duration(len(noise)-1) * time.Second)
	rate := e.Rate(last, 2*time.Second)
	if rate == nil {
		t.Fatal("no rate after 20 s of a clean ascent")
	}
	if math.Abs(*rate-5.0) > 0.15 {
		t.Errorf("rate = %+0.3f m/s, want ~+5.0", *rate)
	}
}

// The case that justifies a windowed fit over a difference. Differencing two
// samples a second apart gives about 3 m of noise on a 5 m/s signal, so the
// one-second answer is noise and this is the assertion that it is not.
func TestItResolvesARateThatDifferencingCannot(t *testing.T) {
	e := NewClimbEstimator()
	base := time.Date(2026, 10, 3, 11, 0, 0, 0, time.UTC)

	const n = 60
	for i := 0; i < n; i++ {
		// A deterministic pseudo-noise, so the test cannot flake.
		alt := float64(i)*5 + math.Sin(float64(i)*2.399)*2.0
		e.Observe(base.Add(time.Duration(i)*time.Second), alt)
	}

	rate := e.Rate(base.Add((n-1)*time.Second), 2*time.Second)
	if rate == nil {
		t.Fatal("no rate")
	}
	// A one-second difference on this series would be 5 +/- ~5 m/s.
	if math.Abs(*rate-5.0) > 0.05 {
		t.Errorf("rate = %+0.4f m/s, want ~+5.0 within 0.05", *rate)
	}
}

func TestAFloatingBalloonReportsAboutZeroAndNotNil(t *testing.T) {
	e := NewClimbEstimator()
	base := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

	for i := 0; i < 40; i++ {
		e.Observe(base.Add(time.Duration(i)*time.Second), 30000+math.Sin(float64(i))*1.5)
	}

	rate := e.Rate(base.Add(39*time.Second), 2*time.Second)
	if rate == nil {
		t.Fatal("a balloon at float must produce a number, not nil: nil means 'not derivable'")
	}
	if math.Abs(*rate) > 0.2 {
		t.Errorf("a level balloon reported %+0.3f m/s", *rate)
	}
}

// A descent has to come out negative. A sign error here would report a balloon
// falling as one rising, and nothing else on the console would catch it.
func TestADescentIsNegative(t *testing.T) {
	e := NewClimbEstimator()
	base := time.Date(2026, 10, 3, 13, 0, 0, 0, time.UTC)

	for i := 0; i < 40; i++ {
		e.Observe(base.Add(time.Duration(i)*time.Second), 30000-float64(i)*6)
	}
	rate := e.Rate(base.Add(39*time.Second), 2*time.Second)
	if rate == nil {
		t.Fatal("no rate")
	}
	if *rate > -5.5 || *rate < -6.5 {
		t.Errorf("descent rate = %+0.3f m/s, want ~-6.0", *rate)
	}
}

// ---------------------------------------------------------------------------
// The nil cases, which are the ones that matter
// ---------------------------------------------------------------------------

// For the first minute of a flight there is no rate. It must be nil, because a
// zero here says the balloon is at float -- which is the one thing it definitely
// is not during an ascent.
func TestThereIsNoRateUntilTheWindowHasSpan(t *testing.T) {
	e := NewClimbEstimator()
	base := time.Date(2026, 10, 3, 14, 0, 0, 0, time.UTC)

	for i := 0; i < 60; i++ {
		e.Observe(base.Add(time.Duration(i)*time.Second), float64(i)*5)
		// Nil until the accepted samples span MinSpan, not merely until there are
		// enough of them: ten samples inside two seconds is a slope with a
		// near-zero denominator, and MinSpan is what catches that.
		spans := time.Duration(i) * time.Second
		spansMin := i > 0 && time.Duration(i-1)*time.Second < e.MinSpan
		if spansMin && spans < e.MinSpan {
			if got := e.Rate(base.Add(time.Duration(i)*time.Second), 2*time.Second); got != nil {
				t.Fatalf("after %d s the rate is %+0.3f, want nil", i+1, *got)
			}
		}
	}
	if got := e.Rate(base.Add(59*time.Second), 2*time.Second); got == nil {
		t.Error("after 60 s there is still no rate")
	}
}

func TestTooFewSamplesIsNotEnough(t *testing.T) {
	e := NewClimbEstimator()
	base := time.Date(2026, 10, 3, 15, 0, 0, 0, time.UTC)

	for i := 0; i < e.MinSamples-1; i++ {
		e.Observe(base.Add(time.Duration(i)*time.Second), float64(i)*5)
	}
	if got := e.Rate(base.Add(time.Duration(e.MinSamples-1)*time.Second), 2*time.Second); got != nil {
		t.Errorf("rate = %+0.3f from %d samples, want nil", *got, e.MinSamples-1)
	}
}

// A burst of samples inside one instant would satisfy a sample count while
// spanning no time at all, which is a slope with a near-zero denominator.
func TestSamplesMustSpanTimeAndNotJustCount(t *testing.T) {
	e := NewClimbEstimator()
	base := time.Date(2026, 10, 3, 16, 0, 0, 0, time.UTC)

	for i := 0; i < 20; i++ {
		e.Observe(base, float64(i)*5)
	}
	if got := e.Rate(base, 2*time.Second); got != nil {
		t.Errorf("rate = %+0.3f from 20 samples at one instant, want nil", *got)
	}
}

func TestAStaleFixHasNoRate(t *testing.T) {
	e := NewClimbEstimator()
	base := time.Date(2026, 10, 3, 17, 0, 0, 0, time.UTC)

	for i := 0; i < 40; i++ {
		e.Observe(base.Add(time.Duration(i)*time.Second), float64(i)*5)
	}
	// The newest sample is 30 s old and stale_after is 2 s, so "now" is 30 s past
	// the last fix rather than before it. A rate from it would be a claim about
	// the past.
	newest := base.Add(39 * time.Second)
	if got := e.Rate(newest.Add(30*time.Second), 2*time.Second); got != nil {
		t.Errorf("rate = %+0.3f from a fix 30 s old, want nil", *got)
	}
}

// ---------------------------------------------------------------------------
// Receiver re-acquisition
// ---------------------------------------------------------------------------

// When a receiver loses and regains lock it can come back with an altitude that
// disagrees with where it was by hundreds of metres. A regression through both
// fits cleanly and is completely wrong, which is the dangerous part: it produces
// a confident number rather than an obvious failure.
func TestAReacquisitionJumpIsRefusedRatherThanFitted(t *testing.T) {
	e := NewClimbEstimator()
	base := time.Date(2026, 10, 3, 18, 0, 0, 0, time.UTC)

	// A clean ascent, then the receiver comes back 500 m higher.
	for i := 0; i < 40; i++ {
		e.Observe(base.Add(time.Duration(i)*time.Second), float64(i)*5)
	}
	accepted := e.Observe(base.Add(40*time.Second), 200+500)

	if accepted {
		t.Fatal("a 500 m jump in one second was accepted as a fix")
	}

	// And the rate is still the ascent, not a line between two unrelated points.
	rate := e.Rate(base.Add(40*time.Second), 2*time.Second)
	if rate == nil {
		t.Fatal("refusing one sample must not destroy the window")
	}
	if math.Abs(*rate-5.0) > 0.3 {
		t.Errorf("rate after a refused jump = %+0.3f m/s, want ~+5.0", *rate)
	}
}

// A balloon in free fall reaches 50-100 m/s at low altitude, so the plausibility
// bound must not refuse a genuinely fast event.
func TestAFastButRealEventIsStillAccepted(t *testing.T) {
	e := NewClimbEstimator()
	base := time.Date(2026, 10, 3, 19, 0, 0, 0, time.UTC)

	for i := 0; i < 40; i++ {
		// 150 m/s at 1 Hz. Well inside the 200 m/s bound, well outside a balloon's
		// steady ascent.
		e.Observe(base.Add(time.Duration(i)*time.Second), float64(i)*150)
	}
	rate := e.Rate(base.Add(39*time.Second), 2*time.Second)
	if rate == nil {
		t.Fatal("a 150 m/s descent produced no rate")
	}
	if *rate < 100 {
		t.Errorf("rate = %+0.3f m/s, want a fast descent", *rate)
	}
}

func TestTheWindowForgetsOldSamples(t *testing.T) {
	e := NewClimbEstimator()
	base := time.Date(2026, 10, 3, 20, 0, 0, 0, time.UTC)

	// Five minutes of ascent at 5 m/s.
	for i := 0; i < 300; i++ {
		e.Observe(base.Add(time.Duration(i)*time.Second), float64(i)*5)
	}

	e.mu.Lock()
	held := len(e.samples)
	oldest := e.samples[0].at
	newest := e.samples[len(e.samples)-1].at
	e.mu.Unlock()

	// Age relative to the NEWEST sample. Measuring from the start of the stream
	// would say nothing about retention: the oldest retained sample is naturally
	// further along the timeline the longer the run goes.
	if age := newest.Sub(oldest); age > e.Window+time.Second {
		t.Errorf("oldest retained sample is %v old at the newest sample; the window is %v", age, e.Window)
	}
	// At 1 Hz over a 60 s window, a few seconds of slack and nothing more.
	if held > 70 {
		t.Errorf("%d samples retained over a %v window at 1 Hz; memory is not bounded", held, e.Window)
	}
}

// The whole estimator runs on one goroutine per receiver but must still be safe
// under -race, because the bank calls it from three.
func TestConcurrentObservationIsSafe(t *testing.T) {
	e := NewClimbEstimator()
	base := time.Date(2026, 10, 3, 21, 0, 0, 0, time.UTC)

	done := make(chan struct{})
	for g := 0; g < 3; g++ {
		go func(offset int) {
			defer func() { done <- struct{}{} }()
			for i := 0; i < 200; i++ {
				e.Observe(base.Add(time.Duration(i)*time.Second), float64(i*5+offset))
				_ = e.Rate(base.Add(time.Duration(i)*time.Second), 2*time.Second)
			}
		}(g)
	}
	for g := 0; g < 3; g++ {
		<-done
	}
}

package gnss

import (
	"sync"
	"time"
)

// ClimbEstimator turns a stream of altitude fixes into a vertical rate.
//
// Why not differencing
// --------------------
// The obvious implementation -- (h - h_prev) / (t - t_prev) -- does not work at
// this noise level. GNSS altitude is roughly 1.5-3 m RMS on a good antenna, so
// differencing two samples a second apart gives about 3 m of noise on a signal
// that is 5 m/s. The answer is +/-3 m/s and it means nothing.
//
// A least-squares slope over a window uses every sample instead of two, and the
// noise falls as the cube root of the sample count:
//
//	sigma_slope ~= sigma_alt * sqrt(12 / N^3)
//
//	 N=10  (10 s)  ->  0.33 m/s
//	 N=60  (60 s)  ->  0.02 m/s
//
// So a 60 s window resolves a balloon's whole vertical-speed range, and a
// differenced pair resolves nothing. This is the estimator the mission needs and
// it is worth saying why, because the diff version looks obviously right.
//
// The window is a compromise: shorter reacts faster to the transition into and
// out of float, longer resolves the rate when it is nearly zero, which is exactly
// when an operator most wants to know whether the balloon has stopped climbing.
type ClimbEstimator struct {
	// Window is how far back the fit reaches. Samples older than this are dropped
	// as new ones arrive, so memory is bounded by the window and the input rate.
	Window time.Duration

	// MinSamples is how many accepted samples are needed before a rate is
	// published at all. Below this the fit is defined but meaningless, and the
	// answer is nil rather than a confident-looking number.
	MinSamples int

	// MinSpan is the minimum time the accepted samples must cover, independent of
	// how many there are. Ten samples arriving in one second from a burst would
	// otherwise satisfy MinSamples and produce a slope with a near-zero
	// denominator.
	MinSpan time.Duration

	// MaxPlausibleSpeed rejects a fix whose implied vertical speed from the
	// previous accepted one exceeds it.
	//
	// This exists for receiver re-acquisition, not for the balloon. When a
	// receiver loses and regains lock it can come back with an altitude that
	// disagrees with where it was by hundreds of metres, and a regression over a
	// window containing both is a straight line between two unrelated numbers: it
	// fits cleanly and is completely wrong. A stratospheric balloon moves at
	// 5-8 m/s and reaches roughly 50-100 m/s in free fall, so 200 m/s is far
	// above any real flight and far below the implied speed of a jump.
	//
	// The cost is that a genuinely faster event is refused rather than reported.
	// For this mission that is the right way round: a missing rate is visibly
	// missing, and a bad one is not.
	MaxPlausibleSpeed float64

	mu       sync.Mutex
	samples  []altitudeSample
	last     altitudeSample
	haveLast bool
}

type altitudeSample struct {
	at   time.Time
	altM float64
}

// DefaultClimbWindow is the window a balloon flight wants.
//
// Sixty seconds resolves a 5 m/s ascent to about 0.02 m/s and still spans the
// time float detection needs -- the balloon slows over minutes, not seconds.
const DefaultClimbWindow = 60 * time.Second

func NewClimbEstimator() *ClimbEstimator {
	return &ClimbEstimator{
		Window:            DefaultClimbWindow,
		MinSamples:        10,
		MinSpan:           20 * time.Second,
		MaxPlausibleSpeed: 200,
	}
}

// Observe adds one fix.
//
// Returns false when the fix was refused, which happens only for an
// implausible step from the previous accepted altitude. Refusing is silent to the
// caller by design: a receiver that re-acquires produces exactly one bad fix, and
// surfacing it as an error would train someone to ignore the error.
func (e *ClimbEstimator) Observe(at time.Time, altM float64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.MaxPlausibleSpeed > 0 && e.haveLast {
		dt := at.Sub(e.last.at).Seconds()
		if dt > 0 {
			implied := (altM - e.last.altM) / dt
			if implied > e.MaxPlausibleSpeed || implied < -e.MaxPlausibleSpeed {
				return false
			}
		}
	}

	e.samples = append(e.samples, altitudeSample{at: at, altM: altM})
	e.last = altitudeSample{at: at, altM: altM}
	e.haveLast = true

	// Drop what the window no longer reaches. Kept rather than overwritten so the
	// ordering the regression depends on is the ordering the samples arrived in.
	cutoff := at.Add(-e.Window)
	drop := 0
	for drop < len(e.samples) && e.samples[drop].at.Before(cutoff) {
		drop++
	}
	if drop > 0 {
		e.samples = append(e.samples[:0], e.samples[drop:]...)
	}
	return true
}

// Rate returns the fitted vertical rate in m/s, or nil when one cannot be
// justified.
//
// Nil is a first-class answer and the most common one at the start of a flight.
// It means: not enough samples, or not enough elapsed time, or the newest sample
// is older than the staleness bound. It never means zero. A balloon that has
// genuinely stopped climbing reports a small number, and a consumer that cannot
// tell that from "no data yet" will eventually render a flight as having stalled
// during the first minute of every ascent.
func (e *ClimbEstimator) Rate(now time.Time, staleAfter time.Duration) *float64 {
	e.mu.Lock()
	defer e.mu.Unlock()

	if len(e.samples) < e.MinSamples {
		return nil
	}

	newest := e.samples[len(e.samples)-1].at
	if now.Sub(newest) > staleAfter {
		return nil
	}

	// Rebased on the first sample's timestamp before any arithmetic.
	//
	// Unix nanoseconds are about 1.8e18, and float64 carries 53 bits of
	// mantissa. Summing squares of raw Unix nanoseconds is therefore garbage long
	// before the window fills -- the fit would come back as noise, or as a
	// denominator of zero, depending on the epoch. Rebasing costs one subtraction
	// and is exact: the offset cancels in num, and den only scales, so the slope
	// is unchanged.
	base := e.samples[0].at

	var sumT, sumH float64
	for _, s := range e.samples {
		sumT += s.at.Sub(base).Seconds()
		sumH += s.altM
	}
	meanT := sumT / float64(len(e.samples))
	meanH := sumH / float64(len(e.samples))

	if e.MinSpan > 0 && newest.Sub(e.samples[0].at) < e.MinSpan {
		return nil
	}

	var num, den float64
	for _, s := range e.samples {
		dt := s.at.Sub(base).Seconds() - meanT
		num += dt * (s.altM - meanH)
		den += dt * dt
	}
	if den == 0 {
		// Every sample at the same instant, which MinSpan should already have
		// caught. Belt and braces: a zero denominator would be an infinity on the
		// wire, and proto3 will encode that as zero, which is the one number that
		// means "not climbing" to every consumer.
		return nil
	}

	rate := num / den
	return &rate
}

package sdr

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"syscall"

	"github.com/rocsar/obc/internal/domain"
)

// Where the program's data goes, and whether it will fit.
//
// # Why this file exists
//
// The acquisition program writes every capture twice:
//
//	ssd_dir + "/rx_data_" + timestamp + ".bin"                          -- SSD_PATH, absolute
//	"./../sdr-ettus-b200mini/Data/raw_data/rx_data_" + timestamp + ".bin" -- CWD-relative
//
// and it treats the two completely differently. The SSD write is wrapped in a
// try/catch that prints "[RX] SSD write failed" to stderr and carries on; the
// second is bare, and write_buffer_to_disk throws when the directory is absent,
// which escapes the RX thread and calls std::terminate.
//
// So the two failure modes are: the SSD silently becomes a fallback, or a
// successful session is reported as a crash. Neither is visible to an operator
// watching telemetry, and one of them loses the flight's data without saying so.
//
// Both are checked here, before the child starts, along with whether there is
// room for what the configured parameters will actually produce.

// sampleBytes is the size of one IQ sample in the program's buffer.
//
// std::complex<int16_t>, which is what both the stream format (sc16) and the
// buffer type in connect.cpp name. Two 16-bit components, no padding: the
// standard guarantees std::complex is layout-compatible with a two-element array
// of its value type.
const sampleBytes = 4

// sidecarRowBytes is the approximate on-disk size of one row of the timestamp
// CSVs the program also writes, per pulse, in Data/tx_timestamps and
// Data/rx_timestamps.
//
// The row is "index,usrp_time_s,unix_timestamp_s,system_time_iso" -- three
// numbers at 6-9 decimal places and a date-time to the microsecond, which lands
// around 55-60 bytes. Sixty is used because rounding it down would understate
// the requirement and rounding it up costs nothing: the CSVs are irrelevant
// beside the .bin, which is three orders of magnitude larger at any realistic
// operating point.
const sidecarRowBytes = 60

// captureBytes is the size of one capture, and of the two CSV sidecars, for a
// given set of parameters.
//
// The arithmetic is the program's own, from connect.cpp and libs/rx/rx.cpp:
//
//	total_pulses  = SESSION_DURATION * PRF
//	window_samps = (T_MAX_US - T_MIN_US) * 1e-6 * FS
//	total_samps  = total_pulses * window_samps
//
// and the file is total_samps * sizeof(complex<int16_t>).
//
// This is not an estimate. Against the parameters on the aircraft it reproduces
// the observed captures exactly: 2750 pulses x 3125 samples x 4 bytes is
// 34,375,000 bytes, which is what every 1-second capture on the Pi is, and the
// 2-second and 25-second captures are exactly 2x and 25x.
//
// The reason to compute rather than measure is that the number is known before
// the radio is touched. A disk with 30 MB free fails a 1-second capture
// partway through, after the acquisition has already been paid for in flight
// time, and the failure surfaces as an aborted child. Refusing to start is
// cheaper than discovering it in the air, and it is the only version of this
// check that can be done before the acquisition rather than after it.
func captureBytes(p domain.SdrParams) (int64, error) {
	windowUS := p.SweepMaxUs - p.SweepMinUs
	if windowUS <= 0 {
		return 0, fmt.Errorf("%w: the sweep window is empty (T_MAX_US=%g, T_MIN_US=%g)",
			ErrInvalidParams, p.SweepMaxUs, p.SweepMinUs)
	}
	duration := float64(p.SessionDurationS)
	if p.PRFHz <= 0 || p.SampleRateHz <= 0 || duration <= 0 {
		return 0, fmt.Errorf("%w: PRF, sample rate and session duration must all be positive", ErrInvalidParams)
	}

	// Truncated to a whole number, because the program does:
	//
	//	const size_t window_samps = static_cast<size_t>(window_dur * fs);
	//
	// At the aircraft's operating point the window is (200-100) us at 31.251
	// MS/s, which is 3125.1 samples -- and the program writes 3125 of them, not
	// 3125.1. A float left in here overstates every capture by the fractional
	// remainder times the pulse count, which is 1100 bytes on a 1-second run and
	// grows with session length. The truncation is not cosmetic here.
	windowSamples := uint64(windowUS * 1e-6 * p.SampleRateHz)
	pulses := uint64(duration * p.PRFHz)

	// The product is what overflows, so it is checked before the conversion: a
	// 24-hour session at a 30 kHz PRF over the widest permitted window is 1.5e18
	// samples, and the byte count is past int64. Silently wrapping to a small
	// number here would turn "does not fit" into "fits easily", which is the
	// exact inversion of what this check is for.
	samples := pulses * windowSamples
	if samples > uint64(math.MaxInt64)/sampleBytes {
		return 0, fmt.Errorf("%w: %g s at %g Hz over a %g us window is more than an "+
			"int64 of bytes; the parameters are not physically real",
			ErrInvalidParams, duration, p.PRFHz, windowUS)
	}

	bin := int64(samples) * sampleBytes
	sidecar := int64(pulses) * sidecarRowBytes
	return bin + sidecar, nil
}

// checkCaptureDestinations verifies that both places the program writes can
// accept a capture, and refuses if either cannot.
func (s *Service) checkCaptureDestinations(ctx context.Context) error {
	params, err := s.Params(ctx)
	if err != nil {
		return err
	}

	need, err := captureBytes(params)
	if err != nil {
		return err
	}

	ssd := s.dataDir
	// The second copy is relative to the child's working directory, which is the
	// program directory, so it is resolved the same way the program resolves it
	// rather than the way this process happens to be rooted.
	fallback := filepath.Join(s.programDir, programCaptureRelPath)

	if err := checkWritable(ssd, need); err != nil {
		return fmt.Errorf("%w: the SSD copy goes to %s: %w", ErrNoDataDir, ssd, err)
	}
	if err := checkWritable(fallback, need); err != nil {
		return fmt.Errorf("%w: the second copy goes to %s, which the program writes "+
			"without a fallback: if it cannot be opened the process aborts after the "+
			"session and the capture is reported as a failed run: %w",
			ErrNoDataDir, fallback, err)
	}

	s.log.Info("SDR capture destinations verified",
		"ssd", ssd, "fallback", fallback, "need_bytes", need,
		"session_s", params.SessionDurationS, "prf_hz", params.PRFHz)
	return nil
}

// checkWritable reports whether dir can take need bytes, distinguishing the ways
// it cannot.
//
// A missing directory and a full one are different problems with different
// fixes, and a message that says only "not writable" sends an operator to
// mkdir when they needed to delete something.
func checkWritable(dir string, need int64) error {
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s does not exist", dir)
		}
		return fmt.Errorf("cannot stat %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}

	probe, err := os.CreateTemp(dir, ".rocsar-space-*")
	if err != nil {
		return fmt.Errorf("%s is not writable: %w", dir, err)
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)

	// Bavail, not Bfree: the capture is written by this process, which is not
	// root, so the blocks reserved for root are not available to it. See
	// storage.Store.FreeSpace, which answers the same question for a Store root.
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return fmt.Errorf("cannot statfs %s: %w", dir, err)
	}
	free := uint64(st.Bavail) * uint64(st.Bsize)
	if free < uint64(need) {
		return fmt.Errorf("%s has %s free and the capture needs %s (%d bytes short)",
			dir, humanBytes(free), humanBytes(uint64(need)), need-int64(free))
	}
	return nil
}

// humanBytes formats a size for an error message, in decimal units, because the
// numbers being compared are the operator's own.
//
// An SD card's capacity and a filesystem's free space are both quoted in decimal
// by every tool that will print them here -- df, lsblk, blkid. Mixing in GiB
// would make a figure an operator has already seen somewhere not match the one
// they are being asked to act on.
func humanBytes(n uint64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit && exp < 3; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGT"[exp])
}

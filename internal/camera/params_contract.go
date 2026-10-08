package camera

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/rocsar/obc/internal/domain"
)

// The capture-settings contract, in one place.
//
// # Why this file exists
//
// Every bound here comes from the fswebcam(1) man page and nowhere else, and
// that is a weaker footing than the SDR table's. internal/sdr/params_contract.go
// cites a datasheet for a radio whose limits are physics. This one cites a man
// page for a program that is installed on the aircraft by apt and is not in
// this repository, is archived upstream, and whose version nobody has written
// down. There is no `fswebcam` source here to re-derive a bound from, so each
// Provenance string names the man-page line it came from -- and a reader who
// upgrades fswebcam can check the line rather than take this file's word.
//
// # The consequence, and what is done about it
//
// A man page is a promise about a program that may not be the one on the
// aircraft. The mitigation is not more validation, it is that a flag this code
// is unsure about is only ever passed when an operator asked for the setting it
// controls: fswebcamArgs omits every option that is at its default, so the
// proven pre-existing invocation is untouched and an unsupported flag can only
// break a capture whose operator specifically asked for that feature. See
// fswebcam.go.
//
// # The one thing that is not a bound
//
// Resolution is a request and is reported back as a fact from the decoded JPEG.
// There is no "maximum resolution" to validate against, because the ceiling
// belongs to a device this code cannot see: fswebcam's man page says outright
// that "the actual resolution used may differ if the source or device cannot
// capture at the specified resolution". So RESOLUTION is validated for SHAPE --
// is it a WxH at all -- and for a typo-guard pixel ceiling, and never checked
// against hardware.

// Key is one numerically settable capture setting.
type Key struct {
	// Name is the settings-file key, exactly as it is spelled in
	// .camera-params.json and as the GUI's JSON field.
	Name string

	// Min and Max bound the value. Every entry has both.
	Min, Max uint32

	// Unit is how the value is named in an error message. Empty means "no
	// unit", which is right for a compression factor and wrong for a delay --
	// an error saying "DELAY_MS = 10000 is out of range" and one saying the
	// same about a factor read very differently.
	Unit string

	// Provenance says where Min and Max come from. Every bound has one. A
	// bound with no stated source is a guess wearing the costume of a fact,
	// which is what the first version of the SDR table was.
	Provenance string
}

// Keys is the contract, in the order fswebcam's man page introduces them:
// format, then the capture options that shape the exposure, then the output
// factor.
var Keys = []Key{
	{
		Name:       "jpeg_quality",
		Min:        1,
		Max:        95,
		Provenance: "fswebcam(1): \"--jpeg <factor> ... The compression factor is a value between 0 and 95, or -1 for automatic.\" Max is the man page's own ceiling. Min is 1 rather than 0 because 0 is not a quality: it is the automatic case, carried in the zero by domain.CameraParams and never passed to the program, so a value of 0 arriving here as a requested factor is a caller that has confused the two. Lower factors are legal and useful at this link speed -- a factor near 40 is how a 320x240 preview becomes seconds instead of minutes over H1.",
	},
	{
		Name:       "frames",
		Min:        1,
		Max:        8,
		Unit:       "frames",
		Provenance: "fswebcam(1): \"-F, --frames <number> ... More frames mean less noise in the final image, however capture times will be longer and moving objects may appear blurred.\" Min is the man page's own default and the smallest useful average. Max is a policy bound and NOT from the man page, which gives no ceiling: it exists because of the second half of that sentence, on a gondola that is moving while the camera photographs it, where a long average smears the horizon into the sky. Eight is roughly a third of a second at 30 fps, which is about the longest exposure a moving platform can hold still.",
	},
	{
		Name:       "skip",
		Min:        1,
		Max:        60,
		Unit:       "frames",
		Provenance: "fswebcam(1): \"-S, --skip <number> ... These frames will be captured but won't be use. Use this option if your camera sends some bad or corrupt frames when it first starts capturing.\" Min is 1 because 0 is the man page's default and is carried by absence, not by a stored value -- a stored skip of 0 would be indistinguishable from never having set it. Max is a typo guard, not a device limit: 60 frames is about two seconds at 30 fps, which is longer than any USB webcam needs to settle, and its real purpose is to reject a mistyped 600 -- see also the frames+skip cross-field check in merged().",
	},
	{
		Name:       "delay_ms",
		Min:        1,
		Max:        10000,
		Unit:       "ms",
		Provenance: "fswebcam(1): \"-D, --delay <delay> ... Inserts a delay after the source or device has been opened and initialised, and before the capture begins. Some devices need this delay to let the image settle after a setting has changed.\" Max is a typo guard: ten seconds is longer than any exposure settle and comparable to the whole 20 s command timeout (client.CommandTimeout), past which the Ground Station has already given up on the reply. Min is 1 because 0 is absence -- no -D is passed at all.",
	},
}

// MaxTotalFrames bounds frames + skip together.
//
// The man page gives no frame rate and the device's is unknown, so this cannot
// be derived from the hardware. It is derived from the other end instead: the
// Ground Station's command round trip is 20 s (internal/client.CommandTimeout),
// and 120 frames is 24 s even at a conservative 5 fps and 4 s at 30 fps. So
// this rejects "skip 1000" and leaves "skip 10, frames 4" alone.
const MaxTotalFrames = 120

// MaxResolutionPixels is a typo guard on one axis of RESOLUTION, and is
// deliberately far above anything a USB camera offers.
//
// The real ceiling belongs to the device. This exists only so that a mistyped
// "19200x10800" -- three orders of magnitude too large -- is refused as a typo
// instead of being handed to a driver that will clamp it and hand back
// something nobody asked for. 7680 is the width of a UHD frame, which is
// larger than any camera on this aircraft.
const MaxResolutionPixels = 7680

// DefaultResolution and DefaultQuality restate the two defaults this file and
// fswebcamArgs reason about most, derived from the one function that owns them
// rather than written out again.
//
// Vars, not consts, because a function call is not a constant expression -- and
// the alternative, two literals restated in a third package, is exactly the
// drift this arrangement exists to prevent.
var (
	DefaultResolution = domain.DefaultCameraParams().Resolution
	DefaultQuality    = domain.DefaultCameraParams().JPEGQuality
)

// Defaults returns the parameters in force before anything is set.
//
// A thin re-export of domain.DefaultCameraParams, kept because every call site
// in this package reads better as Defaults() than as a qualified call into
// another package, and because the argument for the values belongs with the
// values.
func Defaults() domain.CameraParams { return domain.DefaultCameraParams() }

// keysByName indexes Keys.
var keysByName = func() map[string]Key {
	m := make(map[string]Key, len(Keys))
	for _, k := range Keys {
		m[k.Name] = k
	}
	return m
}()

// Lookup returns the contract entry for a settings key.
func Lookup(name string) (Key, bool) {
	k, ok := keysByName[name]
	return k, ok
}

// Validate checks a whole set of parameters.
//
// Every key is checked and every failure is reported, not just the first: an
// operator who has mistyped two fields learns about both from one refusal
// instead of fixing them one round trip at a time.
func Validate(p domain.CameraParams) error {
	var errs []string

	for _, k := range Keys {
		var v uint32
		switch k.Name {
		case "jpeg_quality":
			// 0 is automatic here, so it is not sent through the range check;
			// see Keys[0].Provenance.
			if p.JPEGQuality != 0 && (p.JPEGQuality < k.Min || p.JPEGQuality > k.Max) {
				errs = append(errs, k.explain(p.JPEGQuality))
			}
			continue
		case "frames":
			v = p.Frames
		case "skip":
			v = p.Skip
		case "delay_ms":
			v = p.DelayMs
		}
		if v < k.Min || v > k.Max {
			errs = append(errs, k.explain(v))
		}
	}

	if err := ValidateResolution(p.Resolution); err != nil {
		errs = append(errs, err.Error())
	}

	// The cross-field check, on the sum rather than on either value. Only runs
	// when both are individually in range, so a frames of 0 does not also get
	// reported as "0 + 1 frames exceeds 120".
	if p.Frames >= 1 && p.Frames <= 8 && p.Skip >= 1 && p.Skip <= 60 &&
		uint64(p.Frames)+uint64(p.Skip) > MaxTotalFrames {
		errs = append(errs, fmt.Sprintf(
			"frames + skip = %d exceeds %d: the camera would have to produce %d frames "+
				"inside one 20 s command round trip",
			p.Frames+p.Skip, MaxTotalFrames, p.Frames+p.Skip))
	}

	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("camera: %s", strings.Join(errs, "; "))
}

// ValidateResolution checks the shape of a `WxH` request.
//
// The man page's own caveat is why this validates the STRING and not the
// device: "the actual resolution used may differ if the source or device cannot
// capture at the specified resolution". A request that is well-formed is passed
// through and the result is reported decoded; a request that is not well-formed
// is refused here, because there is no reading of "1920" on its own.
func ValidateResolution(s string) error {
	w, h, err := parseResolution(s)
	if err != nil {
		return err
	}
	if w > MaxResolutionPixels || h > MaxResolutionPixels {
		return fmt.Errorf(
			"camera: resolution %s is a typo guard failure: one axis over %d px (%s)",
			s, MaxResolutionPixels, "no camera on this aircraft is that wide")
	}
	return nil
}

// parseResolution splits `WxH` into its two axes.
//
// Strict on purpose: `strings.Cut` on "x" alone would accept "x", "1920x" and
// "1920x1080p", and each of those is a value the driver would clamp into
// something this code would then have to describe as the resolution in force.
func parseResolution(s string) (uint32, uint32, error) {
	c, rest, ok := strings.Cut(s, "x")
	if !ok || rest == "" {
		return 0, 0, fmt.Errorf(
			"camera: resolution %q is not WxH; fswebcam's -r takes it as one argument, "+
				"for example %s", s, DefaultResolution)
	}
	w, err := parseDimension(c, "width", s)
	if err != nil {
		return 0, 0, err
	}
	h, err := parseDimension(rest, "height", s)
	if err != nil {
		return 0, 0, err
	}
	return w, h, nil
}

func parseDimension(tok, which, whole string) (uint32, error) {
	v, err := strconv.ParseUint(tok, 10, 32)
	if err != nil || v == 0 {
		return 0, fmt.Errorf(
			"camera: resolution %q has no usable %s: %q is not a positive whole number of pixels",
			whole, which, tok)
	}
	return uint32(v), nil
}

// explain formats one out-of-range field as an operator can act on it: which
// field, the value, the range, and where the range came from. The provenance is
// included because "quality 200 is invalid" is not a sentence anybody can do
// anything with.
func (k Key) explain(v uint32) string {
	unit := k.Unit
	if unit == "" {
		return fmt.Sprintf("camera: %s = %s is outside %d..%d (%s)",
			k.Name, strconv.FormatUint(uint64(v), 10), k.Min, k.Max, k.Provenance)
	}
	return fmt.Sprintf("camera: %s = %s %s is outside %d..%d %s (%s)",
		k.Name, strconv.FormatUint(uint64(v), 10), unit, k.Min, k.Max, unit, k.Provenance)
}

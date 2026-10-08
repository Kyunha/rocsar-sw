package camera

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/rocsar/obc/internal/domain"
	"github.com/rocsar/obc/internal/storage"
)

// ParamsFileName is where the capture settings live: a dot-prefixed file in the
// data root.
//
// # Why a file, and not a struct field
//
// Settings that live in memory are true until the next restart, and this system
// restarts: an OBC crash on the aircraft, a brownout, a deliberate reboot. A
// console that read its hints from camera_get_params after such a restart would
// show the defaults, and the operator would believe the resolution they set ten
// minutes ago was still in force -- the failure this file exists to prevent. It
// is the same argument that put the SDR's twelve keys in a file the C++ reads,
// and the same evidence: an OBC restart silently reverting a flight parameter is
// worse than not having the parameter.
//
// # Where it is, and why not in the photographs directory
//
// The data root, not the photos subdirectory. The photographs directory is
// listed to the operator as artefacts and every file in it is something a
// photograph produced; a settings file sitting there would be one entry in that
// list that no photograph produced. storage.List hides it (see the dotfile
// filter there), and the name begins with a dot because that is the convention
// this repository already uses for state that is not an artefact -- the capture
// scratch directory lives there for the same reason.
//
// # Precedence
//
// The stored file wins; the config keys are the initial value, consulted only
// when there is no file. One line of precedence with a test, rather than two
// homes for one fact that can be edited by hand in either place.
const ParamsFileName = ".camera-params.json"

// ErrNoParamsFile means there is no usable stored settings file, which is the
// normal state on a first flight and not a failure.
var ErrNoParamsFile = errors.New("camera: no stored parameters")

// reader is how LoadParams gets bytes out of the store, as a variable so the
// failure paths can be driven from a test without a filesystem that returns
// ENOSPC.
type reader func(*storage.Store, string) ([]byte, error)

// readStored is the real reader.
//
// Through store.Resolve rather than a joined path, so the one way to read a file
// outside the data root stays the one way -- the same rule internal/transport
// follows on the serving side.
var readStored reader = func(store *storage.Store, name string) ([]byte, error) {
	abs, err := store.Resolve(name)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(abs)
}

// LoadParams reads the stored settings.
//
// A file that is absent, unreadable, unparseable or out of bounds all return
// ErrNoParamsFile, and the reason rides along on the error. Those are
// hand-edited states, and the honest response to one is to fall back to settings
// known to work and say so in the log, rather than to refuse to photograph
// because of a typo in a file nobody asked for. A camera that cannot take a
// picture is not a better outcome than a camera that takes it at the documented
// defaults -- and every other layer in this system already degrades to a report
// instead of a refusal, per ARCHITECTURE.md 8.
func LoadParams(store *storage.Store) (domain.CameraParams, error) {
	return loadParams(store, readStored)
}

func loadParams(store *storage.Store, read reader) (domain.CameraParams, error) {
	body, err := read(store, ParamsFileName)
	if err != nil {
		if os.IsNotExist(err) {
			return domain.CameraParams{}, ErrNoParamsFile
		}
		return domain.CameraParams{}, fmt.Errorf("%w: %v", ErrNoParamsFile, err)
	}

	// domain.CameraParams' own JSON tags are what the file and the
	// camera_get_params reply both use, so the two are the same vocabulary. An
	// operator compares the two, and a settings file spelled differently from
	// the console would be a file nobody trusts.
	var p domain.CameraParams
	if err := json.Unmarshal(body, &p); err != nil {
		return domain.CameraParams{}, fmt.Errorf(
			"%w: %s is not the JSON this program writes (%v); the settings in force are the defaults",
			ErrNoParamsFile, ParamsFileName, err)
	}
	if err := Validate(p); err != nil {
		return domain.CameraParams{}, fmt.Errorf(
			"%w: %s holds settings this program refuses to use (%v); the settings in force are the defaults",
			ErrNoParamsFile, ParamsFileName, err)
	}
	return p, nil
}

// SaveParams writes the settings atomically.
//
// Through the store, so the write is a temp file plus a rename in the same
// directory: a settings file truncated by a power loss is the same class of
// failure as a truncated photograph, and the same fix applies to both.
func SaveParams(store *storage.Store, p domain.CameraParams) error {
	body, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("camera: encode parameters: %w", err)
	}
	body = append(body, '\n')
	if err := store.WriteFileAtomic(ParamsFileName, body, 0o600); err != nil {
		return fmt.Errorf("camera: write %s: %w", ParamsFileName, err)
	}
	return nil
}

// Merge applies a patch to the settings in force and validates the RESULT.
//
// The result, not the patch, because the result is the only thing that can be
// wrong: the usual edit leaves most fields alone, so the fields being judged are
// mostly ones nobody just typed. It is also how the SDR checks its sweep window,
// and for the same reason -- per-field bounds cannot see a pair.
func Merge(cur domain.CameraParams, patch domain.CameraParamsPatch) (domain.CameraParams, error) {
	out := cur
	if patch.JPEGQuality != nil {
		out.JPEGQuality = *patch.JPEGQuality
	}
	if patch.Resolution != nil {
		out.Resolution = *patch.Resolution
	}
	if patch.Frames != nil {
		out.Frames = *patch.Frames
	}
	if patch.Skip != nil {
		out.Skip = *patch.Skip
	}
	if patch.DelayMs != nil {
		out.DelayMs = *patch.DelayMs
	}
	if err := Validate(out); err != nil {
		return domain.CameraParams{}, err
	}
	return out, nil
}

// Empty reports whether a patch names nothing at all.
//
// One predicate, because this is the check that decides whether a request is
// worth putting on the wire. An all-nil patch is refused rather than treated as
// a no-op: an operator who pressed apply expects a change, and a silent success
// is indistinguishable from a broken button.
func Empty(p domain.CameraParamsPatch) bool {
	return p.JPEGQuality == nil && p.Resolution == nil && p.Frames == nil &&
		p.Skip == nil && p.DelayMs == nil
}

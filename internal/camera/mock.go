package camera

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"log/slog"
	"sync"

	"github.com/rocsar/obc/internal/domain"
	"github.com/rocsar/obc/internal/storage"
)

// Mock is a camera that writes a synthetic JPEG.
//
// It produces a REAL JPEG, not a byte slice that happens to be called one. A
// mock that returns arbitrary bytes would let a broken encoder through, and the
// failure would appear as "the Ground Station cannot display the photo" long
// after the capture code was written.
//
// It is never used unless asked for. See ARCHITECTURE.md 9.
type Mock struct {
	device string
	dir    string
	store  *storage.Store
	log    *slog.Logger

	mu       sync.Mutex
	state    domain.SubsystemState
	shots    uint64
	last     *domain.Photo
	failWith error
	params   domain.CameraParams
}

var _ domain.Camera = (*Mock)(nil)

// mockWidth and mockHeight are the shape of the synthetic frame.
//
// Deliberately not the configured resolution, and that difference is the point.
// This camera has no sensor, so there is nothing to negotiate a format with, and
// a mock that drew 1920x1080 because it was asked to would be a mock reporting a
// resolution no device agreed to -- the exact fabrication the real camera spends
// its decodable dimensions to avoid. So a mock photograph is 320x240 whatever
// the settings say, which makes the requested-versus-actual distinction visible
// in the one place somebody can look at it without an aircraft.
const (
	mockWidth  = 320
	mockHeight = 240
)

// NewMock returns a mock camera writing into dir.
//
// The settings argument is validated and stored exactly as the real camera
// stores it, so the console's read-modify-write round trip and its version-skew
// handling are exercised identically under --mock-camera. What the mock does with
// them is a narrower question, answered in SetParams.
func NewMock(device, dir string, store *storage.Store, initial domain.CameraParams, log *slog.Logger) *Mock {
	if log == nil {
		log = slog.Default()
	}
	params := initial
	if err := Validate(params); err != nil {
		log.Warn("the configured camera settings are not usable; using the defaults",
			"err", err, "defaults", Defaults())
		params = Defaults()
	}
	return &Mock{
		device: device,
		dir:    dir,
		store:  store,
		log:    log,
		params: params,
		state:  domain.SubsystemDisconnected,
	}
}

// MockFailWith makes every capture fail, to exercise the degraded path.
func MockFailWith(err error) func(*Mock) {
	return func(m *Mock) { m.failWith = err }
}

func (m *Mock) Device() string { return m.device }

func (m *Mock) State() domain.SubsystemState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

func (m *Mock) PhotosTaken() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.shots
}

// LastPhoto is the most recent photograph, or nil.
func (m *Mock) LastPhoto() *domain.Photo {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.last == nil {
		return nil
	}
	out := *m.last
	return &out
}

func (m *Mock) Params(ctx context.Context) (domain.CameraParams, error) {
	if err := ctx.Err(); err != nil {
		return domain.CameraParams{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.params, nil
}

// SetParams stores a partial update, honouring exactly one of the settings.
//
// JPEGQuality is applied, because it is the one setting a synthetic frame can
// genuinely honour: it changes the bytes this mock writes, and an operator
// watching the file size move knows the control is wired to something.
//
// The other four are stored and validated and then deliberately NOT applied.
// Pretending otherwise would make --mock-camera the one place in this system
// where a resolution control appears to work, and H5 -- nothing is simulated
// unless it is labelled -- is not about a mock being honest so much as it is
// about not being able to mislead.
//
// The settings are not persisted to ParamsFileName either. There is one file and
// it belongs to the real camera; a mock run on a laptop sharing a data directory
// with an aircraft's settings is not a scenario worth writing a test for.
func (m *Mock) SetParams(ctx context.Context, patch domain.CameraParamsPatch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if Empty(patch) {
		return fmt.Errorf("camera: no settings were given; a partial update must name at least one")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	next, err := Merge(m.params, patch)
	if err != nil {
		return err
	}
	m.params = next
	return nil
}

func (m *Mock) Capture(ctx context.Context) (*domain.Photo, error) {
	m.mu.Lock()
	fail := m.failWith
	quality := m.params.JPEGQuality
	m.state = domain.SubsystemBusy
	m.mu.Unlock()

	if fail != nil {
		m.mu.Lock()
		m.state = domain.SubsystemError
		m.mu.Unlock()
		return nil, fail
	}

	body, err := m.syntheticJPEG(quality)
	if err != nil {
		return nil, err
	}

	// The shot number is in the name so two captures in the same second do not
	// collide and overwrite each other. It is also what makes the returned name
	// usable for telling two mock photographs apart.
	name := storage.UniqueNameFor(storage.KindCamera,
		fmt.Sprintf("mock%d", m.shots), "jpg")
	sub, err := m.store.Sub(m.dir)
	if err != nil {
		return nil, err
	}
	if err := sub.WriteFileAtomic(name, body, 0o644); err != nil {
		return nil, err
	}

	// Decoded from the bytes just written rather than asserted, so the mock goes
	// through the same code path the real camera does and cannot drift from it.
	w, h := jpegSize(body)

	photo := &domain.Photo{
		Name:      name,
		SizeBytes: uint64(len(body)),
		Kind:      storage.KindCamera,
		Path:      m.dir + "/" + name,
		Width:     w,
		Height:    h,
	}

	m.mu.Lock()
	m.state = domain.SubsystemReady
	m.shots++
	m.last = photo
	m.mu.Unlock()

	return photo, nil
}

// syntheticJPEG draws an image with a visible frame counter, so an operator can
// tell at a glance which photograph they are looking at.
//
// The automatic quality case passes NO options object at all, and that is not a
// style choice. image/jpeg does not treat a zero factor as "automatic": it
// clamps anything below 1 UP to 1 (writer.go: "Clip quality to [1, 100]"), so
// passing Options{Quality: 0} would produce the smallest and ugliest file the
// encoder can make and call it the default. The automatic case is the absence
// of an option, which is exactly how fswebcam spells it too -- -1.
func (m *Mock) syntheticJPEG(quality uint32) ([]byte, error) {
	w, h := mockWidth, mockHeight
	img := image.NewRGBA(image.Rect(0, 0, w, h))

	n := 0
	m.mu.Lock()
	n = int(m.shots)
	m.mu.Unlock()

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			// A visible gradient plus a moving band: two mock photographs are
			// always distinguishable, which is what makes this useful for
			// checking that the fetch path serves distinct files.
			band := (x + n*17) % 40
			v := uint8((x * 255 / w))
			if band < 4 {
				v = 255
			}
			img.Set(x, y, color.RGBA{R: v, G: uint8(y * 255 / h), B: 0x40, A: 0xFF})
		}
	}

	opts := &jpeg.Options{}
	if quality != DefaultQuality {
		opts.Quality = int(quality)
	}

	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, opts); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

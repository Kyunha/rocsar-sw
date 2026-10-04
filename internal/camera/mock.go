package camera

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
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

	mu       sync.Mutex
	state    domain.SubsystemState
	shots    uint64
	last     string
	failWith error
}

var _ domain.Camera = (*Mock)(nil)

// NewMock returns a mock camera writing into dir.
func NewMock(device, dir string, store *storage.Store) *Mock {
	return &Mock{device: device, dir: dir, store: store, state: domain.SubsystemDisconnected}
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

func (m *Mock) Capture(ctx context.Context) (*domain.Photo, error) {
	m.mu.Lock()
	fail := m.failWith
	m.state = domain.SubsystemBusy
	m.mu.Unlock()

	if fail != nil {
		m.mu.Lock()
		m.state = domain.SubsystemError
		m.mu.Unlock()
		return nil, fail
	}

	body, err := m.syntheticJPEG()
	if err != nil {
		return nil, err
	}

	name := storage.NameFor(storage.KindCamera, "jpg")
	sub, err := m.store.Sub(m.dir)
	if err != nil {
		return nil, err
	}
	if err := sub.WriteFileAtomic(name, body, 0o644); err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.state = domain.SubsystemReady
	m.shots++
	m.last = name
	m.mu.Unlock()

	return &domain.Photo{
		Name:      name,
		SizeBytes: uint64(len(body)),
		Kind:      storage.KindCamera,
		Path:      m.dir + "/" + name,
	}, nil
}

// syntheticJPEG draws a small image with a visible frame counter, so an
// operator can tell at a glance which photograph they are looking at.
func (m *Mock) syntheticJPEG() ([]byte, error) {
	const w, h = 160, 120
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

	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 80}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

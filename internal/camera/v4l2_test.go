package camera

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"
)

// The VIDIOC request codes, byte for byte, from linux/videodev2.h.
//
// These were all `iocRead|iocWrite`. QUERYCAP is `_IOR`, so the encoded value
// came out as 0xc0685600 where the kernel wants 0x80685600, and the first call
// against a real UVC camera failed with ENOTTY -- "inappropriate ioctl for
// device", which is the least helpful error the kernel could possibly give.
//
// Nothing local could have caught it. The number is computed, it is plausible,
// it is the right size, and no Go V4L2 package exists to disagree with us. The
// PTY tests exercise the serial link, not the camera. It took a real camera on a
// real Pi.
//
// So the expected values are pinned here, as literals from the header. That is
// the one thing this file cannot check for itself: it is the external reference.
func TestV4L2RequestCodesAgainstTheKernelHeader(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  uintptr
		want uintptr // from linux/videodev2.h on aarch64
	}{
		// _IOR('V', 0, struct v4l2_capability) -- READ, direction 2.
		{"VIDIOC_QUERYCAP", vidiocQueryCap, 0x80685600},
		// _IOR('V', 4, struct v4l2_format) -- READ, direction 2.
		{"VIDIOC_G_FMT", vidiocG_Fmt, 0xc0d05604},
		// _IOWR('V', 5, struct v4l2_format) -- direction 3.
		{"VIDIOC_S_FMT", vidiocS_Fmt, 0xc0d05605},
		// _IOWR('V', 8, struct v4l2_requestbuffers) -- direction 3.
		{"VIDIOC_REQBUFS", vidiocReqBufs, 0xc0145608},
		// _IOWR('V', 11, struct v4l2_buffer) -- direction 3.
		{"VIDIOC_QBUF", vidiocQBUF, 0xc058560f},
		// _IOWR('V', 13, struct v4l2_buffer) -- direction 3.
		{"VIDIOC_DQBUF", vidiocDQBUF, 0xc0585611},
		// _IOW('V', 18, int) -- WRITE, direction 1, size 4. An int, not a struct:
		// there is no struct v4l2_buf_type, the name is an enum.
		{"VIDIOC_STREAMON", vidiocStreamOn, 0x40045612},
		{"VIDIOC_STREAMOFF", vidiocStreamOff, 0x40045613},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = 0x%08x, want 0x%08x (direction %d, want %d)",
				tc.name, tc.got, tc.want, tc.got>>30, tc.want>>30)
		}
	}
}

// The sizes are half of the encoding, and they are ours rather than the
// kernel's: the request code is computed from unsafe.Sizeof of a Go struct. If
// Go lays one of these out differently from C, the encoded request is wrong in
// the same silent way.
//
// sizeof from linux/videodev2.h on aarch64.
func TestV4L2StructSizesMatchTheKernel(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"v4l2_capability", unsafe.Sizeof(v4l2Capability{}), 104},
		{"v4l2_format", unsafe.Sizeof(v4l2Format{}), 208},
		{"v4l2_requestbuffers", unsafe.Sizeof(v4l2ReqBufs{}), 20},
		{"v4l2_buffer", unsafe.Sizeof(v4l2Buffer{}), 88},
		// STREAMON takes a bare int, so this is 4 on every architecture.
		{"int (VIDIOC_STREAMON arg)", unsafe.Sizeof(v4l2BufType(0)), 4},
	} {
		if tc.got != tc.want {
			t.Errorf("sizeof(%s) = %d, want %d -- the request code encodes this "+
				"size, so a mismatch makes the ioctl unrecognised", tc.name, tc.got, tc.want)
		}
	}
}

// The direction bits are the part that was wrong, and they were wrong
// identically for every ioctl. Worth stating on its own: "READ and WRITE" is
// not a safe default, it is a third thing that is correct only when the header
// says _IOWR.
func TestIoCDirectionBitsAreNotAllTheSame(t *testing.T) {
	dir := func(v uintptr) uintptr { return v >> 30 }

	cases := map[string]uintptr{
		"VIDIOC_QUERYCAP":  dir(vidiocQueryCap),
		"VIDIOC_G_FMT":     dir(vidiocG_Fmt),
		"VIDIOC_S_FMT":     dir(vidiocS_Fmt),
		"VIDIOC_STREAMON":  dir(vidiocStreamOn),
		"VIDIOC_STREAMOFF": dir(vidiocStreamOff),
	}
	want := map[string]uintptr{
		// QUERYCAP is _IOR. G_FMT looks like a read but is _IOWR, because the
		// kernel writes the format it actually settled on back into the struct.
		"VIDIOC_QUERYCAP": iocRead,
		"VIDIOC_G_FMT":    iocRead | iocWrite,
		// Read-modify-write: S_FMT reads the current format back to report what
		// the driver actually did with the request.
		"VIDIOC_S_FMT": iocRead | iocWrite,
		// Stream control carries only a buffer type; it writes nothing back.
		"VIDIOC_STREAMON":  iocWrite,
		"VIDIOC_STREAMOFF": iocWrite,
	}
	for name, got := range cases {
		if got != want[name] {
			t.Errorf("direction bits of %s = %d, want %d", name, got, want[name])
		}
	}
}

// If the machine has the kernel headers, check against the real thing rather
// than against the literals above. The literals are the floor; this is the
// authority. Skipped when gcc or the headers are absent, which is normal on a
// developer laptop and never true on the target.
func TestV4L2CodesAgainstRealHeadersWhenAvailable(t *testing.T) {
	cc, err := exec.LookPath("gcc")
	if err != nil {
		t.Skip("no gcc; the pinned literals above are the check")
	}

	src := `
#include <stdio.h>
#include <linux/videodev2.h>
int main(void) {
  printf("0x%08lx\n", (unsigned long)VIDIOC_QUERYCAP);
  printf("0x%08lx\n", (unsigned long)VIDIOC_G_FMT);
  printf("0x%08lx\n", (unsigned long)VIDIOC_S_FMT);
  printf("0x%08lx\n", (unsigned long)VIDIOC_REQBUFS);
  printf("0x%08lx\n", (unsigned long)VIDIOC_QBUF);
  printf("0x%08lx\n", (unsigned long)VIDIOC_DQBUF);
  printf("0x%08lx\n", (unsigned long)VIDIOC_STREAMON);
  printf("0x%08lx\n", (unsigned long)VIDIOC_STREAMOFF);
  return 0;
}
`
	f := filepath.Join(t.TempDir(), "probe.c")
	if err := os.WriteFile(f, []byte(src), 0o600); err != nil {
		t.Skipf("cannot stage the C probe: %v", err)
	}

	out, err := exec.Command(cc, "-o", f+".bin", f).CombinedOutput()
	if err != nil {
		t.Skipf("cannot compile the C probe (missing videodev2.h?): %v\n%s", err, out)
	}
	got, err := exec.Command(f + ".bin").Output()
	if err != nil {
		t.Skipf("cannot run the C probe: %v", err)
	}

	ours := []uintptr{
		vidiocQueryCap, vidiocG_Fmt, vidiocS_Fmt, vidiocReqBufs,
		vidiocQBUF, vidiocDQBUF, vidiocStreamOn, vidiocStreamOff,
	}
	names := []string{
		"VIDIOC_QUERYCAP", "VIDIOC_G_FMT", "VIDIOC_S_FMT", "VIDIOC_REQBUFS",
		"VIDIOC_QBUF", "VIDIOC_DQBUF", "VIDIOC_STREAMON", "VIDIOC_STREAMOFF",
	}

	var theirs []uintptr
	for _, line := range strings.Split(strings.TrimSpace(string(got)), "\n") {
		var v uintptr
		if _, err := fmt.Sscanf(line, "0x%x", &v); err != nil {
			t.Fatalf("unexpected probe output %q: %v", line, err)
		}
		theirs = append(theirs, v)
	}
	if len(theirs) != len(ours) {
		t.Fatalf("the probe printed %d values, expected %d", len(theirs), len(ours))
	}
	for i := range ours {
		if ours[i] != theirs[i] {
			t.Errorf("%s = 0x%08x, the kernel header says 0x%08x", names[i], ours[i], theirs[i])
		}
	}
}

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	rocsarv1 "github.com/rocsar/obc/api/rocsar/v1"
)

// printTelemetry renders one frame as a fixed-shape block.
//
// Fixed shape on purpose: an operator watching a scrolling console wants the same
// lines every second so their eye finds the changed one. Proportional or
// conditional layout defeats that.
//
// Every value is qualified. A reading that is absent, held or simulated says so,
// because a bare number is indistinguishable from a real one -- and a Ground
// Station that renders a held value as a live one is the failure this project
// exists to prevent.
func printTelemetry(f *rocsarv1.TelemetryFrame) {
	s := f.System
	up := time.Duration(s.GetUptimeS()) * time.Second

	fmt.Printf("[seq %-6d] %s  uptime %s  %.0f C\n",
		f.Sequence, s.GetState(), up.Truncate(time.Second), s.GetCpuTempC())

	if m := s.GetMockedSubsystems(); len(m) > 0 {
		fmt.Printf("            SIMULATED: %s   <- fabricated, not measured\n", strings.Join(m, ", "))
	}
	if r := s.GetState(); r != rocsarv1.SubsystemState_SUBSYSTEM_READY {
		fmt.Printf("            system is %s\n", r)
	}

	for _, g := range f.Gnss {
		sel := " "
		if g.GetSelected() {
			sel = "*"
		}
		if !g.GetFixOk() {
			// Deliberately does not print the coordinates. A receiver with no fix
			// still carries the last values it decoded, and printing them next to
			// "no fix" invites reading the number instead of the qualifier.
			fmt.Printf("         gnss%s %d  no fix   <- not a position\n", sel, g.GetReceiverId())
			continue
		}
		fmt.Printf("         gnss%s %d  %10.6f, %11.6f  alt %7.1f m  %5.1f m/s  crs %5.1f\n",
			sel, g.GetReceiverId(), g.GetLatitudeDeg(), g.GetLongitudeDeg(),
			g.GetAltitudeM(), g.GetGroundSpeedMps(), g.GetCourseDeg())
	}

	if p := f.Pico; p != nil {
		conn := "NOT connected"
		if f.GetPicoConnected() {
			conn = "connected"
		}
		fmt.Printf("         pico   %s  heading %.2f (target %.2f)  imu %s  heaters %s/%s\n",
			conn, p.GetGondolaHeadingDeg(), p.GetTargetHeadingDeg(),
			measuredOrHeld(p.GetImuPresent()), onOff(p.GetHeater1State()), onOff(p.GetHeater2State()))
		for _, a := range p.GetAntennas() {
			load := "load ?"
			if a.GetFeedbackState() == 1 {
				// Only meaningful when the servo actually answered. A held value is
				// the last reading or the command echo, and printing it as a
				// measurement is the thing this column exists to prevent.
				load = fmt.Sprintf("load %d%% %dC", a.GetLoad(), a.GetTemperatureC())
			} else {
				load = "load held"
			}
			fmt.Printf("                servo %d  tick %4d  %7.2f deg  %-12s  %s\n",
				a.GetServoId(), a.GetCurrentTick(), a.GetCurrentAngleDeg(),
				load, feedbackName(a.GetFeedbackState()))
			if e := a.GetFeedbackError(); e != 0 {
				// The ST3215 reports overheat and overload here while answering
				// perfectly well-formed frames, so a servo in trouble reads clean
				// unless this is looked at.
				fmt.Printf("                servo %d  FAULT %s\n", a.GetServoId(), rocsarv1.ErrorCode(a.GetFeedbackError()).String())
			}
		}
	}

	if c := f.Camera; c != nil {
		fmt.Printf("         camera %s  %d photo(s)", c.GetState(), c.GetPhotosTaken())
		if n := c.GetLastPhotoName(); n != "" {
			fmt.Printf("  last %s", n)
		}
		fmt.Println()
	}
	if d := f.Sdr; d != nil {
		line := fmt.Sprintf("         sdr    %s", d.GetState())
		if d.GetRunning() {
			line += fmt.Sprintf("  running pid %d", d.GetPid())
		}
		if e := d.GetLastError(); e != "" {
			line += fmt.Sprintf("  last error: %s", e)
		}
		fmt.Println(line)
	}
	if l := f.Link; l != nil {
		shaping := "shaping off"
		if l.GetShapingActive() {
			shaping = "shaping on"
		} else if r := l.GetInactiveReason(); r != "" {
			shaping = "shaping off: " + r
		}
		fmt.Printf("         link   %s  %s  %d kbit/s  %s\n",
			l.GetState(), l.GetDevice(), l.GetRateKbps(), shaping)
	}
	fmt.Println()
}

func measuredOrHeld(present bool) string {
	if present {
		return "MEASURED"
	}
	return "HELD"
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func feedbackName(v int32) string {
	switch v {
	case 1:
		return "MEASURED"
	case 2:
		return "HELD"
	case 0:
		return "no reading"
	default:
		return fmt.Sprintf("state %d", v)
	}
}

// fetch downloads an artefact.
//
// Two things it does that a bare GET does not:
//
//   - It resumes. The link is 115 kbit/s and the files are tens of megabytes, so
//     an interrupted download is the normal case rather than the exceptional one.
//     The server answers a Range request with 206, and this asks for the remainder.
//   - It writes to a temporary file and renames, so an interrupted transfer never
//     leaves a truncated file that looks complete. A partial photograph that looks
//     whole is worse than no photograph.
func (c *client) fetch(ctx context.Context, name, out string, resume bool) error {
	// Validate the name before it becomes part of a URL and a filesystem path.
	//
	// Subdirectories are ALLOWED, and they have to be: the listing reports names
	// like `photos/camera-20261004-223347.jpg`, so a stricter rule here makes
	// `gs_cli ls` output impossible to paste straight into `gs_cli fetch`. The first
	// version of this rejected every "/" and so refused the exact names the tool one
	// line earlier had printed.
	//
	// Refused is anything that could leave the artefact root: a parent traversal, a
	// leading slash, a backslash, or an empty component. The server validates this
	// properly too; refusing here means the tool says so itself rather than relying
	// on the far side.
	if name == "" {
		return errors.New("no artefact name given; `gs_cli ls` lists them")
	}
	if strings.Contains(name, "..") || strings.HasPrefix(name, "/") ||
		strings.Contains(name, "\\") || strings.Contains(name, "//") {
		return fmt.Errorf("%q is not an artefact name inside the data root", name)
	}

	url := c.http + "/" + name

	// Resume: find out how much of the partial file is already there.
	var have int64
	partial := out + ".part"
	if resume {
		if st, err := os.Stat(partial); err == nil {
			have = st.Size()
			fmt.Printf("resuming at %d bytes\n", have)
		}
	}
	if !resume {
		_ = os.Remove(partial)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", name, err)
	}
	defer resp.Body.Close()

	// A server that ignored the Range answers 200 with the whole body. Appending
	// that to the partial file would silently corrupt it.
	truncated := have > 0 && resp.StatusCode != http.StatusPartialContent
	if truncated {
		fmt.Printf("server ignored the Range (HTTP %d); starting over\n", resp.StatusCode)
		have = 0
	}

	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
	default:
		return fmt.Errorf("fetch %s: HTTP %d", name, resp.StatusCode)
	}

	if total := resp.ContentLength; total > 0 && resp.StatusCode != http.StatusPartialContent {
		fmt.Printf("%s: %d bytes\n", name, total)
	}

	dst := os.Stdout
	var f *os.File
	if out != "" {
		flags := os.O_CREATE | os.O_WRONLY
		if have > 0 {
			flags |= os.O_APPEND
		} else {
			flags |= os.O_TRUNC
		}
		f, err = os.OpenFile(partial, flags, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		dst = f
	}

	start := time.Now()
	written, err := io.Copy(dst, resp.Body)
	if err != nil {
		return fmt.Errorf("%s: interrupted after %d bytes: %w", name, written, err)
	}
	if f != nil {
		if err := f.Close(); err != nil {
			return err
		}
		if err := os.Rename(partial, out); err != nil {
			return err
		}
	}

	total := have + written
	el := time.Since(start).Seconds()
	where := out
	if where == "" {
		where = "stdout"
	} else {
		where = filepath.Base(where)
	}
	fmt.Fprintf(os.Stderr, "wrote %s: %d bytes in %.1fs (%.1f kB/s)\n",
		where, total, el, float64(total)/1024/maxf(el, 0.001))
	return nil
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

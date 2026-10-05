// Command sdr_bench drives the SDR without the OBC.
//
// parameters.json is a frozen contract with C++ nobody here maintains, it is
// read with `j.at()` which THROWS on a missing key, and it is validated by
// nothing at all on the C++ side. Every one of those facts is invisible from the
// Go code and obvious from here, which is why this tool exists.
//
//	sdr_bench validate            the contract: what is required, what is bounded
//	sdr_bench show               the current parameters
//	sdr_bench set PRF=3000       a partial update, validated and written atomically
//	sdr_bench probe              uhd_usrp_probe
//	sdr_bench connect            start ./connect detached
//	sdr_bench reset-usb          power-cycle the SDR's USB port
//
// It composes internal/sdr, so it validates with exactly the table the server
// uses and cannot disagree with it about what is legal.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/rocsar/obc/internal/domain"
	"github.com/rocsar/obc/internal/sdr"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "sdr_bench: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	program := flag.String("program", "third_party/sdr-ettus-b200mini",
		"directory holding parameters/ and Data/ -- NOT the connect binary, which is found on PATH")
	dataDir := flag.String("data", "/mnt/rocsar/data",
		"data directory, where SDR logs are written")
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		printUsage()
		return errors.New("no command given")
	}

	svc := sdr.NewService(*program, *dataDir, discardLogger())
	ctx := context.Background()

	switch args[0] {
	case "validate":
		return validate(svc, *program)
	case "show":
		return show(svc)
	case "set":
		return set(ctx, svc, args[1:])
	case "probe":
		out, err := svc.Probe(ctx)
		fmt.Print(out)
		if err != nil {
			return err
		}
		return nil
	case "connect":
		if err := svc.Connect(ctx); err != nil {
			return err
		}
		fmt.Printf("started, pid %d\nlog: %s\n", svc.PID(), svc.LastLog())
		fmt.Println("the program's own output goes to that log; tail it")
		return nil
	case "reset-usb":
		if err := svc.ResetUSB(ctx); err != nil {
			return err
		}
		fmt.Println("USB port power-cycled")
		return nil
	case "-h", "--help", "help":
		printUsage()
		return nil
	default:
		printUsage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func printUsage() {
	fmt.Println(`sdr_bench <command>

  validate      print the params.json contract: required keys and their bounds
  show          print the current parameters
  set K=V ...   apply a partial update, validated and written atomically
  probe         run uhd_usrp_probe
  connect       start connect detached, output to a timestamped log
  reset-usb     power-cycle the SDR's USB port

set accepts: PRF FS TX_FREQ BW NORMALIZED_GAIN_TX NORMALIZED_GAIN_RX SESSION_DURATION

  sdr_bench set PRF=3000
  sdr_bench set TX_FREQ=5.7e9 BW=40e6

Anything not named is left alone. PULSE_DURATION is not accepted: config.hpp
does not read it, so setting it would do nothing and appear to work.

-program names the directory holding parameters/ and Data/, not the binary.
The connect program is found on PATH. That directory must be called
sdr-ettus-b200mini, because connect.cpp resolves its own config path relative
to its working directory; validate says so if it is not.`)
}

// validate prints the contract.
//
// This is the command that answers "what does connect.cpp actually need?", which
// otherwise means reading C++ on the aircraft.
func validate(svc *sdr.Service, program string) error {
	// The path comes from the Service, not from a second derivation here. The
	// file the server edits and the file this reports on have to be the same
	// one, and the C++ has a third opinion about it that only CheckProgramConfig
	// can settle.
	path := svc.ParamsPath()
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	raw := map[string]any{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}

	fmt.Printf("contract from internal/sdr/params_contract.go\n")
	fmt.Printf("  %-20s %-9s %-14s %-14s %s\n", "KEY", "REQUIRED", "MIN", "MAX", "IN FILE")
	fmt.Println(strings.Repeat("-", 96))

	bad := 0
	for _, k := range sdr.Keys {
		req := "no"
		if k.Required {
			req = "yes"
		}
		val, present := raw[k.Name]
		inFile := "absent"
		switch {
		case !present:
			if k.Required {
				inFile = "MISSING"
				bad++
			}
		case k.Min == 0 && k.Max == 0:
			inFile = fmt.Sprintf("%v", val) // a string parameter
		default:
			f, isNum := val.(float64)
			if !isNum {
				inFile = fmt.Sprintf("%v (not a number!)", val)
				bad++
				break
			}
			inFile = fmt.Sprintf("%g", f)
			if err := sdr.ValidateValue(k.Name, f); err != nil {
				inFile += "  OUT OF RANGE"
				bad++
			}
		}
		fmt.Printf("  %-20s %-9s %-14s %-14s %s\n",
			k.Name, req, bound(k.Min), bound(k.Max), inFile)
	}

	// Keys the program ignores but the file carries. Not an error; worth seeing.
	var extra []string
	for name := range raw {
		if _, ok := sdr.Lookup(name); !ok {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		fmt.Printf("\n  also present, not part of the modelled contract: %s\n", strings.Join(extra, ", "))
		fmt.Printf("  they are carried through every update untouched\n")
	}

	fmt.Println()
	for _, k := range sdr.Keys {
		if k.Min == 0 && k.Max == 0 {
			continue
		}
		fmt.Printf("  %s\n    %s\n\n", k.Name, k.Provenance)
	}

	if bad > 0 {
		return fmt.Errorf("%d problem(s) in %s", bad, path)
	}
	fmt.Printf("OK: %s satisfies the contract\n", path)

	// A contract that validates is not the same as a file the program will read.
	// This is the one check that catches sdr.program pointing somewhere the C++'s
	// own hardcoded path does not reach, which would otherwise make every edit
	// above report success and change nothing.
	if err := svc.CheckProgramConfig(); err != nil {
		return err
	}
	if bin, err := sdr.LookProgram(program); err != nil {
		fmt.Printf("  connect: NOT on PATH, and Connect would fall back to the program directory\n")
	} else {
		fmt.Printf("  connect: %s\n", bin)
	}
	fmt.Printf("  the program and this tool read the same file\n")
	return nil
}

func bound(v float64) string {
	if v == 0 {
		return "-"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func show(svc *sdr.Service) error {
	p, err := svc.Params(context.Background())
	if err != nil {
		return err
	}
	fmt.Printf("PRF                 %g Hz\n", p.PRFHz)
	fmt.Printf("FS                  %g Hz\n", p.SampleRateHz)
	fmt.Printf("TX_FREQ             %g Hz\n", p.TxFreqHz)
	fmt.Printf("BW                  %g Hz\n", p.BandwidthHz)
	fmt.Printf("NORMALIZED_GAIN_TX  %g\n", p.NormalizedGainTx)
	fmt.Printf("NORMALIZED_GAIN_RX  %g\n", p.NormalizedGainRx)
	fmt.Printf("SESSION_DURATION    %d s\n", p.SessionDurationS)
	fmt.Printf("TX_ANTENNA          %q\n", p.TxAntenna)
	fmt.Printf("RX_ANTENNA          %q\n", p.RxAntenna)
	return nil
}

// set applies KEY=VALUE pairs.
func set(ctx context.Context, svc *sdr.Service, args []string) error {
	if len(args) == 0 {
		return errors.New("set needs at least one KEY=VALUE")
	}

	var patch domain.SdrParamsPatch
	seen := map[string]bool{}

	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		if !ok {
			return fmt.Errorf("%q is not KEY=VALUE", a)
		}
		k = strings.ToUpper(strings.TrimSpace(k))

		key, known := sdr.Lookup(k)
		if !known {
			// Named rather than "invalid key", and PULSE_DURATION gets its own
			// message because it is the one an operator is most likely to try.
			if k == "PULSE_DURATION" {
				return errors.New("PULSE_DURATION cannot be set: config.hpp line 45 has the " +
					"read commented out, so the program never looks at it")
			}
			return fmt.Errorf("%q is not a parameter this build knows; run `validate` for the list "+
				"(this build knows: %s)", k, knownNames())
		}
		if seen[k] {
			return fmt.Errorf("%s given twice", k)
		}
		seen[k] = true

		if key.Min == 0 && key.Max == 0 {
			return fmt.Errorf("%s is not a settable numeric parameter", k)
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
		// Validated here as well as in the service, so the operator sees the
		// datasheet citation before anything is written.
		if err := sdr.ValidateValue(k, f); err != nil {
			return err
		}

		switch k {
		case "PRF":
			patch.PRFHz = &f
		case "FS":
			patch.SampleRateHz = &f
		case "TX_FREQ":
			patch.TxFreqHz = &f
		case "BW":
			patch.BandwidthHz = &f
		case "NORMALIZED_GAIN_TX":
			patch.NormalizedGainTx = &f
		case "NORMALIZED_GAIN_RX":
			patch.NormalizedGainRx = &f
		case "SESSION_DURATION":
			n := uint32(f)
			if float64(n) != f {
				return fmt.Errorf("SESSION_DURATION must be a whole number of seconds, got %g", f)
			}
			patch.SessionDurationS = &n
		default:
			return fmt.Errorf("%s is bounded but not settable in this build", k)
		}
	}

	if err := svc.SetParams(ctx, patch); err != nil {
		return err
	}
	fmt.Println("written. current values:")
	return show(svc)
}

func knownNames() string {
	var out []string
	for _, k := range sdr.Keys {
		if k.Min != 0 || k.Max != 0 {
			out = append(out, k.Name)
		}
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// discardLogger keeps the output to the command's own report. The sdr package
// logs every parameter update, which is noise in a tool whose whole output is
// the answer to the operator's question.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

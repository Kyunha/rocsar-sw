package qos

import "testing"

// Real `tc filter show` output, captured from the target after adding the filter
// the way this package adds it.
//
// The bug this pins: dst_port was passed without ip_proto, tc rejected it with
// `Illegal "dst_port"`, and the consequence was silent -- shaping reported as
// active while every bulk download joined the priority class and starved it.
func TestFilterInstalledRecognisesRealTcOutput(t *testing.T) {
	const installed = `filter parent 1: protocol ip pref 20 flower chain 0
filter parent 1: protocol ip pref 20 flower chain 0 handle 0x1
  eth_type ipv4
  ip_proto tcp
  dst_port 5557
  not_in_hw
`
	if !filterInstalled(installed, 5557) {
		t.Error("filterInstalled said no for output that clearly contains the filter")
	}
}

// The outputs that mean "bulk traffic is unclassified and shaping says it is
// active". Each of these has happened, or would have, with the bug.
func TestFilterInstalledRejectsTheFailureModes(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
	}{
		{
			// `tc filter add` succeeded and the shaper moved on. Every bulk
			// download now shares the priority class.
			name: "no filters at all",
			out:  "",
		},
		{
			// A flower filter that matches everything except the port: bulk
			// traffic is classified, but not into the bulk class.
			name: "flower without the port",
			out: `filter parent 1: protocol ip pref 20 flower chain 0
  eth_type ipv4
  ip_proto tcp
  not_in_hw
`,
		},
		{
			// The filter is there but on the wrong port, which is what a stale
			// hierarchy from a previous configuration leaves behind.
			name: "wrong port",
			out: `filter parent 1: protocol ip pref 20 flower chain 0
  eth_type ipv4
  ip_proto tcp
  dst_port 6000
  not_in_hw
`,
		},
		{
			// No ip_proto. This is the shape a kernel or iproute2 that accepted
			// the original command would leave: a port match whose transport
			// protocol was never pinned down.
			name: "port without ip_proto",
			out: `filter parent 1: protocol ip pref 20 flower chain 0
  eth_type ipv4
  dst_port 5557
  not_in_hw
`,
		},
		{
			// src_port, not dst_port. A substring check on the number alone
			// would pass this.
			name: "src_port only",
			out: `filter parent 1: protocol ip pref 20 flower chain 0
  eth_type ipv4
  ip_proto tcp
  src_port 5557
  not_in_hw
`,
		},
		{
			// 5557 is a prefix of 55570. Matching on the bare number would pass.
			name: "port is a prefix of another",
			out: `filter parent 1: protocol ip pref 20 flower chain 0
  eth_type ipv4
  ip_proto tcp
  dst_port 55570
  not_in_hw
`,
		},
	} {
		if filterInstalled(tc.out, 5557) {
			t.Errorf("%s: filterInstalled said yes, so bulk traffic would be "+
				"reported as classified when it is not", tc.name)
		}
	}
}

// The command itself.
//
// If ip_proto goes missing, tc answers `Illegal "dst_port"` and the shaper
// degrades silently -- shaping stays on, bulk downloads join the priority class,
// and nothing reports it. Asserted against the argument list the shaper actually
// builds rather than against the source text, which would match the first
// "dst_port" anywhere in the file.
func TestBulkFilterArgsCarryIPProtoBeforeDstPort(t *testing.T) {
	args := bulkFilterArgs("eth0", 5557)

	proto, port := indexOf(args, "ip_proto"), indexOf(args, "dst_port")
	if proto < 0 {
		t.Fatal(`bulkFilterArgs has no "ip_proto"; tc rejects a dst_port without it ` +
			`(Illegal "dst_port") and bulk traffic then starves the priority class silently`)
	}
	if port < 0 {
		t.Fatal(`bulkFilterArgs has no "dst_port"; nothing would be classified`)
	}
	if proto > port {
		t.Errorf("ip_proto is at %d and dst_port at %d: ip_proto must be parsed "+
			"first, since dst_port has no meaning until the transport is known", proto, port)
	}
	if args[proto+1] != "tcp" {
		t.Errorf("ip_proto is %q, want \"tcp\" -- the bulk traffic is HTTP", args[proto+1])
	}
	if args[port+1] != "5557" {
		t.Errorf("dst_port is %q, want \"5557\"", args[port+1])
	}
	if args[0] != "filter" || args[1] != "add" {
		t.Errorf("the list does not start with \"filter add\": %v", args[:2])
	}
	if args[2] != "dev" || args[3] != "eth0" {
		t.Errorf("the filter is not being added to the device asked for: %v", args[2:4])
	}
}

func indexOf(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

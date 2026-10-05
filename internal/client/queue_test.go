package client

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	rocsarv1 "github.com/rocsar/obc/api/rocsar/v1"
)

// The queue serialises: one in flight, one queued, the rest refused. The fake
// holds the first reply hostage while three more submits race for the one
// remaining slot -- exactly one of them queues, the other two are refused, and
// which one queues is the scheduler's business. What is asserted is the
// aggregate, which does not depend on it: two successes in send order, two
// refusals, and the refused requests never reaching the fake.
//
// (An earlier version of this test asserted WHICH submit queued. That is a race
// with the goroutine scheduler wearing a test name, and it failed exactly that
// way.)
func TestQueueSerialisesAndRefusesTheRest(t *testing.T) {
	f := newFakeOBC(t)
	defer f.close()

	release := make(chan struct{})
	f.onCommand = func(req *rocsarv1.CommandRequest) *rocsarv1.CommandResponse {
		<-release
		return &rocsarv1.CommandResponse{
			RequestId: req.GetRequestId(),
			Success:   true,
			Error:     rocsarv1.ErrorCode_ERROR_NONE,
		}
	}

	q := newQueue(f.testConfig(), 5*time.Second)
	defer q.Close()
	ctx := context.Background()

	mkreq := func() *rocsarv1.CommandRequest {
		reqs, err := BuildRequests("query", nil)
		if err != nil {
			t.Fatalf("build query: %v", err)
		}
		return reqs[0]
	}
	type outcome struct {
		id   string
		resp *rocsarv1.CommandResponse
		err  error
	}
	var mu sync.Mutex
	results := map[string]outcome{}
	var refused []string
	run := func(req *rocsarv1.CommandRequest) {
		go func() {
			resp, err := q.Submit(ctx, req)
			mu.Lock()
			defer mu.Unlock()
			results[req.GetRequestId()] = outcome{req.GetRequestId(), resp, err}
			if errors.Is(err, ErrQueueFull) {
				refused = append(refused, req.GetRequestId())
			}
		}()
	}

	first := mkreq()
	run(first)
	// The first request is inside the handler before the rest are sent, so it
	// is first at the fake no matter how the scheduler orders the others.
	eventually(t, 10*time.Second, func() bool { return f.receivedCount() == 1 },
		"first request to reach the fake")

	second, third, fourth := mkreq(), mkreq(), mkreq()
	run(second)
	run(third)
	run(fourth)
	// Exactly two of the three lose the race for the one queued slot, and a
	// refusal touches nothing: it returns before sending. The polling reads
	// mutex-guarded bookkeeping rather than consuming result channels, so a
	// refusal observed on one poll is still observed on the next.
	eventually(t, 10*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(refused) == 2
	}, "two submits to be refused while one queues")

	if st := q.State(); st.CommandsInFlight != 1 {
		t.Errorf("CommandsInFlight = %d, want 1 while a submit is inside", st.CommandsInFlight)
	}
	if n := f.receivedCount(); n != 1 {
		t.Fatalf("a refused or queued request reached the fake: %d received, want 1", n)
	}

	close(release)
	// Everything outstanding completes now: the first request and whichever
	// one of the three queued.
	eventually(t, 10*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(results) == 4
	}, "all four submits to complete after release")

	mu.Lock()
	defer mu.Unlock()
	var succeeded []string
	for id, o := range results {
		if errors.Is(o.err, ErrQueueFull) {
			continue
		}
		if o.err != nil || !o.resp.GetSuccess() {
			t.Errorf("submit %q: resp=%v err=%v, want success", id, o.resp, o.err)
			continue
		}
		succeeded = append(succeeded, id)
	}
	if len(succeeded) != 2 {
		t.Fatalf("%d submits succeeded, want 2 (first + queued): %v", len(succeeded), results)
	}
	if got := q.State().CommandsInFlight; got != 0 {
		t.Errorf("CommandsInFlight = %d after both completed, want 0", got)
	}

	// Send order is preserved: the first request went out first, the queued
	// one second, and the refused ones never.
	f.mu.Lock()
	var ids []string
	for _, r := range f.received {
		ids = append(ids, r.GetRequestId())
	}
	f.mu.Unlock()
	_ = succeeded // order checked below against the fake's log
	if len(ids) != 2 || ids[0] != first.GetRequestId() {
		t.Errorf("fake received %v, want first=%s then the queued one", ids, first.GetRequestId())
	}
	seen := map[string]bool{ids[0]: true, ids[1]: true}
	for id, o := range results {
		if !seen[id] && !errors.Is(o.err, ErrQueueFull) {
			t.Errorf("submit %q neither succeeded at the fake nor was refused: %v", id, o.err)
		}
	}
}

// A submission made while the link is known to be down fails at once instead
// of burning a full exchange timeout -- and, just as importantly, it is never
// queued for when the link returns.
//
// Two halves, because "down" has two shapes. A refused dial fails fast on its
// own (go-zeromq connects synchronously); a blackholed address accepts the TCP
// handshake and then never speaks, so the dial succeeds and only the exchange
// times out. The fail-fast window covers the second shape: the first failure
// costs one exchange timeout, the rest fail at once.
func TestQueueDropsFastWhileTheLinkIsDown(t *testing.T) {
	reqs, err := BuildRequests("query", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// A refused port fails at Dial, synchronously. Port 1 is privileged and
	// never bound here.
	refused := newQueue(Config{
		Control: "tcp://127.0.0.1:1",
	}, 5*time.Second)
	defer refused.Close()
	start := time.Now()
	_, err = refused.Submit(ctx, reqs[0])
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("submit to a refused port returned %v, want ErrNotConnected", err)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Errorf("refused submit took %s; a refused dial should fail fast", el)
	}
	if st := refused.State(); st.ControlConnected || st.LastError == "" {
		t.Errorf("State after a refused dial is not down-with-a-reason: %+v", st)
	}

	// A blackhole accepts TCP and never speaks ZMTP: the dial succeeds and the
	// exchange burns the whole bound.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("blackhole listener: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Held open and never read: the handshake completes and then
			// nothing ever arrives. Closing it would turn the blackhole into
			// a refusal, which is the other test above.
			_ = c
		}
	}()
	blackholed := newQueue(Config{
		Control: "tcp://" + ln.Addr().String(),
	}, 400*time.Millisecond)
	defer blackholed.Close()

	// The first attempt genuinely tries: the dial waits out its handshake bound
	// against the silent endpoint, and the failure carries the sentinel because
	// a failed dial IS a link-down signal -- there is no milder reading of "the
	// far side never completed the handshake".
	start = time.Now()
	_, err = blackholed.Submit(ctx, reqs[0])
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("first submit returned %v, want ErrNotConnected with the dial reason", err)
	}
	if el := time.Since(start); el < 300*time.Millisecond {
		t.Errorf("first submit failed after %s; it should have waited out the handshake bound", el)
	}

	// The second attempt, inside the fail-fast window, returns at once.
	start = time.Now()
	_, err = blackholed.Submit(ctx, reqs[0])
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("submit inside the window returned %v, want ErrNotConnected", err)
	}
	if el := time.Since(start); el > 300*time.Millisecond {
		t.Errorf("fail-fast submit took %s; it should return without touching the socket", el)
	}

	st := blackholed.State()
	if st.ControlConnected {
		t.Error("State reports ControlConnected after two failures")
	}
	if st.LastError == "" {
		t.Error("State carries no reason for the outage")
	}

	// Past the window the next click probes again: it waits out the handshake
	// bound rather than failing fast, which is how a returned link is noticed.
	time.Sleep(1200 * time.Millisecond)
	start = time.Now()
	_, err = blackholed.Submit(ctx, reqs[0])
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("submit past the window returned %v, want ErrNotConnected with a fresh dial reason", err)
	}
	if el := time.Since(start); el < 300*time.Millisecond {
		t.Errorf("submit past the window failed after %s; it should have probed", el)
	}
}

// The happy path through the persistent socket: matched reply, healthy state,
// and the socket reused rather than redialled.
func TestQueueSubmitSucceedsAgainstTheFake(t *testing.T) {
	f := newFakeOBC(t)
	defer f.close()

	q := newQueue(f.testConfig(), 5*time.Second)
	defer q.Close()

	reqs, err := BuildRequests("heading", []string{"275.5"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := q.Submit(context.Background(), reqs[0])
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !resp.GetSuccess() || resp.GetRequestId() != reqs[0].GetRequestId() {
		t.Errorf("reply does not match the request: %+v", resp)
	}
	st := q.State()
	if !st.ControlConnected || st.CommandsInFlight != 0 || st.LastError != "" {
		t.Errorf("State after success is not clean: %+v", st)
	}
}

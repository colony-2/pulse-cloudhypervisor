package cloudhypervisor

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/colony-2/pulse-provider-api"
)

func waitSub(t *testing.T, s api.LogSubscription) api.ListenerResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(bg, 3*time.Second)
	defer cancel()
	select {
	case <-s.Done():
	case <-ctx.Done():
		t.Fatal("subscription did not end")
	}
	r, _ := s.Wait(ctx)
	return r
}
func options(fn func(context.Context, api.LaunchLog) error) *api.ListenOptions {
	return &api.ListenOptions{Context: bg, OnLog: fn}
}
func TestListenerHandleAndInvalidOptions(t *testing.T) {
	for _, o := range []*api.ListenOptions{nil, {}, {Context: bg}, {Context: bg, OnLog: func(context.Context, api.LaunchLog) error { return nil }, Duration: -1}} {
		p, f := testRuntime(t)
		rs, s, e := p.Submit(bg, []api.Launch{launch("listen")}, api.SubmitOptions{Listener: o})
		if e != nil || rs[0].Status != api.Accepted || f.count() != 1 {
			t.Fatal("listener altered admission")
		}
		if o == nil {
			if s != nil {
				t.Fatal("nil listener got handle")
			}
		} else {
			if s == nil || waitSub(t, s).Reason != api.ListenerProtocolError {
				t.Fatal("invalid listener disappeared")
			}
		}
	}
	p, _ := testRuntime(t)
	l := launch("no-start")
	l.Capabilities = []string{"missing"}
	_, s, e := p.Submit(bg, []api.Launch{l}, api.SubmitOptions{Listener: options(func(context.Context, api.LaunchLog) error { return nil })})
	if e != nil || s == nil {
		t.Fatal(e)
	}
	r := waitSub(t, s)
	if r.Reason != api.ListenerSourceUnavailable {
		t.Fatal(r.Reason)
	}
	for _, c := range r.Coverage {
		if c.State != api.SourceUnavailable || c.Closed {
			t.Fatal("no-start coverage")
		}
	}
}
func TestBinaryRecordsSequenceAndCoverage(t *testing.T) {
	events := make(chan api.LaunchLog, 8)
	s := newSubscription([]api.Launch{launch("binary")}, options(func(_ context.Context, l api.LaunchLog) error { events <- l; return nil }))
	defer s.Stop()
	data := bytes.Repeat([]byte{0, 255, 10, 13}, 2300)
	s.emit("binary", api.LogSourceStdout, data)
	s.emit("binary", api.LogSourceStderr, []byte{0, 1, 2})
	got := []byte{}
	for i := uint64(1); i <= 4; i++ {
		select {
		case e := <-events:
			if e.Sequence != i || len(e.Data) > 4096 || e.Timestamp.IsZero() || e.SubscriptionID == "" {
				t.Fatal("invalid record")
			}
			if i < 4 {
				if e.Source != api.LogSourceStdout {
					t.Fatal("source changed")
				}
				got = append(got, e.Data...)
			} else if e.Source != api.LogSourceStderr {
				t.Fatal("stderr merged")
			}
		case <-time.After(time.Second):
			t.Fatal("missing event")
		}
	}
	if !bytes.Equal(data, got) {
		t.Fatal("binary bytes changed")
	}
	s.Stop()
	r := waitSub(t, s)
	if r.Coverage[1].LastSequence != 3 || r.Coverage[2].LastSequence != 4 {
		t.Fatal("dispatch sequence coverage")
	}
	r.Coverage[0].LaunchID = "mutated"
	if s.Coverage()[0].LaunchID == "mutated" {
		t.Fatal("coverage aliases returned result")
	}
}
func TestListenerIndependentContextsAndStableWait(t *testing.T) {
	p, _ := testRuntime(t)
	admit, cancel := context.WithCancel(bg)
	_, s, e := p.Submit(admit, []api.Launch{launch("independent")}, api.SubmitOptions{Listener: options(func(context.Context, api.LaunchLog) error { return nil })})
	if e != nil {
		t.Fatal(e)
	}
	cancel()
	wait, cw := context.WithCancel(bg)
	cw()
	if _, e = s.Wait(wait); !errors.Is(e, context.Canceled) {
		t.Fatal("wait affected lifetime")
	}
	select {
	case <-s.Done():
		t.Fatal("admission canceled listener")
	default:
	}
	s.Stop()
	first := waitSub(t, s)
	second, _ := s.Wait(bg)
	if first.Reason != api.ListenerStopped || second.Reason != first.Reason || len(p.records) != 1 {
		t.Fatal("unstable stop")
	}
	for _, kind := range []string{"context", "duration", "deadline"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cc := context.WithCancel(bg)
			defer cc()
			o := options(func(context.Context, api.LaunchLog) error { return nil })
			o.Context = ctx
			if kind == "duration" {
				o.Duration = time.Millisecond
			}
			if kind == "deadline" {
				var dc context.CancelFunc
				o.Context, dc = context.WithDeadline(bg, time.Now().Add(-time.Second))
				defer dc()
			}
			sub := newSubscription([]api.Launch{launch("end")}, o)
			if kind == "context" {
				cc()
			}
			r := waitSub(t, sub)
			want := api.ListenerContextCanceled
			if kind == "duration" {
				want = api.ListenerDurationElapsed
			}
			if kind == "deadline" {
				want = api.ListenerContextDeadline
			}
			if r.Reason != want {
				t.Fatal(r.Reason)
			}
		})
	}
}
func TestCallbacksSerialErrorsAndPanics(t *testing.T) {
	for _, kind := range []string{"stop", "error", "panic"} {
		t.Run(kind, func(t *testing.T) {
			var active atomic.Int32
			s := newSubscription([]api.Launch{launch("callback")}, options(func(context.Context, api.LaunchLog) error {
				if active.Add(1) != 1 {
					t.Error("concurrent callback")
				}
				defer active.Add(-1)
				switch kind {
				case "stop":
					return api.ErrStopListening
				case "error":
					return errors.New(syntheticLease)
				default:
					panic(syntheticLease)
				}
			}))
			s.emit("callback", api.LogSourceStdout, []byte("x"))
			s.emit("callback", api.LogSourceStderr, []byte("y"))
			r := waitSub(t, s)
			if kind == "stop" {
				if r.Reason != api.ListenerStopped {
					t.Fatal(r.Reason)
				}
			} else if r.Reason != api.ListenerError || r.Cause != api.ErrLogDelivery {
				t.Fatal("unsafe callback failure")
			}
		})
	}
}
func TestOverflowAndBlockedCallbackBudgetAcrossReopen(t *testing.T) {
	waitCallbacks(t)
	// Fill all process-wide slots with callbacks controlled by an explicit barrier.
	entered := make(chan struct{}, cap(callbackSlots))
	release := make(chan struct{})
	subs := []*subscription{}
	for i := 0; i < cap(callbackSlots); i++ {
		s := newSubscription([]api.Launch{launch("blocked")}, options(func(context.Context, api.LaunchLog) error { entered <- struct{}{}; <-release; return nil }))
		subs = append(subs, s)
		s.emit("blocked", api.LogSourceStdout, []byte("x"))
		<-entered
		s.Stop()
	}
	defer func() { close(release); waitCallbacks(t) }()
	for _, s := range subs {
		if waitSub(t, s).Reason != api.ListenerStopped {
			t.Fatal("stop waited for callback")
		}
	}
	p, _ := testRuntime(t)
	p.Close()
	q, f := testRuntime(t)
	rs, s, e := q.Submit(bg, []api.Launch{launch("new")}, api.SubmitOptions{Listener: options(func(context.Context, api.LaunchLog) error { t.Error("callback ceiling bypassed"); return nil })})
	if e != nil || rs[0].Status != api.Accepted || f.count() != 1 {
		t.Fatal("logging saturation affected admission")
	}
	s.(*subscription).emit("new", api.LogSourceStdout, []byte("x"))
	if waitSub(t, s).Reason != api.ListenerCapacityExceeded {
		t.Fatal("reopening reset callback budget")
	}
	// No consumer reads this synthetic bounded queue.
	overflow := &subscription{done: make(chan struct{}), queue: make(chan api.LaunchLog, 1), sequences: map[string]uint64{}}
	overflow.emit("x", api.LogSourceStdout, []byte("a"))
	overflow.emit("x", api.LogSourceStdout, []byte("b"))
	if waitSub(t, overflow).Reason != api.ListenerOverflow || len(overflow.queue) != 0 {
		t.Fatal("overflow failed to detach")
	}
}
func TestSlowCallbackDoneDoesNotWait(t *testing.T) {
	// The previous budget test releases its callbacks before this test starts.
	deadline := time.After(2 * time.Second)
	for len(callbackSlots) != 0 {
		select {
		case <-deadline:
			t.Fatal("callback slots not released")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	entered, release := make(chan struct{}), make(chan struct{})
	s := newSubscription([]api.Launch{launch("slow")}, options(func(context.Context, api.LaunchLog) error { close(entered); <-release; return nil }))
	defer func() { close(release); waitCallbacks(t) }()
	s.emit("slow", api.LogSourceStdout, []byte("x"))
	<-entered
	if r := waitSub(t, s); r.Reason != api.ListenerSlow {
		t.Fatal(r.Reason)
	}
	if len(callbackSlots) != 1 {
		t.Fatal("abandoned callback slot released prematurely")
	}
}
func TestProviderCloseEndsListeningOnly(t *testing.T) {
	p, f := testRuntime(t)
	_, s, e := p.Submit(bg, []api.Launch{launch("close")}, api.SubmitOptions{Listener: options(func(context.Context, api.LaunchLog) error { return nil })})
	if e != nil {
		t.Fatal(e)
	}
	p.Close()
	if waitSub(t, s).Reason != api.ListenerProviderClosed || f.count() != 1 || len(p.records) != 1 {
		t.Fatal("provider close affected execution")
	}
}

func waitCallbacks(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(callbackSlots) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("callback slots did not return")
		}
		runtime.Gosched()
	}
}

func TestCoverageDistinguishesUnavailableFromKnownNativeClosure(t *testing.T) {
	s := newSubscription([]api.Launch{launch("never-available")}, options(func(context.Context, api.LaunchLog) error { return nil }))
	s.source("never-available", api.LogSourceLaunch, api.SourceActive)
	s.source("never-available", api.LogSourceStdout, api.SourceUnavailable)
	for _, src := range []api.LogSource{api.LogSourceLaunch, api.LogSourceStdout, api.LogSourceStderr} {
		s.source("never-available", src, api.SourceClosed)
	}
	r := waitSub(t, s)
	if r.Reason != api.ListenerSourceUnavailable {
		t.Fatal("stream absence reported as successful observation")
	}
	for _, c := range r.Coverage {
		if !c.Closed {
			t.Fatal("known native closure missing")
		}
		if c.Source != api.LogSourceLaunch && c.State != api.SourceUnavailable {
			t.Fatal("unobserved source changed to successful closure")
		}
	}
}

func TestClosingSourcesDeliversFinalCallbackResult(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	for _, stop := range []bool{false, true} {
		var delivered atomic.Bool
		sub := newSubscription([]api.Launch{launch("final")}, options(func(_ context.Context, event api.LaunchLog) error {
			delivered.Store(string(event.Data) == "final record")
			if stop {
				return api.ErrStopListening
			}
			return nil
		}))
		// Let the receiver wait on the channel. A send then hands off the
		// record directly, so the queue is empty before the callback runs.
		runtime.Gosched()
		for _, source := range []api.LogSource{api.LogSourceLaunch, api.LogSourceStdout, api.LogSourceStderr} {
			sub.source("final", source, api.SourceActive)
		}
		sub.emit("final", api.LogSourceLaunch, []byte("final record"))
		for _, source := range []api.LogSource{api.LogSourceLaunch, api.LogSourceStdout, api.LogSourceStderr} {
			sub.source("final", source, api.SourceClosed)
		}
		reason := api.ListenerSourcesClosed
		if stop {
			reason = api.ListenerStopped
		}
		if got := waitSub(t, sub); got.Reason != reason || !delivered.Load() {
			t.Fatalf("final callback delivered=%t, got %v, want %v", delivered.Load(), got.Reason, reason)
		}
	}
}

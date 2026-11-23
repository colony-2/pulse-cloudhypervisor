package cloudhypervisor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	api "github.com/colony-2/pulse-provider-api"
)

// Slots are deliberately process-wide. A callback that ignores cancellation
// retains its slot even after provider closure or another New call.
var callbackSlots = make(chan struct{}, 32)
var subscriptionIDs atomic.Uint64

const maxSubscriptions = 32
const queueRecords = 64

var callbackBudget = time.Second

type subscription struct {
	mu        sync.Mutex
	done      chan struct{}
	queue     chan api.LaunchLog
	context   context.Context
	cancel    context.CancelFunc
	callback  func(context.Context, api.LaunchLog) error
	id        string
	sources   []api.SourceStatus
	sequences map[string]uint64
	result    api.ListenerResult
	ended     bool
	inFlight  bool
}

func newSubscription(ls []api.Launch, o *api.ListenOptions) *subscription {
	if o == nil {
		return nil
	}
	s := &subscription{done: make(chan struct{}), queue: make(chan api.LaunchLog, queueRecords), id: fmt.Sprint(subscriptionIDs.Add(1)), sequences: map[string]uint64{}}
	for _, l := range ls {
		for _, src := range []api.LogSource{api.LogSourceLaunch, api.LogSourceStdout, api.LogSourceStderr} {
			s.sources = append(s.sources, api.SourceStatus{LaunchID: l.LaunchID, Source: src, State: api.SourcePending})
		}
	}
	if o.Context == nil || o.OnLog == nil || o.Duration < 0 {
		s.finish(api.ListenerProtocolError, api.ErrLogProtocol)
		return s
	}
	s.context, s.cancel = context.WithCancel(o.Context)
	s.callback = o.OnLog
	if o.Context.Err() != nil {
		s.endContext()
		return s
	}
	go s.run(o.Duration)
	return s
}
func (s *subscription) endContext() {
	reason := api.ListenerContextCanceled
	if s.context != nil && errors.Is(s.context.Err(), context.DeadlineExceeded) {
		reason = api.ListenerContextDeadline
	}
	s.finish(reason, context.Canceled)
}
func (s *subscription) finish(reason api.ListenerReason, cause error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	s.ended = true
	for i := range s.sources {
		v := &s.sources[i]
		if v.State == api.SourceActive || v.State == api.SourcePending {
			v.State = api.SourceInterrupted
			v.Cause = cause
		}
	}
	s.result = api.ListenerResult{Reason: reason, Cause: cause}
	if s.cancel != nil {
		s.cancel()
	}
	close(s.done)
	// queued bytes are bounded and promptly released, even if a callback blocks.
	for {
		select {
		case <-s.queue:
		default:
			return
		}
	}
}
func (s *subscription) run(duration time.Duration) {
	var timer *time.Timer
	var expiry <-chan time.Time
	if duration > 0 {
		timer = time.NewTimer(duration)
		expiry = timer.C
		defer timer.Stop()
	}
	for {
		select {
		case <-s.done:
			return
		case <-s.context.Done():
			s.endContext()
			return
		case <-expiry:
			s.finish(api.ListenerDurationElapsed, nil)
			return
		case event := <-s.queue:
			select {
			case callbackSlots <- struct{}{}:
			default:
				s.finish(api.ListenerCapacityExceeded, api.ErrLogCapacity)
				return
			}
			s.mu.Lock()
			if s.ended {
				s.mu.Unlock()
				<-callbackSlots
				return
			}
			for i := range s.sources {
				v := &s.sources[i]
				if v.LaunchID == event.LaunchID && v.Source == event.Source {
					v.LastSequence = event.Sequence
				}
			}
			s.inFlight = true
			s.mu.Unlock()
			result := make(chan error, 1)
			go func() {
				defer func() {
					<-callbackSlots
					if recover() != nil {
						result <- api.ErrLogDelivery
					}
				}()
				result <- s.callback(s.context, event)
			}()
			deadline := time.NewTimer(callbackBudget)
			select {
			case <-s.done:
				deadline.Stop()
				return
			case <-s.context.Done():
				deadline.Stop()
				s.endContext()
				return
			case <-expiry:
				deadline.Stop()
				s.finish(api.ListenerDurationElapsed, nil)
				return
			case <-deadline.C:
				s.finish(api.ListenerSlow, api.ErrLogDelivery)
				return
			case e := <-result:
				deadline.Stop()
				s.mu.Lock()
				s.inFlight = false
				s.mu.Unlock()
				if e != nil {
					if errors.Is(e, api.ErrStopListening) {
						s.finish(api.ListenerStopped, nil)
					} else {
						s.finish(api.ListenerError, api.ErrLogDelivery)
					}
					return
				}
			}
			s.maybeClosed()
		}
	}
}
func (s *subscription) maybeClosed() {
	s.mu.Lock()
	closed := len(s.queue) == 0 && !s.inFlight
	unavailable := false
	for _, v := range s.sources {
		if v.State == api.SourceUnavailable {
			unavailable = true
		}
		if v.State == api.SourcePending || v.State == api.SourceActive {
			closed = false
		}
	}
	s.mu.Unlock()
	if closed {
		if unavailable {
			s.finish(api.ListenerSourceUnavailable, api.ErrLogSource)
		} else {
			s.finish(api.ListenerSourcesClosed, nil)
		}
	}
}
func (s *subscription) emit(id string, source api.LogSource, data []byte) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	for len(data) > 0 {
		n := len(data)
		if n > api.MaxLogRecordBytes {
			n = api.MaxLogRecordBytes
		}
		s.sequences[id]++
		event := api.LaunchLog{LaunchID: id, SubscriptionID: s.id, Source: source, Sequence: s.sequences[id], Timestamp: time.Now().UTC(), Data: append([]byte(nil), data[:n]...)}
		select {
		case s.queue <- event:
		default:
			// finish outside the mutex.
			s.mu.Unlock()
			s.finish(api.ListenerOverflow, api.ErrLogDelivery)
			s.mu.Lock()
			return
		}
		data = data[n:]
	}
}
func (s *subscription) source(id string, src api.LogSource, status api.SourceState) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if !s.ended {
		for i := range s.sources {
			v := &s.sources[i]
			if v.LaunchID == id && v.Source == src {
				if status == api.SourceClosed && (v.State == api.SourceUnavailable || v.State == api.SourcePending) {
					v.State = api.SourceUnavailable
				} else {
					v.State = status
				}
				v.Closed = status == api.SourceClosed
				if status == api.SourceUnavailable {
					v.Cause = api.ErrLogSource
				}
			}
		}
	}
	s.mu.Unlock()
	s.maybeClosed()
}
func (s *subscription) unavailable(id string) {
	if s == nil {
		return
	}
	for _, src := range []api.LogSource{api.LogSourceLaunch, api.LogSourceStdout, api.LogSourceStderr} {
		s.source(id, src, api.SourceUnavailable)
	}
}
func (s *subscription) Stop()                 { s.finish(api.ListenerStopped, nil) }
func (s *subscription) Done() <-chan struct{} { return s.done }
func (s *subscription) Coverage() []api.SourceStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]api.SourceStatus{}, s.sources...)
}
func (s *subscription) Wait(ctx context.Context) (api.ListenerResult, error) {
	if ctx == nil {
		return api.ListenerResult{}, api.ErrLogProtocol
	}
	select {
	case <-s.done:
	case <-ctx.Done():
		return api.ListenerResult{}, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.result
	r.Coverage = append([]api.SourceStatus{}, s.sources...)
	return r, r.Cause
}

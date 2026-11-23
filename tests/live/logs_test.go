//go:build live

package live

import (
	"context"
	"sync"
	"testing"

	api "github.com/colony-2/pulse-provider-api"
)

func TestNativeListenerLoss(t *testing.T) {
	for _, mode := range []string{"cancel", "stall"} {
		t.Run(mode, func(t *testing.T) {
			e := openExecution(t)
			l, _ := e.launch(t, "logs-"+mode, "valid", "output")
			ctx, cancel := context.WithCancel(e.ctx)
			defer cancel()
			received := make(chan struct{})
			release := make(chan struct{})
			defer close(release)
			var first sync.Once
			sub := e.submit(t, l, api.SubmitOptions{Listener: &api.ListenOptions{Context: ctx,
				OnLog: func(_ context.Context, event api.LaunchLog) error {
					if event.LaunchID == l.LaunchID && event.Sequence > 0 && len(event.Data) > 0 {
						first.Do(func() { close(received) })
						<-release
					}
					return nil
				},
			}})
			select {
			case <-received:
			case <-e.ctx.Done():
				t.Fatal("native listener received no correlated output")
			}
			expected := api.ListenerSlow
			if mode == "cancel" {
				expected = api.ListenerContextCanceled
				cancel()
			}
			result, err := sub.Wait(e.ctx)
			if err != nil || result.Reason != expected {
				t.Fatal("listener loss did not terminate with its own reason")
			}
			e.waitForCleanup(t, l)
			status, err := e.observe(e.ctx, l.LaunchID)
			if err != nil || status.Job != l.Job || status.Acquisitions != 1 || status.ClaimAttempts != 0 ||
				!status.OutputOK || status.Outcome != "success" {
				t.Fatal("job lost or truncated stdout or stderr after listener loss")
			}
		})
	}
}

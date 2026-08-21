package rtc

import (
	"context"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// delayKeeper pushes a delayed event's timer into the future for as long as the
// bot is alive. Stop refreshing and the homeserver publishes the event, which
// is how the bot removes itself from a call it died in the middle of.
//
// Both membership stacks arm one of these, so it lives here rather than in
// either of them.
type delayKeeper struct {
	stop chan struct{}
	done chan struct{}
}

// startDelayKeeper begins restarting delayID every interval.
func startDelayKeeper(client *mautrix.Client, delayID id.DelayID, interval time.Duration) *delayKeeper {
	k := &delayKeeper{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(k.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-k.stop:
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), interval)
				_, err := client.UpdateDelayedEvent(ctx, &mautrix.ReqUpdateDelayedEvent{
					DelayID: delayID,
					Action:  event.DelayActionRestart,
				})
				cancel()
				if err != nil {
					client.Log.Warn().Err(err).Msg("failed to restart delayed leave event")
				}
			}
		}
	}()
	return k
}

// Stop ends the refresh loop and waits for it to finish, so callers know no
// further restarts will race with the leave they are about to send.
func (k *delayKeeper) Stop() {
	if k == nil {
		return
	}
	close(k.stop)
	<-k.done
}

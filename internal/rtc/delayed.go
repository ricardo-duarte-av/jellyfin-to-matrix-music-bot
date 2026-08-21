package rtc

import (
	"context"
	"sync"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// delayLost is called when the delayed event a keeper was refreshing has gone.
// It must put the membership back the way it should be — which usually means
// re-publishing it, since a delayed leave that has gone has almost always gone
// by being sent — and arm a fresh delayed leave, returning its ID.
type delayLost func(ctx context.Context) (id.DelayID, error)

// delayKeeper pushes a delayed event's timer into the future for as long as the
// bot is alive. Stop refreshing and the homeserver publishes the event, which
// is how the bot removes itself from a call it died in the middle of.
//
// Both membership stacks arm one of these, so it lives here rather than in
// either of them.
type delayKeeper struct {
	name   string
	client *mautrix.Client
	onLost delayLost

	stop chan struct{}
	done chan struct{}

	mu      sync.Mutex
	delayID id.DelayID
}

// startDelayKeeper begins restarting delayID every interval.
//
// name distinguishes the two membership stacks in the log: they arm one of
// these each, and a message that could have come from either is no use when one
// of them is misbehaving.
func startDelayKeeper(client *mautrix.Client, name string, delayID id.DelayID, interval time.Duration, onLost delayLost) *delayKeeper {
	k := &delayKeeper{
		name:    name,
		client:  client,
		onLost:  onLost,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		delayID: delayID,
	}
	go k.run(interval)
	return k
}

// DelayID is the delayed event currently being kept alive. It changes whenever
// the keeper has had to arm a replacement.
func (k *delayKeeper) DelayID() id.DelayID {
	if k == nil {
		return ""
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.delayID
}

func (k *delayKeeper) setDelayID(delayID id.DelayID) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.delayID = delayID
}

func (k *delayKeeper) run(interval time.Duration) {
	defer close(k.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-k.stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), interval)
			k.tick(ctx, interval)
			cancel()
		}
	}
}

// tick refreshes the delayed event, replacing it if it has gone.
func (k *delayKeeper) tick(ctx context.Context, interval time.Duration) {
	if delayID := k.DelayID(); delayID != "" {
		_, err := k.client.UpdateDelayedEvent(ctx, &mautrix.ReqUpdateDelayedEvent{
			DelayID: delayID,
			Action:  event.DelayActionRestart,
		})
		if err == nil {
			return
		}
		if !isNotFound(err) {
			// Transient: the next tick gets another go, and there is enough
			// slack in the timeout to absorb a few of these.
			k.client.Log.Warn().Err(err).Str("leg", k.name).Msg("failed to restart delayed leave event")
			return
		}
		// Terminal. The homeserver has forgotten this delay, which almost
		// always means it already published our leave — so the bot is now out
		// of a call it believes it is in, and retrying this ID would only log
		// the same 404 forever.
		k.client.Log.Warn().Str("leg", k.name).
			Msg("delayed leave event is gone; the homeserver most likely published it — rejoining and re-arming")
		k.setDelayID("")
	}

	delayID, err := k.onLost(ctx)
	if err != nil {
		k.client.Log.Err(err).Str("leg", k.name).Msg("failed to re-arm the delayed leave event")
		return
	}
	k.setDelayID(delayID)
	k.client.Log.Info().Str("leg", k.name).Msg("re-armed the delayed leave event")
}

// Stop ends the refresh loop and waits for it to finish, so callers know no
// further restarts — or re-arms — will race with the leave they are about to
// send.
//
// Callers must not hold a lock that onLost needs.
func (k *delayKeeper) Stop() {
	if k == nil {
		return
	}
	close(k.stop)
	<-k.done
}

package matrix

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/daedric/jellyfin-to-matrix-music-bot/internal/config"
	"github.com/daedric/jellyfin-to-matrix-music-bot/internal/rtc"
)

func stickyMember(t *testing.T, sender id.UserID, eventID, stickyKey, membership string, ts int64, duration time.Duration) *event.Event {
	t.Helper()
	content := rtc.StickyMemberContent{
		SlotID:    rtc.DefaultSlotID,
		Member:    rtc.StickyMemberInfo{ID: stickyKey, Membership: membership},
		StickyKey: stickyKey,
	}
	raw, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	evt := &event.Event{
		ID:        id.EventID(eventID),
		Sender:    sender,
		Type:      rtc.StickyMemberEventType,
		Timestamp: ts,
		Sticky:    &event.Sticky{},
	}
	evt.Sticky.Duration.Duration = duration
	evt.Content.VeryRaw = raw
	return evt
}

const bob = id.UserID("@bob:example.org")

func primedWatcher() *callWatcher {
	w := newCallWatcher()
	w.prime()
	w.primeSticky()
	return w
}

func TestStickyJoinAndLeave(t *testing.T) {
	w := primedWatcher()
	now := time.Now()

	join := stickyMember(t, bob, "$1", "MEMBER", "join", now.UnixMilli(), 10*time.Minute)
	if got := w.applySticky(join, true, now.Add(10*time.Minute), "MEMBER", now); got != callJoined {
		t.Fatalf("applySticky(join) = %v; want callJoined", got)
	}

	leave := stickyMember(t, bob, "$2", "MEMBER", "leave", now.Add(time.Second).UnixMilli(), 10*time.Minute)
	if got := w.applySticky(leave, false, now.Add(11*time.Minute), "MEMBER", now); got != callLeft {
		t.Fatalf("applySticky(leave) = %v; want callLeft", got)
	}
}

// The same event arrives in both the timeline and the sticky section of /sync,
// and either may land first. Announcing twice would be noise.
func TestStickyDuplicateDeliveryIsSilent(t *testing.T) {
	w := primedWatcher()
	now := time.Now()
	join := stickyMember(t, bob, "$1", "MEMBER", "join", now.UnixMilli(), 10*time.Minute)

	if got := w.applySticky(join, true, now.Add(10*time.Minute), "MEMBER", now); got != callJoined {
		t.Fatalf("first delivery = %v; want callJoined", got)
	}
	if got := w.applySticky(join, true, now.Add(10*time.Minute), "MEMBER", now); got != callNoChange {
		t.Errorf("second delivery = %v; want callNoChange", got)
	}
}

// MSC4354 tie-break: for one sticky key the event that expires last wins, so a
// stale refresh arriving late must not undo a leave.
func TestStickyTieBreakKeepsTheLastToExpire(t *testing.T) {
	w := primedWatcher()
	now := time.Now()

	// A leave with a long stickiness, then an older join that expires sooner.
	leave := stickyMember(t, bob, "$2", "MEMBER", "leave", now.UnixMilli(), 30*time.Minute)
	w.applySticky(leave, false, now.Add(30*time.Minute), "MEMBER", now)

	stale := stickyMember(t, bob, "$1", "MEMBER", "join", now.UnixMilli(), 10*time.Minute)
	if got := w.applySticky(stale, true, now.Add(10*time.Minute), "MEMBER", now); got != callNoChange {
		t.Errorf("stale join = %v; want it ignored", got)
	}
	if w.stillPresent(bob) {
		t.Error("a stale join resurrected a member who had left")
	}
}

func TestStickyTieBreakOnEventID(t *testing.T) {
	w := primedWatcher()
	now := time.Now()
	ts := now.UnixMilli()

	high := stickyMember(t, bob, "$zzz", "MEMBER", "join", ts, 10*time.Minute)
	w.applySticky(high, true, now.Add(10*time.Minute), "MEMBER", now)

	// Same score, lower event ID: the higher ID stands.
	low := stickyMember(t, bob, "$aaa", "MEMBER", "leave", ts, 10*time.Minute)
	if got := w.applySticky(low, false, now.Add(10*time.Minute), "MEMBER", now); got != callNoChange {
		t.Errorf("lower event ID = %v; want it ignored", got)
	}
	if !w.stillPresent(bob) {
		t.Error("a lower event ID displaced the winning membership")
	}
}

// A member who crashes sends nothing at all: expiry is the only leave signal.
func TestStickyExpirySweepAnnouncesTheLeave(t *testing.T) {
	w := primedWatcher()
	now := time.Now()
	join := stickyMember(t, bob, "$1", "MEMBER", "join", now.UnixMilli(), time.Minute)
	w.applySticky(join, true, now.Add(time.Minute), "MEMBER", now)

	if left := w.expireSticky(now.Add(30 * time.Second)); len(left) != 0 {
		t.Fatalf("expireSticky() = %v before expiry; want none", left)
	}
	left := w.expireSticky(now.Add(2 * time.Minute))
	if len(left) != 1 || left[0] != bob {
		t.Fatalf("expireSticky() = %v; want [%s]", left, bob)
	}
	if w.stillPresent(bob) {
		t.Error("stillPresent() = true after the membership lapsed")
	}
}

// Until the first sync has been recorded, everything already in flight is
// existing state rather than an arrival.
func TestStickyMembershipsBeforePrimingAreSilent(t *testing.T) {
	w := newCallWatcher()
	w.prime()
	now := time.Now()

	join := stickyMember(t, bob, "$1", "MEMBER", "join", now.UnixMilli(), 10*time.Minute)
	if got := w.applySticky(join, true, now.Add(10*time.Minute), "MEMBER", now); got != callNoChange {
		t.Errorf("unprimed join = %v; want callNoChange", got)
	}
	if !w.stillPresent(bob) {
		t.Error("an unprimed join was not recorded")
	}
}

// The bot itself publishes both dialects at once, and so will every client
// during the transition. Someone visible twice is still one person.
func TestPresenceIsAnnouncedOncePerPerson(t *testing.T) {
	w := primedWatcher()
	now := time.Now()

	if got := w.applyLegacy(bob, "_@bob:example.org_DEVICE", true); got != callJoined {
		t.Fatalf("legacy join = %v; want callJoined", got)
	}
	join := stickyMember(t, bob, "$1", "MEMBER", "join", now.UnixMilli(), 10*time.Minute)
	if got := w.applySticky(join, true, now.Add(10*time.Minute), "MEMBER", now); got != callNoChange {
		t.Errorf("sticky join by an already-present member = %v; want callNoChange", got)
	}

	// Dropping one dialect is not leaving while the other still says otherwise.
	if got := w.applyLegacy(bob, "_@bob:example.org_DEVICE", false); got != callNoChange {
		t.Errorf("legacy leave with a live sticky membership = %v; want callNoChange", got)
	}
	leave := stickyMember(t, bob, "$2", "MEMBER", "leave", now.Add(time.Second).UnixMilli(), 10*time.Minute)
	if got := w.applySticky(leave, false, now.Add(11*time.Minute), "MEMBER", now); got != callLeft {
		t.Errorf("final leave = %v; want callLeft", got)
	}
}

// Ejecting a sticky membership means redacting its event, because the ephemeral
// map is keyed by sender: a leave published by the bot would be its own entry.
func TestStickyEventsOfListsRedactionTargets(t *testing.T) {
	w := primedWatcher()
	now := time.Now()

	w.applySticky(stickyMember(t, bob, "$laptop", "LAPTOP", "join", now.UnixMilli(), 10*time.Minute),
		true, now.Add(10*time.Minute), "LAPTOP", now)
	w.applySticky(stickyMember(t, bob, "$phone", "PHONE", "join", now.UnixMilli(), 10*time.Minute),
		true, now.Add(10*time.Minute), "PHONE", now)
	w.applySticky(stickyMember(t, "@alice:example.org", "$alice", "A", "join", now.UnixMilli(), 10*time.Minute),
		true, now.Add(10*time.Minute), "A", now)

	got := w.stickyEventsOf(bob, now)
	want := []id.EventID{"$laptop", "$phone"}
	if len(got) != len(want) {
		t.Fatalf("stickyEventsOf() = %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("stickyEventsOf() = %v; want %v", got, want)
		}
	}

	w.forgetSticky(bob)
	if w.stillPresent(bob) {
		t.Error("forgetSticky() left the ejected member in the call")
	}
	if !w.stillPresent("@alice:example.org") {
		t.Error("forgetSticky() removed somebody else's membership")
	}
}

// The bot is in the call by definition, and through both dialects at once, so
// its own memberships must never count as an audience.
func TestAnyoneElseIgnoresTheBotItself(t *testing.T) {
	const self = id.UserID("@bot:example.org")
	now := time.Now()

	w := primedWatcher()
	if w.anyoneElse(self, now) {
		t.Error("anyoneElse() = true on an empty call")
	}

	// Both of the bot's own memberships, in the two state key formats and the
	// sticky dialect.
	w.applyLegacy(self, "_@bot:example.org_DEVICE", true)
	w.applySticky(stickyMember(t, self, "$self", "SELF", "join", now.UnixMilli(), 10*time.Minute),
		true, now.Add(10*time.Minute), "SELF", now)
	if w.anyoneElse(self, now) {
		t.Error("anyoneElse() = true with only the bot's own memberships")
	}

	w.applyLegacy(bob, "_@bob:example.org_PHONE", true)
	if !w.anyoneElse(self, now) {
		t.Error("anyoneElse() = false with a listener in the call")
	}

	w.applyLegacy(bob, "_@bob:example.org_PHONE", false)
	if w.anyoneElse(self, now) {
		t.Error("anyoneElse() = true after the only listener left")
	}
}

// A sticky membership that has lapsed is not an audience: a client that crashed
// stops refreshing and never sends a leave.
func TestAnyoneElseIgnoresExpiredStickyMemberships(t *testing.T) {
	const self = id.UserID("@bot:example.org")
	now := time.Now()
	w := primedWatcher()

	w.applySticky(stickyMember(t, bob, "$1", "MEMBER", "join", now.UnixMilli(), time.Minute),
		true, now.Add(time.Minute), "MEMBER", now)
	if !w.anyoneElse(self, now) {
		t.Fatal("anyoneElse() = false with a live sticky membership")
	}
	if w.anyoneElse(self, now.Add(2*time.Minute)) {
		t.Error("anyoneElse() = true after the sticky membership lapsed")
	}
}

// fakePlayback is the player as the empty-call rules see it.
type fakePlayback struct {
	playing bool
	pauses  int
	resumes int
}

func (f *fakePlayback) Pause() bool {
	if !f.playing {
		return false
	}
	f.playing = false
	f.pauses++
	return true
}

func (f *fakePlayback) Resume() bool {
	if f.playing {
		return false
	}
	f.playing = true
	f.resumes++
	return true
}

// fakeCall is the bot's presence in the call as the audience rules see it.
type fakeCall struct {
	mu       sync.Mutex
	joined   bool
	enters   int
	leaves   int
	enterErr error
	// left is signalled on every departure, so a test can wait for the linger
	// to run out instead of sleeping for it.
	left chan struct{}
}

func newFakeCall(joined bool) *fakeCall {
	return &fakeCall{joined: joined, left: make(chan struct{}, 4)}
}

func (f *fakeCall) Enter(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enters++
	if f.enterErr != nil {
		return f.enterErr
	}
	f.joined = true
	return nil
}

func (f *fakeCall) Leave(context.Context) error {
	f.mu.Lock()
	f.leaves++
	f.joined = false
	f.mu.Unlock()
	f.left <- struct{}{}
	return nil
}

func (f *fakeCall) Joined() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.joined
}

func (f *fakeCall) counts() (enters, leaves int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.enters, f.leaves
}

// awaitLeave waits for the linger to run out, or fails the test.
func (f *fakeCall) awaitLeave(t *testing.T) {
	t.Helper()
	select {
	case <-f.left:
	case <-time.After(2 * time.Second):
		t.Fatal("the bot never left the empty call")
	}
}

// testLinger is short enough to wait out in a test and long enough not to fire
// while a test is still setting up.
const testLinger = 30 * time.Millisecond

// audienceBot is a bot with just enough wired up to run the empty-call rules,
// pointed at a homeserver that swallows whatever it announces.
func audienceBot(t *testing.T, leave bool) (*Bot, *fakePlayback, *fakeCall) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"event_id": "$sent"})
	}))
	t.Cleanup(srv.Close)
	client, err := mautrix.NewClient(srv.URL, "@bot:example.org", "token")
	if err != nil {
		t.Fatal(err)
	}
	client.Log = zerolog.New(io.Discard)

	play := &fakePlayback{playing: true}
	call := newFakeCall(true)
	return &Bot{
		cfg: &config.Config{RTC: config.RTC{
			LeaveWhenAlone: &leave,
			Linger:         testLinger,
		}},
		client:   client,
		playback: play,
		call:     call,
		calls:    primedWatcher(),
		roomID:   "!room:example.org",
	}, play, call
}

// The point of the whole thing: an empty call stops the music, and someone
// arriving starts it again.
func TestPlaybackFollowsTheAudience(t *testing.T) {
	ctx := context.Background()
	b, play, _ := audienceBot(t, true)

	b.calls.applyLegacy(bob, "_@bob:example.org_PHONE", true)
	b.followAudience(ctx)
	if play.pauses != 0 {
		t.Fatalf("paused %d times with a listener in the call; want none", play.pauses)
	}

	b.calls.applyLegacy(bob, "_@bob:example.org_PHONE", false)
	b.followAudience(ctx)
	if play.playing {
		t.Fatal("still playing to an empty call")
	}

	// A second membership change while the call is still empty must not pile up
	// announcements.
	b.followAudience(ctx)
	if play.pauses != 1 {
		t.Errorf("paused %d times; want 1", play.pauses)
	}

	b.calls.applyLegacy(bob, "_@bob:example.org_PHONE", true)
	b.followAudience(ctx)
	if !play.playing || play.resumes != 1 {
		t.Errorf("playing = %v after %d resumes; want it playing again", play.playing, play.resumes)
	}
}

// A pause somebody typed is theirs to undo: filling the call back up must not
// start the music behind their back.
func TestManualPauseSurvivesSomeoneJoining(t *testing.T) {
	ctx := context.Background()
	b, play, _ := audienceBot(t, true)

	// Someone pauses by hand, then joins the call.
	b.clearAutoPause()
	play.Pause()

	b.calls.applyLegacy(bob, "_@bob:example.org_PHONE", true)
	b.followAudience(ctx)

	if play.playing {
		t.Error("a hand-made pause was resumed when someone joined")
	}
}

// The whole behaviour is opt-out.
func TestLeaveWhenAloneCanBeTurnedOff(t *testing.T) {
	ctx := context.Background()
	b, play, _ := audienceBot(t, false)

	b.followAudience(ctx)

	if !play.playing || play.pauses != 0 {
		t.Errorf("paused %d times with leave_when_alone off; want none", play.pauses)
	}
}

// Staying in a call nobody is in costs two SFU connections and leaves the room
// showing a call in progress for as long as the bot runs. So it leaves.
func TestBotLeavesACallItIsAloneIn(t *testing.T) {
	ctx := context.Background()
	b, play, call := audienceBot(t, true)

	b.calls.applyLegacy(bob, "_@bob:example.org_PHONE", true)
	b.followAudience(ctx)
	b.calls.applyLegacy(bob, "_@bob:example.org_PHONE", false)
	b.followAudience(ctx)

	if play.playing {
		t.Error("still playing to an empty call")
	}
	call.awaitLeave(t)
	if _, leaves := call.counts(); leaves != 1 {
		t.Errorf("left %d times; want 1", leaves)
	}
}

// The linger is the whole point of the delay: a client that drops and comes
// straight back must not cost a full rejoin.
func TestSomebodyComingBackCancelsTheDeparture(t *testing.T) {
	ctx := context.Background()
	b, _, call := audienceBot(t, true)

	b.calls.applyLegacy(bob, "_@bob:example.org_PHONE", true)
	b.followAudience(ctx)
	b.calls.applyLegacy(bob, "_@bob:example.org_PHONE", false)
	b.followAudience(ctx)
	// Back before the linger runs out.
	b.calls.applyLegacy(bob, "_@bob:example.org_PHONE", true)
	b.followAudience(ctx)

	time.Sleep(4 * testLinger)
	enters, leaves := call.counts()
	if leaves != 0 {
		t.Errorf("left the call %d times; want none, the listener came back", leaves)
	}
	if enters != 0 {
		t.Errorf("rejoined %d times; want none, it never left", enters)
	}
}

// Once out of the call, the bot has to get back in before it can be heard —
// resuming playback into a connection it no longer holds would be silence.
func TestBotRejoinsWhenSomebodyArrives(t *testing.T) {
	ctx := context.Background()
	b, play, call := audienceBot(t, true)
	call.joined = false
	play.Pause()
	b.autoPaused = true

	b.calls.applyLegacy(bob, "_@bob:example.org_PHONE", true)
	b.followAudience(ctx)

	enters, _ := call.counts()
	if enters != 1 {
		t.Fatalf("entered the call %d times; want 1", enters)
	}
	if !call.Joined() {
		t.Error("not in the call after somebody joined it")
	}
	if !play.playing {
		t.Error("playback did not resume for the listener that just arrived")
	}
}

// A !play typed into a room where nobody is in the call must not stream a whole
// queue to an audience of nobody: there is no membership change coming to stop
// it, since nothing about the call has changed.
func TestPlayIntoAnEmptyCallIsHeld(t *testing.T) {
	b, play, _ := audienceBot(t, true)

	if !b.holdForEmptyCall() {
		t.Fatal("holdForEmptyCall() = false with nobody in the call")
	}
	if play.playing {
		t.Error("still playing into an empty call")
	}

	// With a listener there it is somebody else's music, and holding it would
	// be wrong.
	play.Resume()
	b.calls.applyLegacy(bob, "_@bob:example.org_PHONE", true)
	if b.holdForEmptyCall() {
		t.Error("held playback with a listener in the call")
	}
}

// The bot leaves the call it is in when told to stop watching the audience is
// not the same as never having been in one: with the behaviour off, an empty
// call changes nothing at all.
func TestNothingHappensWithLeavingTurnedOff(t *testing.T) {
	ctx := context.Background()
	b, _, call := audienceBot(t, false)

	b.followAudience(ctx)
	time.Sleep(4 * testLinger)

	if enters, leaves := call.counts(); enters != 0 || leaves != 0 {
		t.Errorf("entered %d and left %d times with leave_when_alone off; want none", enters, leaves)
	}
}

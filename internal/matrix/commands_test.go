package matrix

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix/event"

	"github.com/daedric/jellyfin-to-matrix-music-bot/internal/jellyfin"
)

func TestParseCommand(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
		body   string
		want   Command
		wantOK bool
	}{
		{name: "simple", prefix: "!", body: "!pause", want: Command{Name: "pause", Args: []string{}, Rest: ""}, wantOK: true},
		{name: "with args", prefix: "!", body: "!play 1 2 3", want: Command{Name: "play", Args: []string{"1", "2", "3"}, Rest: "1 2 3"}, wantOK: true},
		{name: "query keeps spaces", prefix: "!", body: "!search dark side of the moon", want: Command{Name: "search", Args: []string{"dark", "side", "of", "the", "moon"}, Rest: "dark side of the moon"}, wantOK: true},
		{name: "uppercase name", prefix: "!", body: "!NEXT", want: Command{Name: "next", Args: []string{}, Rest: ""}, wantOK: true},
		{name: "surrounding space", prefix: "!", body: "  !next  ", want: Command{Name: "next", Args: []string{}, Rest: ""}, wantOK: true},
		{name: "custom prefix", prefix: ".", body: ".play 2", want: Command{Name: "play", Args: []string{"2"}, Rest: "2"}, wantOK: true},
		{name: "not a command", prefix: "!", body: "hello there", wantOK: false},
		{name: "wrong prefix", prefix: "!", body: ".play 2", wantOK: false},
		{name: "prefix only", prefix: "!", body: "!", wantOK: false},
		{name: "empty", prefix: "!", body: "", wantOK: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseCommand(tc.prefix, tc.body)
			if ok != tc.wantOK {
				t.Fatalf("ParseCommand(%q, %q) ok = %v; want %v", tc.prefix, tc.body, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseCommand(%q, %q) = %+v; want %+v", tc.prefix, tc.body, got, tc.want)
			}
		})
	}
}

func TestIntArgs(t *testing.T) {
	tests := []struct {
		args []string
		want []int
		ok   bool
	}{
		{args: []string{"1"}, want: []int{1}, ok: true},
		{args: []string{"3", "1", "2"}, want: []int{3, 1, 2}, ok: true},
		{args: []string{"dark", "side"}, ok: false},
		{args: []string{"1", "side"}, ok: false},
		{args: []string{"0"}, ok: false},
		{args: []string{"-2"}, ok: false},
		{args: nil, ok: false},
	}

	for _, tc := range tests {
		got, ok := intArgs(tc.args)
		if ok != tc.ok {
			t.Errorf("intArgs(%v) ok = %v; want %v", tc.args, ok, tc.ok)
			continue
		}
		if ok && !reflect.DeepEqual(got, tc.want) {
			t.Errorf("intArgs(%v) = %v; want %v", tc.args, got, tc.want)
		}
	}
}

func TestCanonicalName(t *testing.T) {
	aliases := map[string]string{
		"np": "nowplaying", "q": "queue", "s": "search", "p": "play",
		"n": "next", "back": "prev", "unpause": "resume", "ls": "list",
		"play": "play", "unknown": "unknown",
	}
	for in, want := range aliases {
		if got := canonicalName(in); got != want {
			t.Errorf("canonicalName(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestResultsResolve(t *testing.T) {
	r := newResults(time.Minute)

	if _, err := r.resolve(1); err == nil {
		t.Error("resolve() with no results succeeded; want an error")
	}

	items := []jellyfin.Item{
		{ID: "a", Name: "First", Kind: jellyfin.KindTrack},
		{ID: "b", Name: "Second", Kind: jellyfin.KindAlbum},
	}
	r.set(items)

	got, err := r.resolve(2)
	if err != nil {
		t.Fatalf("resolve(2) error: %v", err)
	}
	if got.ID != "b" {
		t.Errorf("resolve(2) = %q; want b — indexes are 1-based", got.ID)
	}
	for _, bad := range []int{0, 3, -1} {
		if _, err := r.resolve(bad); err == nil {
			t.Errorf("resolve(%d) succeeded; want an out-of-range error", bad)
		}
	}
}

func TestResultsExpire(t *testing.T) {
	r := newResults(10 * time.Millisecond)
	r.set([]jellyfin.Item{{ID: "a", Name: "First"}})

	if _, err := r.resolve(1); err != nil {
		t.Fatalf("resolve(1) immediately after set: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := r.resolve(1); err == nil {
		t.Error("resolve(1) succeeded after the TTL elapsed; want an error")
	}
	if got := r.get(); got != nil {
		t.Errorf("get() = %v after expiry; want nil", got)
	}
}

func TestBestMatchPrefersTrack(t *testing.T) {
	items := []jellyfin.Item{
		{ID: "artist", Kind: jellyfin.KindArtist},
		{ID: "album", Kind: jellyfin.KindAlbum},
		{ID: "track", Kind: jellyfin.KindTrack},
	}
	if got := bestMatch(items); got.ID != "track" {
		t.Errorf("bestMatch() = %q; want the track", got.ID)
	}

	noTracks := []jellyfin.Item{{ID: "artist", Kind: jellyfin.KindArtist}}
	if got := bestMatch(noTracks); got.ID != "artist" {
		t.Errorf("bestMatch() with no tracks = %q; want the first item", got.ID)
	}
}

func TestParseOnOff(t *testing.T) {
	for _, arg := range []string{"on", "ON", "yes", "true", "enable", "1"} {
		if on, ok := parseOnOff(arg); !ok || !on {
			t.Errorf("parseOnOff(%q) = %v, %v; want true, true", arg, on, ok)
		}
	}
	for _, arg := range []string{"off", "OFF", "no", "false", "disable", "0"} {
		if on, ok := parseOnOff(arg); !ok || on {
			t.Errorf("parseOnOff(%q) = %v, %v; want false, true", arg, on, ok)
		}
	}
	for _, arg := range []string{"", "maybe", "toggle"} {
		if _, ok := parseOnOff(arg); ok {
			t.Errorf("parseOnOff(%q) reported a valid toggle; want not ok", arg)
		}
	}
}

func TestUserPillRendersMatrixToLink(t *testing.T) {
	got := userPill("@daedric:aguiarvieira.pt", "Ricardo Duarte")
	want := `<a href="https://matrix.to/#/@daedric:aguiarvieira.pt">Ricardo Duarte</a>`
	if got != want {
		t.Errorf("userPill() = %q; want %q", got, want)
	}
}

// A display name is arbitrary user input and must not be able to inject markup.
func TestUserPillEscapesDisplayName(t *testing.T) {
	got := userPill("@evil:example.org", `<img src=x onerror=alert(1)>`)
	if strings.Contains(got, "<img") {
		t.Errorf("userPill() left raw markup in the output: %q", got)
	}
}

// The initial sync replays existing memberships; those must not be announced,
// or every restart would report everyone already in the call as joining.
func TestCallWatcherSuppressesInitialState(t *testing.T) {
	w := newCallWatcher()

	if got := w.handleMembership("_@alice:example.org_DEV", true); got != callNoChange {
		t.Errorf("got %v before priming; initial state must be silent", got)
	}
	w.prime()

	if got := w.handleMembership("_@bob:example.org_DEV", true); got != callJoined {
		t.Errorf("got %v for a join after priming; want callJoined", got)
	}
	// State events are re-sent on every update, so a repeat is not a new join.
	if got := w.handleMembership("_@bob:example.org_DEV", true); got != callNoChange {
		t.Errorf("got %v for a repeated membership; want callNoChange", got)
	}
	if got := w.handleMembership("_@alice:example.org_DEV", true); got != callNoChange {
		t.Errorf("got %v for someone already present at prime time; want callNoChange", got)
	}
}

func TestCallWatcherAnnouncesLeaveAndRejoin(t *testing.T) {
	w := newCallWatcher()
	w.prime()

	if got := w.handleMembership("_@bob:example.org_DEV", true); got != callJoined {
		t.Fatalf("got %v for the first join; want callJoined", got)
	}
	if got := w.handleMembership("_@bob:example.org_DEV", false); got != callLeft {
		t.Errorf("got %v for a leave; want callLeft", got)
	}
	// A retraction for someone already gone is not a second leave.
	if got := w.handleMembership("_@bob:example.org_DEV", false); got != callNoChange {
		t.Errorf("got %v for a repeated leave; want callNoChange", got)
	}
	if got := w.handleMembership("_@bob:example.org_DEV", true); got != callJoined {
		t.Errorf("got %v for a rejoin; want callJoined", got)
	}
}

// Someone in the call from two devices who closes one has not left.
func TestCallWatcherTracksMultipleDevices(t *testing.T) {
	w := newCallWatcher()
	w.prime()

	w.handleMembership("_@bob:example.org_PHONE", true)
	w.handleMembership("_@bob:example.org_LAPTOP", true)

	if got := w.handleMembership("_@bob:example.org_LAPTOP", false); got != callLeft {
		t.Fatalf("got %v when one device left; want callLeft", got)
	}
	if !w.stillPresent("@bob:example.org") {
		t.Error("stillPresent() = false while another device is in the call")
	}

	if got := w.handleMembership("_@bob:example.org_PHONE", false); got != callLeft {
		t.Fatalf("got %v when the last device left; want callLeft", got)
	}
	if w.stillPresent("@bob:example.org") {
		t.Error("stillPresent() = true after every device left")
	}
	if w.stillPresent("@someoneelse:example.org") {
		t.Error("stillPresent() matched a user who was never in the call")
	}
}

func TestIsEmptyJSONObject(t *testing.T) {
	for _, raw := range []string{"{}", " {} ", "", "null"} {
		if !isEmptyJSONObject([]byte(raw)) {
			t.Errorf("isEmptyJSONObject(%q) = false; want true", raw)
		}
	}
	if isEmptyJSONObject([]byte(`{"application":"m.call"}`)) {
		t.Error("a populated membership was treated as empty")
	}
}

// MSC4391 structured invocations must reach exactly the same handlers as typed
// text, so the two paths cannot drift apart in behaviour.
func TestStructuredCommandMatchesTypedText(t *testing.T) {
	tests := []struct {
		name      string
		command   string
		arguments string
		want      Command
	}{
		{
			name: "no arguments", command: "pause", arguments: "",
			want: Command{Name: "pause", Args: []string{}, Rest: ""},
		},
		{
			name: "search with kind", command: "search",
			arguments: `{"kind":"album","query":"dark side of the moon"}`,
			want:      Command{Name: "search", Args: []string{"album", "dark", "side", "of", "the", "moon"}, Rest: "album dark side of the moon"},
		},
		{
			name: "search without the optional kind", command: "search",
			arguments: `{"query":"mario"}`,
			want:      Command{Name: "search", Args: []string{"mario"}, Rest: "mario"},
		},
		{
			name: "play by result numbers", command: "play",
			arguments: `{"selection":[3,1,2]}`,
			want:      Command{Name: "play", Args: []string{"3", "1", "2"}, Rest: "3 1 2"},
		},
		{
			name: "play by query", command: "play",
			arguments: `{"selection":["ocarina","of","time"]}`,
			want:      Command{Name: "play", Args: []string{"ocarina", "of", "time"}, Rest: "ocarina of time"},
		},
		{
			name: "integer argument", command: "skip",
			arguments: `{"position":7}`,
			want:      Command{Name: "skip", Args: []string{"7"}, Rest: "7"},
		},
		{
			name: "boolean becomes on", command: "random",
			arguments: `{"enabled":true}`,
			want:      Command{Name: "random", Args: []string{"on"}, Rest: "on"},
		},
		{
			name: "boolean becomes off", command: "repeat",
			arguments: `{"enabled":false}`,
			want:      Command{Name: "repeat", Args: []string{"off"}, Rest: "off"},
		},
		{
			name: "omitted optional reports state", command: "random", arguments: "{}",
			want: Command{Name: "random", Args: []string{}, Rest: ""},
		},
		{
			name: "aliases resolve", command: "np", arguments: "",
			want: Command{Name: "nowplaying", Args: []string{}, Rest: ""},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input := &event.MSC4391BotCommandInput{Command: tc.command}
			if tc.arguments != "" {
				input.Arguments = json.RawMessage(tc.arguments)
			}
			got, ok := structuredCommand(input)
			if !ok {
				t.Fatalf("structuredCommand(%q) was rejected", tc.command)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("structuredCommand() = %+v; want %+v", got, tc.want)
			}
		})
	}
}

func TestStructuredCommandRejectsJunk(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input *event.MSC4391BotCommandInput
	}{
		{"nil", nil},
		{"empty command", &event.MSC4391BotCommandInput{}},
		{"unknown command", &event.MSC4391BotCommandInput{Command: "selfdestruct"}},
		{"malformed arguments", &event.MSC4391BotCommandInput{
			Command: "search", Arguments: json.RawMessage(`["not","an","object"]`)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := structuredCommand(tc.input); ok {
				t.Error("structuredCommand() accepted it; want rejected so the text path can try")
			}
		})
	}
}

// The state key is what stops two bots' descriptions colliding, so it has to be
// exactly what the MSC specifies: padded base64 of sha256(command + mxid).
func TestDescriptionStateKey(t *testing.T) {
	sum := sha256.Sum256([]byte("play@jellybot:example.org"))
	want := base64.StdEncoding.EncodeToString(sum[:])

	got := descriptionStateKey("play", "@jellybot:example.org")
	if got != want {
		t.Errorf("descriptionStateKey() = %q; want %q", got, want)
	}
	if strings.HasSuffix(got, "=") == false && len(got) != 44 {
		t.Errorf("state key %q does not look like padded base64", got)
	}

	// Different bots and different commands must not collide.
	if descriptionStateKey("play", "@other:example.org") == got {
		t.Error("state key ignores the bot's user ID")
	}
	if descriptionStateKey("stop", "@jellybot:example.org") == got {
		t.Error("state key ignores the command name")
	}
}

// Every advertised command must be one dispatch actually handles, or clients
// would offer commands that answer "unknown command".
func TestAdvertisedCommandsAreHandled(t *testing.T) {
	handled := map[string]bool{
		"help": true, "search": true, "list": true, "play": true, "queue": true,
		"nowplaying": true, "pause": true, "resume": true, "next": true,
		"prev": true, "skip": true, "random": true, "repeat": true,
		"clear": true, "stop": true,
	}
	for _, spec := range commandSpecs() {
		if !handled[spec.Command] {
			t.Errorf("advertised command %q is not handled by dispatch", spec.Command)
		}
		if canonicalName(spec.Command) != spec.Command {
			t.Errorf("advertised command %q is an alias; advertise the canonical name", spec.Command)
		}
		if spec.Description.Text[0].Body == "" {
			t.Errorf("command %q has no description", spec.Command)
		}
		seen := map[string]bool{}
		for _, param := range spec.Parameters {
			if seen[param.Key] {
				t.Errorf("command %q has duplicate parameter %q; the MSC says clients must hide it", spec.Command, param.Key)
			}
			seen[param.Key] = true
		}
	}
}

// MSC4391 describes "parameters" as an array. Go marshals a nil slice to null,
// which a client validating descriptions against the schema rejects, so every
// argument-less command would go unoffered.
func TestPublishedDescriptionsAlwaysCarryAParameterArray(t *testing.T) {
	for _, spec := range commandSpecs() {
		raw, err := json.Marshal(spec)
		if err != nil {
			t.Fatalf("marshalling %q: %v", spec.Command, err)
		}
		var content struct {
			Parameters *[]json.RawMessage `json:"parameters"`
		}
		if err := json.Unmarshal(raw, &content); err != nil {
			t.Fatalf("unmarshalling %q: %v", spec.Command, err)
		}
		if content.Parameters == nil {
			t.Errorf("command %q published %s; want an array for parameters", spec.Command, raw)
		}
	}
}

// Matrix does not deduplicate state events: re-sending identical content still
// writes a new event. Without this check the bot would add one event per
// command to room state on every restart, forever.
func TestUnchangedDescriptionSkipsRewrites(t *testing.T) {
	spec := commandSpecs()[0]
	encoded, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}

	same := &event.Event{Content: event.Content{VeryRaw: encoded}}
	if !unchangedDescription(same, spec) {
		t.Error("identical description reported as changed; it would be rewritten every restart")
	}

	// Key order and whitespace are not significant.
	var reordered map[string]any
	if err := json.Unmarshal(encoded, &reordered); err != nil {
		t.Fatal(err)
	}
	shuffled, err := json.MarshalIndent(reordered, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !unchangedDescription(&event.Event{Content: event.Content{VeryRaw: shuffled}}, spec) {
		t.Error("re-encoded description reported as changed; comparison should be semantic")
	}

	// A real change must be detected, or edits would never reach clients.
	edited := spec
	edited.Description = describe("something else entirely")
	if unchangedDescription(same, edited) {
		t.Error("edited description reported as unchanged; clients would keep the old text")
	}

	for _, missing := range []*event.Event{
		nil,
		{Content: event.Content{}},
		{Content: event.Content{VeryRaw: []byte("{}")}},
	} {
		if unchangedDescription(missing, spec) {
			t.Error("missing or retracted description reported as unchanged; it would never be published")
		}
	}
}

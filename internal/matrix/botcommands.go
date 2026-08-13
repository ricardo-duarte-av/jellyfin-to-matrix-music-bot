package matrix

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// MSC4391 lets a bot describe its commands as state events so clients can offer
// them directly, and lets clients send a structured invocation instead of a
// line of text.
//
// https://github.com/matrix-org/matrix-spec-proposals/pull/4391
//
// One table drives both directions here. The descriptions published to the room
// and the parser that reads structured invocations come from the same specs, so
// they cannot describe different things.

// schema is an MSC4391 argument type schema.
type schema struct {
	SchemaType string `json:"schema_type"`
	// Primitive types.
	Type string `json:"type,omitempty"`
	// Literal types.
	LiteralType string `json:"literal_type,omitempty"`
	Value       any    `json:"value,omitempty"`
	// Composite types.
	Items    *schema  `json:"items,omitempty"`
	Variants []schema `json:"variants,omitempty"`
}

func primitive(t string) schema { return schema{SchemaType: "primitive", Type: t} }

func literal(value string) schema {
	return schema{SchemaType: "literal", LiteralType: "string", Value: value}
}

func union(variants ...schema) schema {
	return schema{SchemaType: "union", Variants: variants}
}

func arrayOf(items schema) schema {
	return schema{SchemaType: "array", Items: &items}
}

// textBlock is an MSC1767 m.text content block, which MSC4391 uses for
// descriptions so they can be translated later.
type textBlock struct {
	Text []textBody `json:"m.text"`
}

type textBody struct {
	Body string `json:"body"`
}

func describe(s string) textBlock {
	return textBlock{Text: []textBody{{Body: s}}}
}

// paramSpec is one parameter of a command.
type paramSpec struct {
	Key         string    `json:"key"`
	Schema      schema    `json:"schema"`
	Description textBlock `json:"description"`
	Optional    bool      `json:"optional,omitempty"`
}

// commandSpec is a command description as published to the room.
type commandSpec struct {
	Command     string      `json:"command"`
	Parameters  []paramSpec `json:"parameters"`
	Description textBlock   `json:"description"`
}

// commandSpecs describes every command the bot accepts.
//
// Parameter order matters: it is the order clients prompt in, and the order a
// structured invocation is flattened back into for the text handlers.
func commandSpecs() []commandSpec {
	kind := union(
		literal("artist"), literal("album"), literal("track"), literal("playlist"),
	)
	// Result numbers or free text, which is exactly what the text parser
	// accepts: all integers means indexes into the last search, anything else
	// is a query.
	selection := arrayOf(union(primitive("integer"), primitive("string")))

	specs := []commandSpec{
		{
			Command:     "search",
			Description: describe("Search the music library"),
			Parameters: []paramSpec{
				{Key: "kind", Schema: kind, Optional: true,
					Description: describe("Limit the search to one kind of item")},
				{Key: "query", Schema: primitive("string"),
					Description: describe("What to search for")},
			},
		},
		{
			Command:     "play",
			Description: describe("Replace the queue with this and start playing it now"),
			Parameters: []paramSpec{
				{Key: "selection", Schema: selection,
					Description: describe("Result numbers from the last search, or a search query")},
			},
		},
		{
			Command:     "queue",
			Description: describe("Add to the end of the queue, or show the queue"),
			Parameters: []paramSpec{
				{Key: "selection", Schema: selection, Optional: true,
					Description: describe("Result numbers from the last search, or a search query")},
			},
		},
		{
			Command:     "skip",
			Description: describe("Jump to a position in the queue"),
			Parameters: []paramSpec{
				{Key: "position", Schema: primitive("integer"),
					Description: describe("Queue position, starting at 1")},
			},
		},
		{
			Command:     "random",
			Description: describe("Shuffle the queue"),
			Parameters: []paramSpec{
				{Key: "enabled", Schema: primitive("boolean"), Optional: true,
					Description: describe("Leave empty to see the current setting")},
			},
		},
		{
			Command:     "repeat",
			Description: describe("Loop the queue when it ends"),
			Parameters: []paramSpec{
				{Key: "enabled", Schema: primitive("boolean"), Optional: true,
					Description: describe("Leave empty to see the current setting")},
			},
		},
		{Command: "list", Description: describe("Show the last search results again")},
		{Command: "nowplaying", Description: describe("Show the current track")},
		{Command: "pause", Description: describe("Pause playback")},
		{Command: "resume", Description: describe("Resume playback")},
		{Command: "next", Description: describe("Skip to the next track")},
		{Command: "prev", Description: describe("Go back to the previous track")},
		{Command: "clear", Description: describe("Drop everything after the current track")},
		{
			Command:     "eject",
			Description: describe("Remove someone from the call"),
			Parameters: []paramSpec{
				{Key: "user_id", Schema: primitive("user_id"),
					Description: describe("Who to remove; they can rejoin")},
			},
		},
		{Command: "stop", Description: describe("Stop playing and empty the queue")},
		{Command: "help", Description: describe("List the commands")},
	}

	// A nil slice marshals to "parameters": null, but MSC4391 describes
	// parameters as an array. A client validating the description against that
	// schema drops the command, so an argument-less command like stop or help
	// would never be offered or sent.
	for i := range specs {
		if specs[i].Parameters == nil {
			specs[i].Parameters = []paramSpec{}
		}
	}
	return specs
}

// descriptionStateKey is the state key a command description lives under.
//
// MSC4391 defines it as the padded base64 SHA256 of the command and the bot's
// user ID, so descriptions from different bots cannot collide.
func descriptionStateKey(command string, user id.UserID) string {
	sum := sha256.Sum256([]byte(command + user.String()))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// advertiseCommands publishes the command descriptions and removes any stale
// ones the bot published previously.
//
// Failures are not fatal: the descriptions are state events, so a bot without
// permission to send state simply goes without them, and the text commands work
// regardless.
func (b *Bot) advertiseCommands(ctx context.Context) {
	specs := commandSpecs()

	// Read the room's existing descriptions first. Matrix does not deduplicate
	// state events: re-sending identical content still writes a new event, so
	// advertising unconditionally would add an event per command on every
	// restart, forever.
	var existing map[string]*event.Event
	if state, err := b.client.State(ctx, b.roomID); err != nil {
		b.client.Log.Warn().Err(err).Msg("could not read existing command descriptions; re-advertising all")
	} else {
		existing = state[event.StateMSC4391BotCommand]
	}

	current := make(map[string]bool, len(specs))
	published := 0
	for _, spec := range specs {
		key := descriptionStateKey(spec.Command, b.client.UserID)
		current[key] = true
		if unchangedDescription(existing[key], spec) {
			continue
		}
		if _, err := b.client.SendStateEvent(ctx, b.roomID, event.StateMSC4391BotCommand, key, &spec); err != nil {
			b.client.Log.Warn().Err(err).Str("command", spec.Command).
				Msg("could not advertise command; clients will not offer it, text commands still work")
			// One failure usually means no permission to send state, so the
			// rest will fail the same way.
			return
		}
		published++
	}
	b.client.Log.Info().
		Int("commands", len(specs)).
		Int("published", published).
		Msg("command descriptions are up to date")

	b.pruneCommandDescriptions(ctx, existing, current)
}

// unchangedDescription reports whether the room already carries this exact
// description, so it does not need rewriting.
func unchangedDescription(evt *event.Event, spec commandSpec) bool {
	if evt == nil || len(evt.Content.VeryRaw) == 0 {
		return false
	}
	want, err := json.Marshal(spec)
	if err != nil {
		return false
	}
	// Compare decoded, since key order and whitespace are not significant.
	var have, wanted any
	if err := json.Unmarshal(evt.Content.VeryRaw, &have); err != nil {
		return false
	}
	if err := json.Unmarshal(want, &wanted); err != nil {
		return false
	}
	return reflect.DeepEqual(have, wanted)
}

// pruneCommandDescriptions retracts descriptions for commands that no longer
// exist. State keys hash the command name, so a renamed or removed command
// leaves an orphan that clients would keep offering.
func (b *Bot) pruneCommandDescriptions(ctx context.Context, existing map[string]*event.Event, current map[string]bool) {
	for key, evt := range existing {
		if evt == nil || current[key] || evt.Sender != b.client.UserID {
			continue
		}
		if isEmptyJSONObject(evt.Content.VeryRaw) {
			continue
		}
		if _, err := b.client.SendStateEvent(ctx, b.roomID, event.StateMSC4391BotCommand, key, struct{}{}); err != nil {
			b.client.Log.Warn().Err(err).Str("state_key", key).Msg("could not retract a stale command description")
			continue
		}
		b.client.Log.Info().Str("state_key", key).Msg("retracted a stale command description")
	}
}

// structuredCommand turns an MSC4391 invocation into the same Command the text
// parser produces, so both paths reach identical handlers.
func structuredCommand(input *event.MSC4391BotCommandInput) (Command, bool) {
	if input == nil || input.Command == "" {
		return Command{}, false
	}
	name := canonicalName(strings.ToLower(strings.TrimSpace(input.Command)))

	var spec *commandSpec
	for _, candidate := range commandSpecs() {
		if candidate.Command == name {
			spec = &candidate
			break
		}
	}
	if spec == nil {
		return Command{}, false
	}

	var args map[string]json.RawMessage
	if len(input.Arguments) > 0 {
		if err := json.Unmarshal(input.Arguments, &args); err != nil {
			return Command{}, false
		}
	}

	// Flatten in the declared parameter order, which is the order the text
	// form uses.
	var tokens []string
	for _, param := range spec.Parameters {
		raw, ok := args[param.Key]
		if !ok {
			continue
		}
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			return Command{}, false
		}
		tokens = append(tokens, flattenArgument(value)...)
	}

	rest := strings.Join(tokens, " ")
	return Command{Name: name, Args: strings.Fields(rest), Rest: rest}, true
}

// flattenArgument renders a JSON argument as the words a user would have typed.
func flattenArgument(value any) []string {
	switch typed := value.(type) {
	case string:
		if trimmed := strings.TrimSpace(typed); trimmed != "" {
			return []string{trimmed}
		}
	case bool:
		return []string{onOff(typed)}
	case float64:
		// JSON has no integers; MSC4391 only permits whole numbers.
		return []string{strconv.FormatInt(int64(typed), 10)}
	case []any:
		var out []string
		for _, item := range typed {
			out = append(out, flattenArgument(item)...)
		}
		return out
	case map[string]any:
		// Room and event references carry their id alongside routing info.
		if id, ok := typed["id"].(string); ok {
			return []string{id}
		}
	case nil:
		return nil
	default:
		return []string{fmt.Sprint(typed)}
	}
	return nil
}

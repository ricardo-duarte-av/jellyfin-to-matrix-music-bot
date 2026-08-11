package matrix

import (
	"strconv"
	"strings"
)

// Command is a parsed chat command.
type Command struct {
	// Name is the command word, lowercased and without the prefix.
	Name string
	// Args are the whitespace-separated arguments.
	Args []string
	// Rest is everything after the command word, unsplit — used for search
	// queries, which may contain spaces.
	Rest string
}

// ParseCommand extracts a command from a message body. It returns false when
// the message is not a command for this bot.
func ParseCommand(prefix, body string) (Command, bool) {
	body = strings.TrimSpace(body)
	if prefix == "" || !strings.HasPrefix(body, prefix) {
		return Command{}, false
	}
	body = strings.TrimPrefix(body, prefix)
	if body == "" {
		return Command{}, false
	}

	name, rest, _ := strings.Cut(body, " ")
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return Command{}, false
	}
	rest = strings.TrimSpace(rest)
	return Command{Name: name, Args: strings.Fields(rest), Rest: rest}, true
}

// intArgs interprets the arguments as a list of positive integers. It reports
// false if any argument is not one, which is how "!play <query>" is told apart
// from "!play 3 4".
func intArgs(args []string) ([]int, bool) {
	if len(args) == 0 {
		return nil, false
	}
	out := make([]int, 0, len(args))
	for _, arg := range args {
		n, err := strconv.Atoi(arg)
		if err != nil || n < 1 {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// canonicalName maps command aliases to their canonical name.
func canonicalName(name string) string {
	switch name {
	case "np", "now", "nowplaying", "current":
		return "nowplaying"
	case "q":
		return "queue"
	case "s", "find":
		return "search"
	case "p":
		return "play"
	case "skip":
		return "skip"
	case "unpause", "continue":
		return "resume"
	case "n":
		return "next"
	case "previous", "back":
		return "prev"
	case "h", "commands":
		return "help"
	case "ls", "results":
		return "list"
	}
	return name
}

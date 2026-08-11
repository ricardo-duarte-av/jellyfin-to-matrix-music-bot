// Package logging sets up the bot's logger and bridges LiveKit's and pion's
// logging into it.
package logging

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"
	protoLogger "github.com/livekit/protocol/logger"
	"github.com/rs/zerolog"
)

// New builds the bot's logger. level is one of debug, info, warn, error.
func New(level string) (zerolog.Logger, error) {
	parsed, err := zerolog.ParseLevel(strings.ToLower(strings.TrimSpace(level)))
	if err != nil || parsed == zerolog.NoLevel {
		return zerolog.Nop(), fmt.Errorf("invalid log_level %q: use debug, info, warn or error", level)
	}
	writer := zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339}
	return zerolog.New(writer).Level(parsed).With().Timestamp().Logger(), nil
}

// LiveKit adapts the bot's logger for the LiveKit SDK, which also carries
// pion's WebRTC logging.
//
// Both are extremely chatty at info level — an idle connection produces
// hundreds of KB per minute of ICE candidate churn — so unless the bot is in
// debug mode their info output is demoted to debug. Warnings and errors always
// come through, since those are what actually matter when a call misbehaves.
func LiveKit(base zerolog.Logger, verbose bool) protoLogger.Logger {
	sink := &zerologSink{log: base.With().Str("component", "livekit").Logger(), verbose: verbose}
	return protoLogger.LogRLogger(logr.New(sink))
}

// zerologSink implements logr.LogSink on top of zerolog.
type zerologSink struct {
	log     zerolog.Logger
	name    string
	verbose bool
}

func (s *zerologSink) Init(logr.RuntimeInfo) {}

// Enabled gates Info calls only; logr routes errors through Error regardless.
func (s *zerologSink) Enabled(level int) bool {
	if s.verbose {
		return true
	}
	return s.log.GetLevel() <= zerolog.DebugLevel
}

func (s *zerologSink) Info(level int, msg string, kv ...any) {
	event := s.log.Debug()
	if s.verbose {
		event = s.log.Info()
	}
	s.emit(event, msg, kv)
}

func (s *zerologSink) Error(err error, msg string, kv ...any) {
	s.emit(s.log.Error().Err(err), msg, kv)
}

func (s *zerologSink) emit(event *zerolog.Event, msg string, kv []any) {
	if event == nil {
		return
	}
	if s.name != "" {
		event = event.Str("logger", s.name)
	}
	for i := 0; i+1 < len(kv); i += 2 {
		event = event.Interface(fmt.Sprint(kv[i]), kv[i+1])
	}
	event.Msg(msg)
}

func (s *zerologSink) WithValues(kv ...any) logr.LogSink {
	ctx := s.log.With()
	for i := 0; i+1 < len(kv); i += 2 {
		ctx = ctx.Interface(fmt.Sprint(kv[i]), kv[i+1])
	}
	return &zerologSink{log: ctx.Logger(), name: s.name, verbose: s.verbose}
}

func (s *zerologSink) WithName(name string) logr.LogSink {
	if s.name != "" {
		name = s.name + "." + name
	}
	return &zerologSink{log: s.log, name: name, verbose: s.verbose}
}

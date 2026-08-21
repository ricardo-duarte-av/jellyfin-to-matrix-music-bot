package rtc

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// SlotEventType is the MSC4143 state event that declares a MatrixRTC slot: the
// virtual address members meet at. Sticky membership is inert without one —
// clients MUST treat a member as left unless an open slot exists with a
// state_key matching the member event's slot_id.
//
// The class must be set explicitly, for the reason spelled out on
// CallMemberEventType.
var SlotEventType = event.Type{
	Type:  "org.matrix.msc4143.rtc.slot",
	Class: event.StateEventType,
}

// DefaultSlotID is the slot every calling client uses for the room-wide call.
// It is also the value lk-jwt-service hardcodes when serving legacy /sfu/get
// requests, which is what makes the legacy and sticky stacks derive the same
// LiveKit room alias and therefore land in the same SFU room.
const DefaultSlotID = "m.call#ROOM"

const (
	slotStatusOpen   = "open"
	slotStatusClosed = "closed"
)

// SlotApplication names the application running in a slot.
type SlotApplication struct {
	Type string `json:"type"`
}

// SlotContent is the content of an m.rtc.slot state event.
type SlotContent struct {
	Status      string          `json:"status"`
	Application SlotApplication `json:"application,omitempty"`
}

// IsOpenCall reports whether this slot is one the bot can join: open, and
// running the calling application rather than something else.
func (s SlotContent) IsOpenCall() bool {
	return s.Status == slotStatusOpen && s.Application.Type == applicationCall
}

// EnsureSlot opens the slot if it is not already open.
//
// An already-open slot is left alone rather than rewritten: re-sending state
// the room already holds is pure churn, and other clients react to it.
func EnsureSlot(ctx context.Context, client *mautrix.Client, roomID id.RoomID, slotID string) error {
	var existing SlotContent
	err := client.StateEvent(ctx, roomID, SlotEventType, slotID, &existing)
	switch {
	case err == nil && existing.IsOpenCall():
		client.Log.Debug().Str("slot_id", slotID).Msg("MatrixRTC slot already open")
		return nil
	case err != nil && !isStateNotFound(err):
		return fmt.Errorf("read MatrixRTC slot %q: %w", slotID, err)
	}

	content := SlotContent{Status: slotStatusOpen, Application: SlotApplication{Type: applicationCall}}
	if _, err := client.SendStateEvent(ctx, roomID, SlotEventType, slotID, &content); err != nil {
		return fmt.Errorf(
			"open MatrixRTC slot %q: %w; give the bot permission to send %s events in this room, "+
				"or set rtc.stack: legacy in config.yaml",
			slotID, err, SlotEventType.Type)
	}
	client.Log.Info().Str("slot_id", slotID).Msg("opened MatrixRTC slot")
	return nil
}

// isStateNotFound reports whether err is the homeserver saying the state event
// simply is not there, as opposed to something having gone wrong.
func isStateNotFound(err error) bool {
	var httpErr mautrix.HTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	if httpErr.RespError != nil && httpErr.RespError.ErrCode == mautrix.MNotFound.ErrCode {
		return true
	}
	return httpErr.Response != nil && httpErr.Response.StatusCode == http.StatusNotFound
}

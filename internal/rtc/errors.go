package rtc

import (
	"errors"
	"net/http"

	"maunium.net/go/mautrix"
)

// isNotFound reports whether err is the homeserver saying the thing simply is
// not there, as opposed to something having gone wrong.
func isNotFound(err error) bool {
	var httpErr mautrix.HTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	if httpErr.RespError != nil {
		return httpErr.RespError.ErrCode == mautrix.MNotFound.ErrCode
	}
	return httpErr.Response != nil && httpErr.Response.StatusCode == http.StatusNotFound
}

// isEndpointMissing reports whether err is the homeserver saying it does not
// implement this endpoint, which is the only error worth falling through on
// when walking the token ladder.
func isEndpointMissing(err error) bool {
	var httpErr mautrix.HTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	if httpErr.RespError != nil {
		switch httpErr.RespError.ErrCode {
		case mautrix.MUnrecognized.ErrCode, mautrix.MNotFound.ErrCode:
			return true
		default:
			return false
		}
	}
	return httpErr.Response != nil && httpErr.Response.StatusCode == http.StatusNotFound
}

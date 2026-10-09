package notify

import (
	"context"
	"errors"

	"github.com/coder/websocket"
)

// Sentinel errors used by the Listener to label disconnect causes. The
// Listener wraps the underlying error with %w so classifyError can use
// errors.Is / errors.As for typed matching instead of substring search
// on err.Error().
var (
	// errDialFailed marks a failure to establish the WebSocket
	// connection (DNS, TCP, handshake, TLS).
	errDialFailed = errors.New("websocket dial")

	// errReadLimit marks a read that exceeded the 1 MB read limit.
	errReadLimit = errors.New("read limited")

	// errReadError marks a generic read failure (EOF on a truncated
	// frame, i/o timeout, transport error).
	errReadError = errors.New("websocket read")

	// errServerClose marks a clean or expected close from the server
	// side (close frames 1000/1001/1006, plain EOF).
	errServerClose = errors.New("server close")
)

// Reason codes logged as the disconnect `reason` value. Operators filter
// logs on these strings, so keep them stable.
const (
	reasonUnknown     = "unknown"
	reasonServerClose = "server_close"
	reasonReadLimit   = "read_limit"
	reasonDialFailed  = "dial_failed"
	reasonReadError   = "read_error"
)

// classifyError maps a WebSocket disconnect error to a stable reason
// code so alerts can segment by cause without matching on error text.
//
// Classification order (first match wins):
//  1. nil → reasonUnknown
//  2. context.DeadlineExceeded → reasonReadError (read-idle backstop fired)
//  3. typed sentinel wraps via errors.Is (errReadLimit, errServerClose,
//     errDialFailed, errReadError)
//  4. *websocket.CloseError via errors.As for codes 1000 / 1001 / 1006
//     → reasonServerClose
//  5. default → reasonUnknown
func classifyError(err error) string {
	if err == nil {
		return reasonUnknown
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return reasonReadError
	}
	switch {
	case errors.Is(err, errReadLimit):
		return reasonReadLimit
	case errors.Is(err, errServerClose):
		return reasonServerClose
	case errors.Is(err, errDialFailed):
		return reasonDialFailed
	case errors.Is(err, errReadError):
		return reasonReadError
	}
	if ce, ok := errors.AsType[websocket.CloseError](err); ok && isServerCloseCode(ce.Code) {
		return reasonServerClose
	}
	return reasonUnknown
}

// isServerCloseCode reports whether a WebSocket close code represents an
// expected server-side close (normal closure, going away, or abnormal
// closure). Centralizes the close-code set shared by classifyError and
// the listener's wrapReadError so the two cannot drift apart.
func isServerCloseCode(code websocket.StatusCode) bool {
	switch code {
	case websocket.StatusNormalClosure,
		websocket.StatusGoingAway,
		websocket.StatusAbnormalClosure:
		return true
	default:
		return false
	}
}

package notify

import (
	"context"
	"errors"

	"github.com/coder/websocket"
)

// Sentinel errors used by the Listener to label disconnect causes. The
// Listener wraps the underlying error with %w so ClassifyError can use
// errors.Is / errors.As for typed matching instead of substring search
// on err.Error(). Code outside this package rarely needs these directly;
// ClassifyError is the consumer API.
var (
	// ErrDialFailed marks a failure to establish the WebSocket
	// connection (DNS, TCP, handshake, TLS).
	ErrDialFailed = errors.New("websocket dial")

	// ErrReadLimit marks a read that exceeded the 1 MB read limit.
	ErrReadLimit = errors.New("read limited")

	// ErrReadError marks a generic read failure (EOF on a truncated
	// frame, i/o timeout, transport error).
	ErrReadError = errors.New("websocket read")

	// ErrServerClose marks a clean or expected close from the server
	// side (close frames 1000/1001/1006, plain EOF).
	ErrServerClose = errors.New("server close")
)

// Reason codes surfaced to logs (and thus to Loki alert rules). These
// string values are byte-for-byte frozen because Loki alert rules match
// on them.
const (
	ReasonUnknown     = "unknown"
	ReasonServerClose = "server_close"
	ReasonReadLimit   = "read_limit"
	ReasonDialFailed  = "dial_failed"
	ReasonReadError   = "read_error"
)

// ClassifyError maps a WebSocket disconnect error to a stable reason
// code so alerts can segment by cause without matching on error text.
//
// Classification order (first match wins):
//  1. nil → ReasonUnknown
//  2. context.DeadlineExceeded → ReasonReadError (read-idle backstop fired)
//  3. typed sentinel wraps via errors.Is (ErrReadLimit, ErrServerClose,
//     ErrDialFailed, ErrReadError)
//  4. *websocket.CloseError via errors.As for codes 1000 / 1001 / 1006
//     → ReasonServerClose
//  5. default → ReasonUnknown
func ClassifyError(err error) string {
	if err == nil {
		return ReasonUnknown
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ReasonReadError
	}
	switch {
	case errors.Is(err, ErrReadLimit):
		return ReasonReadLimit
	case errors.Is(err, ErrServerClose):
		return ReasonServerClose
	case errors.Is(err, ErrDialFailed):
		return ReasonDialFailed
	case errors.Is(err, ErrReadError):
		return ReasonReadError
	}
	if ce, ok := errors.AsType[websocket.CloseError](err); ok && isServerCloseCode(ce.Code) {
		return ReasonServerClose
	}
	return ReasonUnknown
}

// isServerCloseCode reports whether a WebSocket close code represents an
// expected server-side close (normal closure, going away, or abnormal
// closure). Centralizes the close-code set shared by ClassifyError and
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

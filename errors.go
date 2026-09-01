package announcer

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// Sentinel errors for errors.Is. Every API failure returns an *Error that
// unwraps to exactly one of these (a suppressed recipient unwraps to both
// ErrSuppressedRecipient and ErrUnprocessable), so callers can branch on the
// kind without type-asserting:
//
//	if errors.Is(err, announcer.ErrSuppressedRecipient) {
//	    // they bounced or complained before; stop mailing them
//	}
//
// Use errors.As to reach the details:
//
//	var apiErr *announcer.Error
//	if errors.As(err, &apiErr) {
//	    log.Printf("status=%d request=%s", apiErr.StatusCode, apiErr.RequestID)
//	}
var (
	// ErrAuthentication is a 401: the key is missing, malformed, or revoked.
	ErrAuthentication = newKind("authentication failed")

	// ErrPermission is a 403: the key is valid but not allowed to do this.
	// In practice a send-scoped key touching the management surface, or a
	// From: domain that is unregistered or unverified.
	ErrPermission = newKind("permission denied")

	// ErrNotFound is a 404: no such resource, or it belongs to another account.
	ErrNotFound = newKind("not found")

	// ErrConflict is a 409: a duplicate domain, or a send with this
	// Idempotency-Key still in flight. The in-flight case is retried
	// automatically first.
	ErrConflict = newKind("conflict")

	// ErrValidation is a 400. See Error.Errors for the per-field messages.
	ErrValidation = newKind("validation failed")

	// ErrUnprocessable is a 422: well-formed, but it cannot be carried out.
	ErrUnprocessable = newKind("unprocessable")

	// ErrSuppressedRecipient is a 422 from a send: the recipient hard-bounced
	// or complained before. Do not retry.
	ErrSuppressedRecipient = newKind("recipient is suppressed")

	// ErrRateLimit is a 429. See Error.RetryAfter.
	ErrRateLimit = newKind("rate limited")

	// ErrServer is a 5xx. Retried automatically before you see it.
	ErrServer = newKind("server error")

	// ErrConnection means the request never got an answer: DNS, TCP, TLS, or
	// the client-side timeout. Error.Unwrap also yields the underlying cause.
	ErrConnection = newKind("connection failed")

	// ErrSignatureVerification means a webhook's X-Announcer-Signature did not
	// check out. Treat the delivery as hostile.
	ErrSignatureVerification = newKind("webhook signature verification failed")
)

type kind struct{ s string }

func newKind(s string) error  { return &kind{s} }
func (k *kind) Error() string { return "announcer: " + k.s }

// Error is the single error type returned for every API failure.
type Error struct {
	// StatusCode is the HTTP status, or 0 when the request never reached the API.
	StatusCode int

	// Message is the most useful sentence the API gave us.
	Message string

	// Title and Detail come from the RFC 9457 problem body, when there was one.
	Title  string
	Detail string

	// Errors holds per-field messages on a 400. Field names are verbatim.
	Errors map[string][]string

	// RetryAfter is set on a 429, from the Retry-After header.
	RetryAfter time.Duration

	// Recipient is set when a send was refused because the address is
	// suppressed. It is the first entry of Suppressed when the API named any,
	// and otherwise the address the SDK sent.
	Recipient string

	// Suppressed holds every address the API refused, from the RFC 9457
	// extension member on a suppression refusal. Prefer it over parsing
	// Message: it is there precisely so clients need not read the prose.
	Suppressed []string

	// RequestID is the response's x-request-id. Quote it in support.
	RequestID string

	// Raw is the error body exactly as the API sent it, for anything this
	// struct does not model.
	Raw json.RawMessage

	kinds []error
}

func (e *Error) Error() string {
	if e.StatusCode == 0 {
		return "announcer: " + e.Message
	}
	return fmt.Sprintf("announcer: %s (HTTP %d)", e.Message, e.StatusCode)
}

// Unwrap yields the sentinel kinds (and the transport cause, for connection
// failures) so errors.Is works against any of them.
func (e *Error) Unwrap() []error { return e.kinds }

// problemBody covers all four error shapes the API produces: RFC 9457
// problem+json, the validation variant with an errors map, the bare
// {"error": "..."} used by two 409 paths, and — by way of every field being
// optional — the several 404 handlers that send no body at all.
type problemBody struct {
	Type   string              `json:"type"`
	Title  string              `json:"title"`
	Status int                 `json:"status"`
	Detail string              `json:"detail"`
	Errors map[string][]string `json:"errors"`
	Error  string              `json:"error"`
	// Extension member on a suppression refusal: the addresses refused.
	Suppressed []string `json:"suppressed"`
}

// genericMessage is the wording for a status that arrived with no usable body.
func genericMessage(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "the request was rejected as invalid"
	case http.StatusUnauthorized:
		return "invalid or revoked API key"
	case http.StatusForbidden:
		return "this API key is not permitted to perform that action"
	case http.StatusNotFound:
		return "not found"
	case http.StatusConflict:
		return "conflict"
	case http.StatusUnprocessableEntity:
		return "the request could not be processed"
	case http.StatusTooManyRequests:
		return "rate limit exceeded"
	default:
		return fmt.Sprintf("the API returned HTTP %d", status)
	}
}

// messageFor picks the most useful sentence out of whichever shape arrived.
// Order matters: detail is written for humans, errors is specific about which
// field is wrong, and title is boilerplate that only helps when nothing better
// exists.
func messageFor(status int, body *problemBody) string {
	if body != nil {
		if body.Detail != "" {
			return body.Detail
		}
		if body.Error != "" {
			return body.Error
		}
		if len(body.Errors) > 0 {
			for field, messages := range body.Errors {
				if len(messages) > 0 {
					return field + ": " + messages[0]
				}
			}
		}
		if body.Title != "" {
			return body.Title
		}
	}
	return genericMessage(status)
}

// newAPIError maps an unsuccessful response onto an *Error carrying the right
// sentinel kinds.
func newAPIError(status int, raw []byte, header http.Header, path, recipient string) *Error {
	var body *problemBody
	if len(raw) > 0 {
		var parsed problemBody
		if err := json.Unmarshal(raw, &parsed); err == nil {
			body = &parsed
		}
	}

	e := &Error{
		StatusCode: status,
		Message:    messageFor(status, body),
		RequestID:  header.Get("X-Request-Id"),
		Raw:        json.RawMessage(raw),
	}
	if body != nil {
		e.Title = body.Title
		e.Detail = body.Detail
		e.Errors = body.Errors
	}

	switch {
	case status == http.StatusBadRequest:
		e.kinds = []error{ErrValidation}
	case status == http.StatusUnauthorized:
		e.kinds = []error{ErrAuthentication}
	case status == http.StatusForbidden:
		e.kinds = []error{ErrPermission}
	case status == http.StatusNotFound:
		e.kinds = []error{ErrNotFound}
	case status == http.StatusConflict:
		e.kinds = []error{ErrConflict}
	case status == http.StatusUnprocessableEntity:
		// Only the send path can produce a suppression refusal; anything else
		// 422 is a plain unprocessable (domain limit, failed verification).
		if path == pathEmails {
			e.Recipient = recipient
			if body != nil && len(body.Suppressed) > 0 {
				e.Suppressed = body.Suppressed
				e.Recipient = body.Suppressed[0]
			}
			e.kinds = []error{ErrSuppressedRecipient, ErrUnprocessable}
		} else {
			e.kinds = []error{ErrUnprocessable}
		}
	case status == http.StatusTooManyRequests:
		e.RetryAfter = parseRetryAfter(header)
		e.kinds = []error{ErrRateLimit}
	case status >= 500:
		e.kinds = []error{ErrServer}
	}

	return e
}

// newConnectionError wraps a transport failure, keeping the cause reachable
// through errors.Is and errors.As.
func newConnectionError(message string, cause error) *Error {
	e := &Error{Message: message, kinds: []error{ErrConnection}}
	if cause != nil {
		e.kinds = append(e.kinds, cause)
	}
	return e
}

// parseRetryAfter reads Retry-After, which the API sends as whole seconds.
func parseRetryAfter(header http.Header) time.Duration {
	raw := header.Get("Retry-After")
	if raw == "" {
		return 0
	}
	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds * float64(time.Second))
}

package announcer

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// DefaultTolerance is how far apart a delivery's timestamp and our clock may
// be before VerifyWebhook rejects it as a possible replay.
const DefaultTolerance = 5 * time.Minute

// verifyConfig holds the tunable parts of VerifyWebhook.
type verifyConfig struct {
	tolerance time.Duration
	now       time.Time
}

// VerifyOption tunes VerifyWebhook.
type VerifyOption func(*verifyConfig)

// WithTolerance sets the clock-skew allowance. Zero disables the timestamp
// check entirely, which also disables replay protection — do that only if
// something else in your stack already provides it.
func WithTolerance(d time.Duration) VerifyOption {
	return func(c *verifyConfig) { c.tolerance = d }
}

// WithClock fixes the current time. For tests.
func WithClock(now time.Time) VerifyOption {
	return func(c *verifyConfig) { c.now = now }
}

// parseSignatureHeader reads "t=<unix>,v1=<hex>", ignoring schemes we do not
// know about so a future v2 cannot break v1 consumers.
func parseSignatureHeader(header string) (int64, string, error) {
	var (
		timestamp int64
		v1        string
		haveTS    bool
	)

	for _, part := range strings.Split(header, ",") {
		name, value, found := strings.Cut(part, "=")
		if !found {
			continue
		}
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		switch name {
		case "t":
			if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
				timestamp, haveTS = parsed, true
			}
		case "v1":
			v1 = value
		}
	}

	if !haveTS || v1 == "" {
		return 0, "", &Error{
			Message: fmt.Sprintf(
				`malformed X-Announcer-Signature header: expected "t=<unix>,v1=<hex>", got %q`, header),
			kinds: []error{ErrSignatureVerification},
		}
	}
	return timestamp, v1, nil
}

// VerifyWebhook checks a webhook delivery and returns the parsed event.
//
// Pass the raw request body — the exact bytes Announcer sent. Re-serialising a
// parsed struct reorders keys and changes whitespace, and the signature will
// not match:
//
//	func handler(w http.ResponseWriter, r *http.Request) {
//	    body, err := io.ReadAll(r.Body)
//	    if err != nil {
//	        http.Error(w, "", http.StatusBadRequest)
//	        return
//	    }
//	    event, err := announcer.VerifyWebhook(
//	        body,
//	        r.Header.Get("X-Announcer-Signature"),
//	        os.Getenv("ANNOUNCER_WEBHOOK_SECRET"),
//	    )
//	    if err != nil {
//	        http.Error(w, "", http.StatusBadRequest)
//	        return
//	    }
//	    ...
//	}
//
// Errors match ErrSignatureVerification. The timestamp is inside the MAC, so
// the staleness check is what actually stops a captured delivery being
// replayed at you later.
func VerifyWebhook(body []byte, signatureHeader, secret string, opts ...VerifyOption) (*WebhookEvent, error) {
	cfg := verifyConfig{tolerance: DefaultTolerance}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.now.IsZero() {
		cfg.now = time.Now()
	}

	if signatureHeader == "" {
		return nil, &Error{
			Message: "missing X-Announcer-Signature header",
			kinds:   []error{ErrSignatureVerification},
		}
	}
	if secret == "" {
		return nil, &Error{
			Message: "missing webhook signing secret",
			kinds:   []error{ErrSignatureVerification},
		}
	}

	timestamp, v1, err := parseSignatureHeader(signatureHeader)
	if err != nil {
		return nil, err
	}

	if cfg.tolerance > 0 {
		skew := math.Abs(float64(cfg.now.Unix() - timestamp))
		if skew > cfg.tolerance.Seconds() {
			return nil, &Error{
				Message: fmt.Sprintf(
					"webhook timestamp is outside the %s tolerance (signed at %d, now %d); "+
						"rejecting as a possible replay",
					cfg.tolerance, timestamp, cfg.now.Unix()),
				kinds: []error{ErrSignatureVerification},
			}
		}
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(expected), []byte(v1)) {
		return nil, &Error{
			Message: "webhook signature does not match; check that you are passing the raw " +
				"request body and the secret returned by Webhooks.Create",
			kinds: []error{ErrSignatureVerification},
		}
	}

	var event WebhookEvent
	if err := json.Unmarshal(body, &event); err != nil {
		return nil, &Error{
			Message: fmt.Sprintf("webhook body is not valid JSON: %v", err),
			kinds:   []error{ErrSignatureVerification},
		}
	}
	return &event, nil
}

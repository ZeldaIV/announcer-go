package announcer

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"time"
)

const pathEmails = "/v1/emails"

// Retry bounds. The cap on Retry-After stops a server (or a proxy) from
// parking a request for an hour inside a call the caller thinks is quick.
const (
	maxBackoff    = 8 * time.Second
	maxRetryAfter = 60 * time.Second
)

// requestSpec describes one API call.
type requestSpec struct {
	method  string
	path    string
	query   url.Values
	body    any
	headers map[string]string

	// retryOn409 treats 409 as retryable. Set for sends carrying an
	// Idempotency-Key, where a 409 means "the original attempt is still in
	// flight" rather than a real conflict — waiting is exactly right.
	retryOn409 bool

	// recipient is threaded through so a 422 on the send path can name the
	// address that was refused.
	recipient string
}

// shouldRetry reports whether a status is worth trying again. Everything else
// is the caller's problem, not a transient failure.
func shouldRetry(status int, retryOn409 bool) bool {
	switch {
	case status == http.StatusRequestTimeout, status == http.StatusTooManyRequests:
		return true
	case status == http.StatusConflict && retryOn409:
		return true
	default:
		return status >= 500
	}
}

// backoff is exponential with full jitter. The jitter is not decoration:
// without it, every client that hit the same rate limit retries in lockstep
// and hits it again together.
func backoff(attempt int) time.Duration {
	ceiling := time.Duration(1<<uint(attempt)) * 500 * time.Millisecond
	if ceiling > maxBackoff || ceiling <= 0 {
		ceiling = maxBackoff
	}
	return time.Duration(randFloat() * float64(ceiling))
}

// randFloat returns a value in [0,1). crypto/rand keeps this package free of
// math/rand's global state, which a library has no business touching.
func randFloat() float64 {
	const precision = 1 << 53
	n, err := rand.Int(rand.Reader, big.NewInt(precision))
	if err != nil {
		return 0.5
	}
	return float64(n.Int64()) / float64(precision)
}

// delayFor prefers the server's Retry-After: it knows when the per-tenant
// window actually rolls, and our guess does not.
func delayFor(resp *http.Response, attempt int) time.Duration {
	if d := parseRetryAfter(resp.Header); d > 0 {
		if d > maxRetryAfter {
			return maxRetryAfter
		}
		return d
	}
	return backoff(attempt)
}

// sleep waits, but gives up the moment the context is done.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// newIdempotencyKey returns a random 32-hex-character key. Generated when the
// caller supplies none, so the retry loop below can never double-send.
func newIdempotencyKey() string {
	buf := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		// crypto/rand does not fail in practice; a time-based fallback still
		// beats sending with no key at all.
		return fmt.Sprintf("ann-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

// do runs one request, retrying transient failures, and decodes the response
// into out. A nil out discards the body — right for the 204s.
func (c *Client) do(ctx context.Context, spec requestSpec, out any) error {
	endpoint := c.baseURL + spec.path
	if len(spec.query) > 0 {
		endpoint += "?" + spec.query.Encode()
	}

	// Marshalled once: the retry loop needs a fresh reader per attempt, and
	// re-encoding on every pass would be wasted work.
	var payload []byte
	if spec.body != nil {
		var err error
		payload, err = json.Marshal(spec.body)
		if err != nil {
			return fmt.Errorf("announcer: encoding request body: %w", err)
		}
	}

	var lastErr error

	for attempt := 0; ; attempt++ {
		var reader io.Reader
		if payload != nil {
			reader = bytes.NewReader(payload)
		}

		req, err := http.NewRequestWithContext(ctx, spec.method, endpoint, reader)
		if err != nil {
			return fmt.Errorf("announcer: building request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", c.userAgent)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		for name, value := range c.headers {
			req.Header.Set(name, value)
		}
		for name, value := range spec.headers {
			req.Header.Set(name, value)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			// The caller cancelling is not a transient failure.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = newConnectionError(
				fmt.Sprintf("could not reach the Announcer API at %s: %v", c.baseURL, err), err)
			if attempt < c.maxRetries {
				if werr := sleep(ctx, backoff(attempt)); werr != nil {
					return werr
				}
				continue
			}
			return lastErr
		}

		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if readErr != nil {
				return newConnectionError("reading the response body failed", readErr)
			}
			if out == nil || len(body) == 0 || resp.StatusCode == http.StatusNoContent {
				return nil
			}
			if err := json.Unmarshal(body, out); err != nil {
				return fmt.Errorf("announcer: decoding response: %w", err)
			}
			return nil
		}

		lastErr = newAPIError(resp.StatusCode, body, resp.Header, spec.path, spec.recipient)
		if shouldRetry(resp.StatusCode, spec.retryOn409) && attempt < c.maxRetries {
			if werr := sleep(ctx, delayFor(resp, attempt)); werr != nil {
				return werr
			}
			continue
		}
		return lastErr
	}
}

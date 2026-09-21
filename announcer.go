// Package announcer is the Go SDK for Announcer — transactional email sent
// from your own domain, DKIM-signed.
//
// Create one client and reuse it; it is safe for concurrent use and holds a
// connection pool.
//
//	client, err := announcer.New("") // reads ANNOUNCER_API_KEY
//	if err != nil {
//	    log.Fatal(err)
//	}
//
//	sent, err := client.Send(ctx, &announcer.SendEmailRequest{
//	    From:    "Acme <billing@acme.com>",
//	    To:      "customer@example.com",
//	    Subject: "Your receipt",
//	    Text:    "Thanks for your order.",
//	})
//
// Every send carries an Idempotency-Key, generated per call when you do not
// supply one, so the SDK's automatic retries can never send twice.
package announcer

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"
)

// DefaultBaseURL is where the hosted API lives.
const DefaultBaseURL = "https://mail.misralo.com"

// Version is this SDK's version, reported in the User-Agent.
const Version = "0.1.0"

// Client talks to the Announcer API. Build one with New.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	maxRetries int
	userAgent  string
	headers    map[string]string

	// Emails covers sending and send history. Works with either key scope.
	Emails *EmailsService

	// Domains covers registering and verifying sending domains. Needs a
	// full-scoped key.
	Domains *DomainsService

	// APIKeys covers issuing and revoking keys. Needs a full-scoped key.
	APIKeys *APIKeysService

	// Webhooks covers endpoints and delivery verification. Needs a
	// full-scoped key for the endpoint calls.
	Webhooks *WebhooksService

	// Suppressions lists addresses that hard-bounced or complained.
	Suppressions *SuppressionsService
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL points the client at a different API root, such as a mock
// server in tests.
func WithBaseURL(baseURL string) Option {
	return func(c *Client) { c.baseURL = strings.TrimRight(baseURL, "/") }
}

// WithHTTPClient supplies your own *http.Client (proxies, custom TLS,
// instrumentation). Its Timeout, if set, applies per attempt.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// WithTimeout sets the per-attempt timeout. Default 30s.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.httpClient.Timeout = d }
}

// WithMaxRetries sets how many extra attempts follow a retryable failure.
// Default 2, so three attempts in all. Zero disables retries.
func WithMaxRetries(n int) Option {
	return func(c *Client) {
		if n >= 0 {
			c.maxRetries = n
		}
	}
}

// WithUserAgent appends your application's name to the SDK's own User-Agent.
func WithUserAgent(ua string) Option {
	return func(c *Client) { c.userAgent = "announcer-go/" + Version + " " + ua }
}

// WithHeader adds a header to every request.
func WithHeader(name, value string) Option {
	return func(c *Client) { c.headers[name] = value }
}

// New builds a client.
//
// Pass an empty apiKey to read ANNOUNCER_API_KEY from the environment; the
// base URL likewise falls back to ANNOUNCER_BASE_URL and then DefaultBaseURL.
// It returns an error only when no key can be found.
func New(apiKey string, opts ...Option) (*Client, error) {
	if apiKey == "" {
		apiKey = os.Getenv("ANNOUNCER_API_KEY")
	}
	if apiKey == "" {
		return nil, errors.New(
			`announcer: no API key. Pass one to New("ann_...") or set ANNOUNCER_API_KEY`)
	}

	baseURL := os.Getenv("ANNOUNCER_BASE_URL")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}

	c := &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		maxRetries: 2,
		userAgent:  "announcer-go/" + Version,
		headers:    map[string]string{},
	}
	for _, opt := range opts {
		opt(c)
	}

	c.Emails = &EmailsService{client: c}
	c.Domains = &DomainsService{client: c}
	c.APIKeys = &APIKeysService{client: c}
	c.Webhooks = &WebhooksService{client: c}
	c.Suppressions = &SuppressionsService{client: c}
	return c, nil
}

// BaseURL reports the API root this client talks to.
func (c *Client) BaseURL() string { return c.baseURL }

// Send is shorthand for Emails.Send — the one call most integrations make.
func (c *Client) Send(ctx context.Context, req *SendEmailRequest) (*SentEmail, error) {
	return c.Emails.Send(ctx, req)
}

// Usage reports consumption against this account's limits, plus a 14-day
// sending series for charting.
func (c *Client) Usage(ctx context.Context) (*Usage, error) {
	var out Usage
	if err := c.do(ctx, requestSpec{method: http.MethodGet, path: "/v1/usage"}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

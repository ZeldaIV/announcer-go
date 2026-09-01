package announcer

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// DomainsService covers registering and verifying sending domains. Every call
// here needs a full-scoped key.
type DomainsService struct{ client *Client }

// Create registers a sending domain and returns the DNS record to publish.
//
// Registration generates an RSA keypair, so the API rate-limits it hard —
// roughly one a minute. Publish every record in CreatedDomain.DNS, then call
// Verify. A domain that is already registered fails with ErrConflict.
func (s *DomainsService) Create(ctx context.Context, domain string) (*CreatedDomain, error) {
	var out CreatedDomain
	err := s.client.do(ctx, requestSpec{
		method: http.MethodPost,
		path:   "/v1/domains",
		body:   map[string]string{"domain": domain},
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// List returns every domain on the account, newest first.
func (s *DomainsService) List(ctx context.Context) ([]Domain, error) {
	var out []Domain
	if err := s.client.do(ctx, requestSpec{method: http.MethodGet, path: "/v1/domains"}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DNS returns the records for a domain, re-derived from the stored public key
// — so "what was I supposed to publish?" stays answerable after registration.
func (s *DomainsService) DNS(ctx context.Context, domainID string) (*DomainDNS, error) {
	var out DomainDNS
	err := s.client.do(ctx, requestSpec{
		method: http.MethodGet,
		path:   "/v1/domains/" + url.PathEscape(domainID) + "/dns",
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Verify resolves the DKIM record and compares it to the key we issued.
//
// This is a real DNS lookup, not a self-report: it only succeeds on an actual
// match. A missing or mismatched record fails with ErrUnprocessable. DNS
// propagation takes minutes to hours — retry rather than re-registering.
func (s *DomainsService) Verify(ctx context.Context, domainID string) (*DomainVerification, error) {
	var out DomainVerification
	err := s.client.do(ctx, requestSpec{
		method: http.MethodPost,
		path:   "/v1/domains/" + url.PathEscape(domainID) + "/verify",
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Delete removes the domain and its signing key. Send history survives;
// sending from the domain stops immediately.
func (s *DomainsService) Delete(ctx context.Context, domainID string) error {
	return s.client.do(ctx, requestSpec{
		method: http.MethodDelete,
		path:   "/v1/domains/" + url.PathEscape(domainID),
	}, nil)
}

// APIKeysService covers issuing and revoking API keys. Needs a full-scoped key.
type APIKeysService struct{ client *Client }

// Create issues a key. The secret is in the response and nowhere else — the
// API stores only its hash.
//
// Pass ScopeSend for anything that only sends mail: a leaked send key cannot
// register domains, mint more keys, or reach billing. An empty scope means
// ScopeFull.
func (s *APIKeysService) Create(ctx context.Context, name, scope string) (*CreatedAPIKey, error) {
	if scope == "" {
		scope = ScopeFull
	}
	var out CreatedAPIKey
	err := s.client.do(ctx, requestSpec{
		method: http.MethodPost,
		path:   "/v1/keys",
		body:   map[string]string{"name": name, "scope": scope},
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// List returns every key on the account. Secrets are never included.
func (s *APIKeysService) List(ctx context.Context) ([]APIKey, error) {
	var out []APIKey
	if err := s.client.do(ctx, requestSpec{method: http.MethodGet, path: "/v1/keys"}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Revoke revokes a key. The row stays, so LastUsedAt remains auditable.
func (s *APIKeysService) Revoke(ctx context.Context, keyID string) error {
	return s.client.do(ctx, requestSpec{
		method: http.MethodDelete,
		path:   "/v1/keys/" + url.PathEscape(keyID),
	}, nil)
}

// WebhooksService covers webhook endpoints. Registering needs a full-scoped
// key; verifying a delivery needs nothing but the secret.
type WebhooksService struct{ client *Client }

// Create registers an endpoint. Maximum two active per account.
//
// The returned Secret crosses the wire exactly once — store it now and pass it
// to VerifyWebhook on every delivery.
func (s *WebhooksService) Create(ctx context.Context, endpointURL string) (*CreatedWebhookEndpoint, error) {
	var out CreatedWebhookEndpoint
	err := s.client.do(ctx, requestSpec{
		method: http.MethodPost,
		path:   "/v1/webhooks",
		body:   map[string]string{"url": endpointURL},
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// List returns every endpoint on the account, including disabled ones.
func (s *WebhooksService) List(ctx context.Context) ([]WebhookEndpoint, error) {
	var out []WebhookEndpoint
	if err := s.client.do(ctx, requestSpec{method: http.MethodGet, path: "/v1/webhooks"}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Delete disables an endpoint. Pending deliveries stop; history stays auditable.
func (s *WebhooksService) Delete(ctx context.Context, endpointID string) error {
	return s.client.do(ctx, requestSpec{
		method: http.MethodDelete,
		path:   "/v1/webhooks/" + url.PathEscape(endpointID),
	}, nil)
}

// Verify checks a delivery's signature and returns the parsed event. It is a
// method for discoverability; VerifyWebhook is the same function and needs no
// client.
func (s *WebhooksService) Verify(body []byte, signatureHeader, secret string, opts ...VerifyOption) (*WebhookEvent, error) {
	return VerifyWebhook(body, signatureHeader, secret, opts...)
}

// SuppressionsService lists addresses that hard-bounced or complained. Works
// with either key scope.
type SuppressionsService struct{ client *Client }

// List returns addresses Announcer refuses to send to, newest first. A limit
// of zero asks for the API's default of 50.
func (s *SuppressionsService) List(ctx context.Context, limit int) ([]Suppression, error) {
	query := url.Values{}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}

	var out []Suppression
	err := s.client.do(ctx, requestSpec{
		method: http.MethodGet,
		path:   "/v1/suppressions",
		query:  query,
	}, &out)
	if err != nil {
		return nil, err
	}
	return out, nil
}

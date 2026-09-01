package announcer

import "time"

// Message statuses. bounced, failed and suppressed are terminal.
const (
	StatusQueued     = "queued"
	StatusSent       = "sent"
	StatusDelivered  = "delivered"
	StatusBounced    = "bounced"
	StatusComplained = "complained"
	StatusFailed     = "failed"
	StatusSuppressed = "suppressed"
)

// Webhook event types — the transitions that produce a delivery.
const (
	EventSent       = "sent"
	EventDelivered  = "delivered"
	EventBounced    = "bounced"
	EventComplained = "complained"
	EventSuppressed = "suppressed"
)

// API key scopes. Give integrations ScopeSend: a leaked send key cannot
// register domains, mint successor keys, or reach billing.
const (
	ScopeFull = "full"
	ScopeSend = "send"
)

// Addresses is one or more email addresses.
//
// Its underlying type is []string, so a slice literal assigns directly:
//
//	To: []string{"a@example.com", "b@example.com"},
//
// For the common single-address case, [Address] reads better:
//
//	To: announcer.Address("a@example.com"),
type Addresses []string

// Address wraps one address, for the common case.
func Address(address string) Addresses { return Addresses{address} }

// AddressList wraps several addresses.
func AddressList(addresses ...string) Addresses { return addresses }

// First returns the first address, or "" when there are none.
func (a Addresses) First() string {
	if len(a) == 0 {
		return ""
	}
	return a[0]
}

// SendEmailRequest is one email to send.
//
// Every address accepts either a bare address (billing@acme.com) or a display
// name (Acme Billing <billing@acme.com>). At least one of Text or HTML is
// required, and at most 50 addresses across To, Cc and Bcc combined.
//
// To and Cc go out as one email whose recipients see each other; Bcc
// recipients see nobody. For separate emails that share nothing, use
// EmailsService.SendMany.
type SendEmailRequest struct {
	// From is the sender. Its domain must be registered to this account.
	From string `json:"from"`

	// To holds the primary recipients. At least one is required.
	To Addresses `json:"to"`

	// Cc holds carbon copies, visible to every other recipient.
	Cc Addresses `json:"cc,omitempty"`

	// Bcc holds blind copies. They receive the message; nobody — including
	// the other blind copies — sees that they did.
	Bcc Addresses `json:"bcc,omitempty"`

	// ReplyTo is where replies should go. A header only: no delivery, nothing
	// billable, nothing that can bounce.
	ReplyTo Addresses `json:"reply_to,omitempty"`

	Subject string `json:"subject,omitempty"`

	// Text is the plain-text body. Supply it even alongside HTML — filters
	// like seeing both.
	Text string `json:"text,omitempty"`

	// HTML is the HTML body.
	HTML string `json:"html,omitempty"`

	// IdempotencyKey makes the send exactly-once. Left empty, the SDK
	// generates one per call so its own retries cannot double-send; set it
	// yourself (an order id, a job id) to keep that guarantee across process
	// restarts. Not serialised — it travels as a header.
	IdempotencyKey string `json:"-"`
}

// SentEmail is the result of a successful send.
type SentEmail struct {
	// ID is Announcer's id for the message. Use it with EmailsService.Events.
	ID string `json:"id"`

	// MessageID is the RFC 5322 Message-ID the MTA assigned.
	MessageID *string `json:"messageId"`

	Status string `json:"status"`

	// IdempotentReplay is true when this key had already been used: nothing
	// was sent a second time and these are the original send's details.
	IdempotentReplay bool `json:"idempotentReplay"`

	// Recipients is how many addresses the message went to, across To, Cc and
	// Bcc. This is the number billed and counted against quota.
	Recipients int `json:"recipients"`

	// Suppressed holds addresses dropped because they are on the account's
	// suppression list. Empty on a clean send — the rest of the message still
	// went out. Only when every recipient is suppressed does the send fail.
	Suppressed []string `json:"suppressed"`
}

// Message is one row of send history.
//
// The json tags are the API's own names; the Go field names are the ones the
// rest of this package uses. header_from and recipient are the database's
// vocabulary, while the webhook payload already says from/to — one vocabulary
// is worth the mapping.
type Message struct {
	ID        string  `json:"id"`
	MessageID *string `json:"message_id"`
	From      string  `json:"header_from"`

	// To is the primary recipient — the first To address. A message with Cc,
	// Bcc or several To addresses reports its first here and the total in
	// RecipientCount.
	To string `json:"recipient"`

	// RecipientCount is how many addresses the message went to, across To, Cc
	// and Bcc.
	RecipientCount int `json:"recipient_count"`

	// ReplyTo is the Reply-To header that went out, if any.
	ReplyTo *string `json:"reply_to"`

	Subject *string `json:"subject"`

	// Status is the rolled-up status. One bounced recipient makes the whole
	// message bounced — it is the thing you have to act on.
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

// ListMessagesOptions filters send history. Zero values are omitted.
type ListMessagesOptions struct {
	// Limit is clamped to 1-200 by the API. Zero means the API default of 50.
	Limit int

	// Status filters by one of the Status* constants.
	Status string

	// Search matches a substring of the recipient address.
	Search string
}

// MessageEvent is one entry in a message's audit trail.
type MessageEvent struct {
	ID    int64  `json:"id"`
	Event string `json:"event"`

	// Payload is event-specific data, left exactly as the API sent it — these
	// keys are tenant data.
	Payload map[string]any `json:"payload"`

	CreatedAt time.Time `json:"created_at"`
}

// DNSRecord is a record the domain owner has to publish.
type DNSRecord struct {
	Type string `json:"type"`

	// Name is the record name, e.g. mail._domainkey.acme.com.
	Name string `json:"name"`

	// Value is the record value, e.g. v=DKIM1; k=rsa; p=MIIBIjANBg...
	Value string `json:"value"`

	// Purpose is why the record exists, in a sentence you can show a user.
	Purpose string `json:"purpose"`
}

// Domain is a sending domain, as returned by DomainsService.List.
type Domain struct {
	ID     string `json:"id"`
	Domain string `json:"domain"`

	// Selector is the DKIM selector. Always "mail"; the API ignores
	// client-supplied selectors.
	Selector string `json:"selector"`

	// VerifiedAt is when verification first succeeded, or nil.
	VerifiedAt *time.Time `json:"verified_at"`

	CreatedAt time.Time `json:"created_at"`
}

// Verified reports whether the DKIM record has been seen in DNS and matched.
func (d Domain) Verified() bool { return d.VerifiedAt != nil }

// CreatedDomain is a freshly registered domain, including the record to publish.
type CreatedDomain struct {
	ID       string `json:"id"`
	Domain   string `json:"domain"`
	Selector string `json:"selector"`
	Verified bool   `json:"verified"`

	// DNS holds every record to publish. Publish them all, then call Verify.
	DNS []DNSRecord `json:"dns"`
}

// DomainDNS is the record set for an existing domain, re-derived from the
// stored public key.
type DomainDNS struct {
	ID       string      `json:"id"`
	Domain   string      `json:"domain"`
	Verified bool        `json:"verified"`
	DNS      []DNSRecord `json:"dns"`
}

// DomainVerification is the outcome of a verification attempt.
type DomainVerification struct {
	ID       string `json:"id"`
	Verified bool   `json:"verified"`
}

// APIKey is a key, minus the secret.
type APIKey struct {
	ID   string `json:"id"`
	Name string `json:"name"`

	// Prefix is the first few characters, for telling keys apart in a list.
	Prefix string `json:"prefix"`

	Scope      string     `json:"scope"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
}

// Revoked reports whether the key has been revoked.
func (k APIKey) Revoked() bool { return k.RevokedAt != nil }

// CreatedAPIKey is a newly minted key. Key is shown once and never again —
// the API stores only its hash.
type CreatedAPIKey struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Prefix string `json:"prefix"`
	Scope  string `json:"scope"`

	// Key is the full secret. Only ever present here.
	Key string `json:"key"`
}

// WebhookEndpoint is a registered endpoint.
type WebhookEndpoint struct {
	ID         string     `json:"id"`
	URL        string     `json:"url"`
	CreatedAt  time.Time  `json:"created_at"`
	DisabledAt *time.Time `json:"disabled_at"`
}

// Disabled reports whether the endpoint has been disabled.
func (w WebhookEndpoint) Disabled() bool { return w.DisabledAt != nil }

// CreatedWebhookEndpoint is a newly registered endpoint. Secret is shown once.
type CreatedWebhookEndpoint struct {
	ID  string `json:"id"`
	URL string `json:"url"`

	// Secret is the whsec_... value to pass to VerifyWebhook.
	Secret string `json:"secret"`
}

// Suppression is an address Announcer refuses to send to.
type Suppression struct {
	ID    string `json:"id"`
	Email string `json:"email"`

	// Reason is why it was suppressed, e.g. "hard bounce (5.1.1 user unknown)".
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}

// UsagePoint is one day of the 14-day sending series.
type UsagePoint struct {
	// Date is YYYY-MM-DD.
	Date      string `json:"date"`
	Sent      int64  `json:"sent"`
	Delivered int64  `json:"delivered"`

	// Failed is bounced + complained + failed. A subset of Sent, not
	// additional to it.
	Failed int64 `json:"failed"`
}

// Usage is consumption against this account's limits.
type Usage struct {
	SentToday      int64  `json:"sentToday"`
	DailySendLimit int    `json:"dailySendLimit"`
	Domains        int64  `json:"domains"`
	MaxDomains     int    `json:"maxDomains"`
	Plan           string `json:"plan"`
	SentThisPeriod int64  `json:"sentThisPeriod"`

	PeriodStart time.Time `json:"periodStart"`

	// MonthlyIncludedMessages is nil on plans with no monthly accounting.
	MonthlyIncludedMessages *int `json:"monthlyIncludedMessages"`

	// MonthlyHardCap is the spend ceiling. Nil when uncapped.
	MonthlyHardCap *int `json:"monthlyHardCap"`

	// OverageMinorUnits is what the current period's overage would cost.
	OverageMinorUnits *int64 `json:"overageMinorUnits"`

	// Series is the last 14 days, oldest first. Days with no sends are
	// present as zeroes rather than missing.
	Series []UsagePoint `json:"series"`
}

// WebhookEventMessage is the message summary carried on a webhook delivery.
type WebhookEventMessage struct {
	ID      string  `json:"id"`
	From    string  `json:"from"`
	To      string  `json:"to"`
	Subject *string `json:"subject"`
	Status  string  `json:"status"`
}

// WebhookEvent is a verified webhook delivery.
type WebhookEvent struct {
	// Event is one of the Event* constants.
	Event      string              `json:"event"`
	OccurredAt time.Time           `json:"occurredAt"`
	Message    WebhookEventMessage `json:"message"`

	// Detail is event-specific data, e.g. {"dsnStatus": "5.1.1 ..."} on a bounce.
	Detail map[string]any `json:"detail"`
}

// BatchSendResult is one recipient's outcome from EmailsService.SendMany.
type BatchSendResult struct {
	To string

	// Result is set when Err is nil.
	Result *SentEmail

	// Err is set when the send failed. Inspect it with errors.Is/errors.As.
	Err error
}

// OK reports whether this recipient's send succeeded.
func (r BatchSendResult) OK() bool { return r.Err == nil }

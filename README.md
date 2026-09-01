# announcer-go

Go SDK for [Announcer](https://misralo.com) — send transactional email from
your own domain, DKIM-signed.

No dependencies outside the standard library.

```bash
go get github.com/ZeldaIV/announcer-go
```

## Send an email

```go
package main

import (
	"context"
	"log"

	"github.com/ZeldaIV/announcer-go"
)

func main() {
	client, err := announcer.New("") // reads ANNOUNCER_API_KEY
	if err != nil {
		log.Fatal(err)
	}

	sent, err := client.Send(context.Background(), &announcer.SendEmailRequest{
		From:    "Acme <billing@acme.com>",
		To:      announcer.Address("customer@example.com"),
		Subject: "Your receipt",
		Text:    "Thanks for your order.",
		HTML:    "<p>Thanks for your order.</p>",
	})
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("sent %s", sent.ID)
}
```

That's the whole integration. Pass the key to `New` explicitly if you would
rather not use the environment.

`To`, `Cc` and `Bcc` are `announcer.Addresses`, whose underlying type is
`[]string` — so `announcer.Address("one@example.com")` and
`[]string{"a@example.com", "b@example.com"}` both assign directly. See
[Several recipients](#several-recipients).

## Before your first send

You need a registered, verified sending domain — Announcer will not let you
send `From:` a domain you have not proved you control.

```go
domain, err := client.Domains.Create(ctx, "acme.com")
if err != nil {
	log.Fatal(err)
}

for _, r := range domain.DNS {
	fmt.Printf("%s  %s  %s\n", r.Type, r.Name, r.Value)
}
// TXT  mail._domainkey.acme.com  v=DKIM1; k=rsa; p=MIIBIjANBg...

// Publish that record, wait for DNS, then:
_, err = client.Domains.Verify(ctx, domain.ID)
```

One TXT record is the entire ask. SPF and MX stay on Announcer's own bounce
domain, so your root domain's DNS is untouched.

## What the SDK does for you

**Retries are safe.** Every send carries an `Idempotency-Key`, generated per
call when you leave `IdempotencyKey` empty. A timeout, a 500, or a 429 gets
retried with exponential backoff and jitter — and because the key travels with
the retry, the API recognises it as the same operation instead of sending
twice. Retries respect your `context.Context`: cancel it and the loop stops.

Set the key yourself to extend that guarantee across process restarts:

```go
sent, err := client.Send(ctx, &announcer.SendEmailRequest{
	From:           "billing@acme.com",
	To:             "customer@example.com",
	Subject:        "Your receipt",
	Text:           "Thanks!",
	IdempotencyKey: fmt.Sprintf("receipt-%d", order.ID), // mails exactly once, ever
})
if err != nil {
	log.Fatal(err)
}
if sent.IdempotentReplay {
	// Already sent earlier. Nothing went out a second time.
}
```

**Errors work the way Go errors work.** Branch with `errors.Is`, reach the
details with `errors.As`:

```go
sent, err := client.Send(ctx, msg)
switch {
case err == nil:
	// ...
case errors.Is(err, announcer.ErrSuppressedRecipient):
	// They hard-bounced or complained before. Don't retry; mark them inactive.
	deactivate(msg.To)
case errors.Is(err, announcer.ErrRateLimit):
	var apiErr *announcer.Error
	errors.As(err, &apiErr)
	time.Sleep(apiErr.RetryAfter)
case errors.Is(err, announcer.ErrPermission):
	// Domain not registered, not verified, or this key is send-scoped.
default:
	return err
}
```

The full set: `ErrValidation` (see `Error.Errors` for the per-field messages),
`ErrAuthentication`, `ErrPermission`, `ErrNotFound`, `ErrConflict`,
`ErrUnprocessable`, `ErrSuppressedRecipient`, `ErrRateLimit`, `ErrServer`,
`ErrConnection`, `ErrSignatureVerification`. A suppressed recipient matches
both `ErrSuppressedRecipient` and the broader `ErrUnprocessable`.

**Field names make sense.** The API calls them `header_from` and `recipient`;
the SDK calls them `From` and `To`, matching what you used to send and what the
webhook payload says. Timestamps are `time.Time`, nullable ones are
`*time.Time`, and the booleans you want are methods: `domain.Verified()`,
`key.Revoked()`, `endpoint.Disabled()`.

## Several recipients

`To`, `Cc` and `Bcc` each take one address or many. Everything in `To` and `Cc`
is **one email** whose recipients see each other; `Bcc` recipients see nobody,
not even each other:

```go
sent, err := client.Send(ctx, &announcer.SendEmailRequest{
	From:    "billing@acme.com",
	To:      []string{"customer@example.com", "partner@example.com"},
	Cc:      announcer.Address("accounting@acme.com"),
	Bcc:     announcer.Address("archive@acme.com"),
	ReplyTo: announcer.Address("support@acme.com"),
	Subject: "Your receipt",
	Text:    "Thanks!",
})
```

At most 50 addresses across the three. `ReplyTo` is a header only — it costs
nothing and cannot bounce.

**Recipients are the billable unit.** That call counts four against your quota,
not one. It is also what keeps `MonthlyHardCap` meaningful: otherwise a leaked
key could send fifty times your ceiling by padding the slice.

### One email, or many?

For anything list-shaped — a newsletter, a digest, a fan-out — you want
`SendMany`, not a slice:

```go
results, err := client.Emails.SendMany(ctx,
	[]string{"a@example.com", "b@example.com", "c@example.com"},
	&announcer.SendEmailRequest{
		From:    "news@acme.com",
		Subject: "September update",
		HTML:    body,
	},
	nil, // or &announcer.SendManyOptions{StopOnError: true}
)

for _, r := range results {
	if !r.OK() {
		log.Printf("%s failed: %v", r.To, r.Err)
	}
}
```

|  | `To: []string{a, b}` | `SendMany([]string{a, b}, …)` |
|---|---|---|
| Emails sent | one | two |
| Do they see each other? | yes, in `To:` | no |
| API requests | one | two |
| Idempotency key | one | one each, derived |
| One address fails | the send reports it | the others are unaffected |

Each `SendMany` recipient gets its own derived idempotency key, and your
request struct is left untouched.

### Suppressed recipients

A recipient on your suppression list is dropped and the rest still goes out:

```go
sent, err := client.Send(ctx, &announcer.SendEmailRequest{
	From:    "billing@acme.com",
	To:      []string{"good@example.com", "bounced-before@example.com"},
	Subject: "Your receipt",
	Text:    "Thanks!",
})

sent.Recipients // 1 — what actually went out and what you were billed
sent.Suppressed // ["bounced-before@example.com"]
```

`ErrSuppressedRecipient` is returned only when *every* recipient is suppressed
(or every `To` recipient — a message with no visible primary recipient is
refused rather than sent). `Error.Suppressed` names them all.

## Webhooks

Register an endpoint, store the secret, verify every delivery:

```go
endpoint, err := client.Webhooks.Create(ctx, "https://acme.com/hooks/announcer")
log.Println(endpoint.Secret) // whsec_... — shown once, store it now
```

```go
func announcerWebhook(w http.ResponseWriter, r *http.Request) {
	// The raw bytes are what was signed. Decoding and re-encoding reorders
	// keys and the signature will not match.
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "", http.StatusBadRequest)
		return
	}

	event, err := announcer.VerifyWebhook(
		body,
		r.Header.Get("X-Announcer-Signature"),
		os.Getenv("ANNOUNCER_WEBHOOK_SECRET"),
	)
	if err != nil {
		http.Error(w, "", http.StatusBadRequest)
		return
	}

	switch event.Event {
	case announcer.EventDelivered:
		markDelivered(event.Message.ID)
	case announcer.EventBounced, announcer.EventComplained:
		deactivate(event.Message.To)
	}

	w.WriteHeader(http.StatusOK)
}
```

Verification checks the HMAC **and** the timestamp, rejecting anything more
than five minutes old so a captured delivery cannot be replayed at you. Tune it
with `announcer.WithTolerance(d)`.

Events: `EventSent`, `EventDelivered`, `EventBounced`, `EventComplained`,
`EventSuppressed`.

## API reference

### Client

```go
func New(apiKey string, opts ...Option) (*Client, error)
```

| Option | Default |
|--------|---------|
| `WithBaseURL(url)` | `ANNOUNCER_BASE_URL`, then `https://mail.misralo.com` |
| `WithTimeout(d)` | 30s, per attempt |
| `WithMaxRetries(n)` | 2 extra attempts |
| `WithHTTPClient(hc)` | `&http.Client{Timeout: 30s}` |
| `WithUserAgent(ua)` | appended to the SDK's own — name your app |
| `WithHeader(k, v)` | — |

An empty `apiKey` reads `ANNOUNCER_API_KEY`. `New` errors only when no key is
found anywhere.

### Methods

Every call takes a `context.Context` first.

| Call | Does |
|------|------|
| `client.Send(ctx, msg)` | Shorthand for `Emails.Send`. |
| `client.Usage(ctx)` | Quota consumption plus a 14-day sending series. |
| `Emails.Send(ctx, msg)` | Sends one email. `To`/`Cc`/`Bcc` take one address or many. |
| `Emails.SendMany(ctx, to, msg, opts)` | Separate emails, one per recipient. |
| `Emails.List(ctx, opts)` | Send history. |
| `Emails.Events(ctx, id)` | A message's audit trail. |
| `Domains.Create(ctx, domain)` | Registers a domain, returns the DNS record. |
| `Domains.List(ctx)` | Every domain on the account. |
| `Domains.DNS(ctx, id)` | The records again, for a domain you already registered. |
| `Domains.Verify(ctx, id)` | Resolves DNS and checks the published key. |
| `Domains.Delete(ctx, id)` | Removes the domain and its signing key. |
| `APIKeys.Create(ctx, name, scope)` | Issues a key. Secret shown once. |
| `APIKeys.List(ctx)` | Every key, without secrets. |
| `APIKeys.Revoke(ctx, id)` | Revokes a key; history survives. |
| `Webhooks.Create(ctx, url)` | Registers an endpoint. Max 2 active. |
| `Webhooks.List(ctx)` | Every endpoint. |
| `Webhooks.Delete(ctx, id)` | Disables an endpoint. |
| `VerifyWebhook(body, header, secret, ...)` | Verifies a delivery. |
| `Suppressions.List(ctx, limit)` | Addresses that bounced or complained. |

`Domains`, `APIKeys` and `Webhooks` need a `full`-scoped key. Everything else
works with a `send` key too — give integrations `send`.

## Scopes

Mint a `send`-scoped key for anything that only sends mail:

```go
key, err := client.APIKeys.Create(ctx, "production-worker", announcer.ScopeSend)
```

A leaked send key cannot register domains, mint successor keys, or touch
billing. It is the difference between an incident and a catastrophe.

## Local development

Point the SDK at a local Announcer stack:

```go
client, err := announcer.New(
	"ann_dev_0000000000000000000000000000",
	announcer.WithBaseURL("http://localhost:8080"),
)
```

## Contributing

```bash
go test ./...   # no network; everything runs against httptest
go vet ./...
gofmt -l .
```

## License

MIT

package announcer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubResponse is one canned reply in a queue.
type stubResponse struct {
	status  int
	body    string
	headers map[string]string
}

// recordedCall is what the client actually sent.
type recordedCall struct {
	method   string
	path     string
	rawQuery string
	headers  http.Header
	body     []byte
}

// recorder is a test server that plays back a queue and records every request.
type recorder struct {
	mu        sync.Mutex
	responses []stubResponse
	calls     []recordedCall
	server    *httptest.Server
}

func newRecorder(t *testing.T, responses ...stubResponse) *recorder {
	t.Helper()
	r := &recorder{responses: responses}
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)

		r.mu.Lock()
		r.calls = append(r.calls, recordedCall{
			method:   req.Method,
			path:     req.URL.Path,
			rawQuery: req.URL.RawQuery,
			headers:  req.Header.Clone(),
			body:     body,
		})
		var next stubResponse
		if len(r.responses) == 0 {
			r.mu.Unlock()
			http.Error(w, "unexpected extra request", http.StatusTeapot)
			return
		}
		next, r.responses = r.responses[0], r.responses[1:]
		r.mu.Unlock()

		for k, v := range next.headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		status := next.status
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		if next.body != "" {
			io.WriteString(w, next.body)
		}
	}))
	t.Cleanup(r.server.Close)
	return r
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *recorder) call(i int) recordedCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[i]
}

// testClient wires a client to the recorder, with retries off by default.
func testClient(t *testing.T, rec *recorder, opts ...Option) *Client {
	t.Helper()
	base := []Option{WithBaseURL(rec.server.URL), WithMaxRetries(0)}
	client, err := New("ann_test_key", append(base, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

func decodeBody(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("request body was not JSON: %v (%s)", err, raw)
	}
	return out
}

// -- construction ---------------------------------------------------------

func TestNewReadsTheEnvironment(t *testing.T) {
	t.Setenv("ANNOUNCER_API_KEY", "ann_from_env")
	t.Setenv("ANNOUNCER_BASE_URL", "http://localhost:8080")

	client, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if client.BaseURL() != "http://localhost:8080" {
		t.Errorf("BaseURL = %q", client.BaseURL())
	}
}

func TestNewWithoutAKeyExplainsItself(t *testing.T) {
	t.Setenv("ANNOUNCER_API_KEY", "")

	_, err := New("")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "ANNOUNCER_API_KEY") {
		t.Errorf("error should name the environment variable, got %q", err)
	}
}

func TestBaseURLTrailingSlashIsTrimmed(t *testing.T) {
	client, err := New("ann_k", WithBaseURL("https://api.example.test/"))
	if err != nil {
		t.Fatal(err)
	}
	if client.BaseURL() != "https://api.example.test" {
		t.Errorf("BaseURL = %q", client.BaseURL())
	}
}

func TestUserAgentNamesTheCaller(t *testing.T) {
	rec := newRecorder(t, stubResponse{body: `{"sentToday":0}`})
	client := testClient(t, rec, WithUserAgent("acme-billing/2.1"))

	if _, err := client.Usage(context.Background()); err != nil {
		t.Fatal(err)
	}

	ua := rec.call(0).headers.Get("User-Agent")
	if !strings.HasPrefix(ua, "announcer-go/") || !strings.HasSuffix(ua, "acme-billing/2.1") {
		t.Errorf("User-Agent = %q", ua)
	}
}

// -- sending --------------------------------------------------------------

func TestSend(t *testing.T) {
	rec := newRecorder(t, stubResponse{
		body: `{"id":"msg-1","messageId":"<abc@acme.test>","status":"sent"}`,
	})
	client := testClient(t, rec)

	sent, err := client.Send(context.Background(), &SendEmailRequest{
		From:    "Acme <billing@acme.test>",
		To:      Address("customer@example.com"),
		Subject: "Your receipt",
		Text:    "Thanks!",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	call := rec.call(0)
	if call.method != http.MethodPost || call.path != "/v1/emails" {
		t.Errorf("got %s %s", call.method, call.path)
	}
	if got := call.headers.Get("Authorization"); got != "Bearer ann_test_key" {
		t.Errorf("Authorization = %q", got)
	}
	body := decodeBody(t, call.body)
	if body["from"] != "Acme <billing@acme.test>" {
		t.Errorf("body = %v", body)
	}
	if got := body["to"]; !reflect.DeepEqual(got, []any{"customer@example.com"}) {
		t.Errorf("to = %#v", got)
	}
	// Headers the caller left unset must not appear at all.
	for _, absent := range []string{"cc", "bcc", "reply_to"} {
		if _, present := body[absent]; present {
			t.Errorf("%s should be omitted when unset", absent)
		}
	}
	// IdempotencyKey is json:"-" and must not leak into the payload.
	if _, present := body["IdempotencyKey"]; present {
		t.Error("IdempotencyKey should travel as a header, not in the body")
	}

	if sent.ID != "msg-1" || sent.MessageID == nil || *sent.MessageID != "<abc@acme.test>" {
		t.Errorf("sent = %+v", sent)
	}
	if sent.IdempotentReplay {
		t.Error("IdempotentReplay should be false")
	}
}

func TestSendGeneratesAnIdempotencyKey(t *testing.T) {
	rec := newRecorder(t, stubResponse{body: `{"id":"m","status":"sent"}`})
	client := testClient(t, rec)

	if _, err := client.Send(context.Background(), &SendEmailRequest{
		From: "a@acme.test", To: Address("b@example.com"), Text: "hi",
	}); err != nil {
		t.Fatal(err)
	}

	if key := rec.call(0).headers.Get("Idempotency-Key"); len(key) != 32 {
		t.Errorf("expected a generated 32-hex key, got %q", key)
	}
}

func TestSendPassesASuppliedKeyThrough(t *testing.T) {
	rec := newRecorder(t, stubResponse{body: `{"id":"m","status":"sent"}`})
	client := testClient(t, rec)

	if _, err := client.Send(context.Background(), &SendEmailRequest{
		From: "a@acme.test", To: Address("b@example.com"), Text: "hi", IdempotencyKey: "order-4711",
	}); err != nil {
		t.Fatal(err)
	}

	if key := rec.call(0).headers.Get("Idempotency-Key"); key != "order-4711" {
		t.Errorf("Idempotency-Key = %q", key)
	}
}

func TestSendReportsAnIdempotentReplay(t *testing.T) {
	rec := newRecorder(t, stubResponse{
		body: `{"id":"m","messageId":"<x@a.test>","status":"sent","idempotentReplay":true}`,
	})
	client := testClient(t, rec)

	sent, err := client.Send(context.Background(), &SendEmailRequest{
		From: "a@acme.test", To: Address("b@example.com"), Text: "hi", IdempotencyKey: "k",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sent.IdempotentReplay {
		t.Error("expected IdempotentReplay")
	}
}

func TestSendValidatesBeforeSpendingACall(t *testing.T) {
	cases := map[string]*SendEmailRequest{
		"no from": {To: Address("b@example.com"), Text: "hi"},
		"no to":   {From: "a@acme.test", Text: "hi"},
		"no body": {From: "a@acme.test", To: Address("b@example.com"), Subject: "empty"},
		"nil":     nil,
	}

	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			rec := newRecorder(t)
			client := testClient(t, rec)

			if _, err := client.Send(context.Background(), req); err == nil {
				t.Fatal("expected a validation error")
			}
			if rec.count() != 0 {
				t.Errorf("should not have called the API, made %d calls", rec.count())
			}
		})
	}
}

func TestSendMany(t *testing.T) {
	rec := newRecorder(t,
		stubResponse{body: `{"id":"m1","status":"sent"}`},
		stubResponse{status: 422, body: `{"status":422,"detail":"b@example.com is on your suppression list."}`},
		stubResponse{body: `{"id":"m3","status":"sent"}`},
	)
	client := testClient(t, rec)

	results, err := client.Emails.SendMany(context.Background(),
		[]string{"a@example.com", "b@example.com", "c@example.com"},
		&SendEmailRequest{From: "billing@acme.test", Subject: "Notice", Text: "hi"},
		nil,
	)
	if err != nil {
		t.Fatalf("SendMany: %v", err)
	}

	if len(results) != 3 || rec.count() != 3 {
		t.Fatalf("got %d results from %d calls", len(results), rec.count())
	}
	if !results[0].OK() || results[1].OK() || !results[2].OK() {
		t.Errorf("outcomes = %v %v %v", results[0].OK(), results[1].OK(), results[2].OK())
	}
	if !errors.Is(results[1].Err, ErrSuppressedRecipient) {
		t.Errorf("second result should be a suppression, got %v", results[1].Err)
	}
	if results[1].To != "b@example.com" {
		t.Errorf("To = %q", results[1].To)
	}
}

func TestSendManyDerivesAKeyPerRecipient(t *testing.T) {
	rec := newRecorder(t,
		stubResponse{body: `{"id":"m1","status":"sent"}`},
		stubResponse{body: `{"id":"m2","status":"sent"}`},
	)
	client := testClient(t, rec)

	msg := &SendEmailRequest{From: "billing@acme.test", Text: "hi", IdempotencyKey: "digest-2026-09-01"}
	if _, err := client.Emails.SendMany(context.Background(),
		[]string{"a@example.com", "b@example.com"}, msg, nil); err != nil {
		t.Fatal(err)
	}

	// One key across the batch would make every recipient after the first an
	// idempotent replay of the first, and only one person gets the mail.
	if got := rec.call(0).headers.Get("Idempotency-Key"); got != "digest-2026-09-01-0" {
		t.Errorf("first key = %q", got)
	}
	if got := rec.call(1).headers.Get("Idempotency-Key"); got != "digest-2026-09-01-1" {
		t.Errorf("second key = %q", got)
	}
	// The caller's struct must come back untouched.
	if len(msg.To) != 0 || msg.IdempotencyKey != "digest-2026-09-01" {
		t.Errorf("SendMany mutated the caller's request: %+v", msg)
	}
}

func TestSendManyStopOnError(t *testing.T) {
	rec := newRecorder(t,
		stubResponse{status: 500, body: `{"status":500,"detail":"boom"}`},
		stubResponse{body: `{"id":"m2","status":"sent"}`},
	)
	client := testClient(t, rec)

	results, err := client.Emails.SendMany(context.Background(),
		[]string{"a@example.com", "b@example.com"},
		&SendEmailRequest{From: "billing@acme.test", Text: "hi"},
		&SendManyOptions{StopOnError: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || rec.count() != 1 {
		t.Errorf("expected to stop after the first failure, got %d results", len(results))
	}
}

// -- reading --------------------------------------------------------------

func TestListMapsTheSnakeCaseRow(t *testing.T) {
	rec := newRecorder(t, stubResponse{body: `[{
		"id":"m1",
		"message_id":"<x@acme.test>",
		"header_from":"billing@acme.test",
		"recipient":"customer@example.com",
		"subject":"Receipt",
		"status":"delivered",
		"created_at":"2026-09-01T10:00:00Z"
	}]`})
	client := testClient(t, rec)

	messages, err := client.Emails.List(context.Background(), &ListMessagesOptions{
		Limit: 10, Status: StatusDelivered,
	})
	if err != nil {
		t.Fatal(err)
	}

	if q := rec.call(0).rawQuery; q != "limit=10&status=delivered" {
		t.Errorf("query = %q", q)
	}
	m := messages[0]
	if m.From != "billing@acme.test" || m.To != "customer@example.com" {
		t.Errorf("from/to = %q/%q", m.From, m.To)
	}
	if m.CreatedAt.Year() != 2026 {
		t.Errorf("CreatedAt = %v", m.CreatedAt)
	}
}

func TestListOmitsUnsetFilters(t *testing.T) {
	rec := newRecorder(t, stubResponse{body: `[]`})
	client := testClient(t, rec)

	if _, err := client.Emails.List(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if q := rec.call(0).rawQuery; q != "" {
		t.Errorf("expected no query string, got %q", q)
	}
}

func TestEventsKeepsThePayloadVerbatim(t *testing.T) {
	rec := newRecorder(t, stubResponse{body: `[{
		"id":2,"event":"bounced",
		"payload":{"dsn_status":"5.1.1","Retry_Count":3},
		"created_at":"2026-09-01T10:00:00Z"
	}]`})
	client := testClient(t, rec)

	events, err := client.Emails.Events(context.Background(), "m1")
	if err != nil {
		t.Fatal(err)
	}
	// Tenant data. Rewriting these keys would corrupt real values.
	if events[0].Payload["dsn_status"] != "5.1.1" {
		t.Errorf("payload = %v", events[0].Payload)
	}
}

func TestDomainsDeriveVerified(t *testing.T) {
	rec := newRecorder(t, stubResponse{body: `[
		{"id":"d1","domain":"acme.test","selector":"mail","verified_at":"2026-08-01T09:00:00Z","created_at":"2026-07-01T09:00:00Z"},
		{"id":"d2","domain":"beta.test","selector":"mail","verified_at":null,"created_at":"2026-07-02T09:00:00Z"}
	]`})
	client := testClient(t, rec)

	domains, err := client.Domains.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !domains[0].Verified() || domains[1].Verified() {
		t.Errorf("verified = %v %v", domains[0].Verified(), domains[1].Verified())
	}
}

func TestAPIKeysAndWebhooksDeriveTheirFlags(t *testing.T) {
	rec := newRecorder(t,
		stubResponse{body: `[{"id":"k1","name":"old","prefix":"ann_a","scope":"full","created_at":"2026-01-01T00:00:00Z","last_used_at":"2026-02-01T00:00:00Z","revoked_at":"2026-03-01T00:00:00Z"}]`},
		stubResponse{body: `[{"id":"w1","url":"https://a.test","created_at":"2026-01-01T00:00:00Z","disabled_at":null}]`},
	)
	client := testClient(t, rec)
	ctx := context.Background()

	keys, err := client.APIKeys.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !keys[0].Revoked() {
		t.Error("expected the key to read as revoked")
	}

	endpoints, err := client.Webhooks.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if endpoints[0].Disabled() {
		t.Error("expected the endpoint to read as enabled")
	}
}

func TestDeleteTolteratesTheEmpty204(t *testing.T) {
	rec := newRecorder(t, stubResponse{status: http.StatusNoContent})
	client := testClient(t, rec)

	if err := client.Domains.Delete(context.Background(), "d1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if call := rec.call(0); call.method != http.MethodDelete || call.path != "/v1/domains/d1" {
		t.Errorf("got %s %s", call.method, call.path)
	}
}

func TestUsage(t *testing.T) {
	rec := newRecorder(t, stubResponse{body: `{
		"sentToday":12,"dailySendLimit":100,"domains":1,"maxDomains":3,"plan":"free",
		"sentThisPeriod":40,"periodStart":"2026-09-01T00:00:00Z",
		"monthlyIncludedMessages":null,"monthlyHardCap":null,"overageMinorUnits":null,
		"series":[{"date":"2026-09-01","sent":12,"delivered":10,"failed":1}]
	}`})
	client := testClient(t, rec)

	usage, err := client.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if usage.SentToday != 12 || usage.MonthlyHardCap != nil || usage.Series[0].Date != "2026-09-01" {
		t.Errorf("usage = %+v", usage)
	}
}

// -- errors ---------------------------------------------------------------

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		stub stubResponse
		want error
	}{
		{"401", stubResponse{status: 401, body: `{"status":401,"detail":"Invalid API key."}`}, ErrAuthentication},
		{"403", stubResponse{status: 403, body: `{"status":403,"detail":"Register the domain first."}`}, ErrPermission},
		{"400", stubResponse{status: 400, body: `{"status":400,"errors":{"from":["Not a valid address."]}}`}, ErrValidation},
		{"404 empty body", stubResponse{status: 404}, ErrNotFound},
		{"429", stubResponse{status: 429, body: `{"status":429,"detail":"Daily send limit reached."}`}, ErrRateLimit},
		{"503", stubResponse{status: 503, body: `{"status":503,"detail":"Refusing to send unsigned."}`}, ErrServer},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := newRecorder(t, tc.stub)
			client := testClient(t, rec)

			_, err := client.Send(context.Background(), &SendEmailRequest{
				From: "a@acme.test", To: Address("b@example.com"), Text: "hi",
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("errors.Is(%v, %v) = false", err, tc.want)
			}
		})
	}
}

func TestValidationErrorCarriesTheFieldMessages(t *testing.T) {
	rec := newRecorder(t, stubResponse{
		status: 400,
		body:   `{"title":"One or more validation errors occurred.","status":400,"errors":{"from":["Not a valid address."]}}`,
	})
	client := testClient(t, rec)

	_, err := client.Send(context.Background(), &SendEmailRequest{
		From: "nonsense", To: Address("b@example.com"), Text: "hi",
	})

	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected an *Error, got %T", err)
	}
	if got := apiErr.Errors["from"]; len(got) != 1 || got[0] != "Not a valid address." {
		t.Errorf("Errors = %v", apiErr.Errors)
	}
	if apiErr.Message != "from: Not a valid address." {
		t.Errorf("Message = %q", apiErr.Message)
	}
}

func TestSuppressedRecipientIsAlsoUnprocessable(t *testing.T) {
	rec := newRecorder(t, stubResponse{
		status: 422,
		body:   `{"status":422,"detail":"bounced@example.com is on your suppression list."}`,
	})
	client := testClient(t, rec)

	_, err := client.Send(context.Background(), &SendEmailRequest{
		From: "a@acme.test", To: Address("bounced@example.com"), Text: "hi",
	})

	if !errors.Is(err, ErrSuppressedRecipient) {
		t.Error("should match ErrSuppressedRecipient")
	}
	if !errors.Is(err, ErrUnprocessable) {
		t.Error("should also match the broader ErrUnprocessable")
	}
	var apiErr *Error
	if errors.As(err, &apiErr); apiErr.Recipient != "bounced@example.com" {
		t.Errorf("Recipient = %q", apiErr.Recipient)
	}
}

func TestUnprocessableOutsideSendIsNotASuppression(t *testing.T) {
	rec := newRecorder(t, stubResponse{
		status: 422,
		body:   `{"status":422,"detail":"No TXT record found."}`,
	})
	client := testClient(t, rec)

	_, err := client.Domains.Verify(context.Background(), "d1")

	if !errors.Is(err, ErrUnprocessable) {
		t.Error("should match ErrUnprocessable")
	}
	if errors.Is(err, ErrSuppressedRecipient) {
		t.Error("a failed DKIM verification is not a suppressed recipient")
	}
}

func TestBareErrorKeyShapeIsUnderstood(t *testing.T) {
	// Two 409 paths answer {"error": "..."} rather than problem+json.
	rec := newRecorder(t, stubResponse{
		status: 409,
		body:   `{"error":"Domain acme.test is already registered."}`,
	})
	client := testClient(t, rec)

	_, err := client.Domains.Create(context.Background(), "acme.test")

	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
	var apiErr *Error
	errors.As(err, &apiErr)
	if apiErr.Message != "Domain acme.test is already registered." {
		t.Errorf("Message = %q", apiErr.Message)
	}
}

func TestRetryAfterAndRequestIDAreSurfaced(t *testing.T) {
	rec := newRecorder(t, stubResponse{
		status:  429,
		headers: map[string]string{"Retry-After": "3600", "X-Request-Id": "req_abc123"},
		body:    `{"status":429,"detail":"Daily send limit reached."}`,
	})
	client := testClient(t, rec)

	_, err := client.Usage(context.Background())

	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected an *Error, got %T", err)
	}
	if apiErr.RetryAfter != time.Hour {
		t.Errorf("RetryAfter = %v", apiErr.RetryAfter)
	}
	if apiErr.RequestID != "req_abc123" {
		t.Errorf("RequestID = %q", apiErr.RequestID)
	}
}

func TestConnectionFailure(t *testing.T) {
	client, err := New("ann_k",
		WithBaseURL("http://127.0.0.1:1"), // nothing listens on port 1
		WithMaxRetries(0),
		WithTimeout(2*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.Usage(context.Background())
	if !errors.Is(err, ErrConnection) {
		t.Fatalf("expected ErrConnection, got %v", err)
	}
}

// -- retries --------------------------------------------------------------

func TestRetriesA500(t *testing.T) {
	rec := newRecorder(t,
		stubResponse{status: 500, body: `{"status":500,"detail":"boom"}`},
		stubResponse{body: `{"id":"m","status":"sent"}`},
	)
	client := testClient(t, rec, WithMaxRetries(2))

	sent, err := client.Send(context.Background(), &SendEmailRequest{
		From: "a@acme.test", To: Address("b@example.com"), Text: "hi",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if rec.count() != 2 || sent.ID != "m" {
		t.Errorf("calls=%d sent=%+v", rec.count(), sent)
	}
}

func TestRetriesReuseTheSameIdempotencyKey(t *testing.T) {
	rec := newRecorder(t,
		stubResponse{status: 500, body: `{"status":500}`},
		stubResponse{body: `{"id":"m","status":"sent"}`},
	)
	client := testClient(t, rec, WithMaxRetries(2))

	if _, err := client.Send(context.Background(), &SendEmailRequest{
		From: "a@acme.test", To: Address("b@example.com"), Text: "hi",
	}); err != nil {
		t.Fatal(err)
	}

	// The whole point: the retry must be recognisable to the API as the same
	// operation, or a timeout on the first attempt would send twice.
	first := rec.call(0).headers.Get("Idempotency-Key")
	second := rec.call(1).headers.Get("Idempotency-Key")
	if first == "" || first != second {
		t.Errorf("keys differed across a retry: %q vs %q", first, second)
	}
}

func TestConflictOnSendIsRetried(t *testing.T) {
	rec := newRecorder(t,
		stubResponse{status: 409, body: `{"error":"A request with this Idempotency-Key is already in flight."}`},
		stubResponse{body: `{"id":"m","status":"sent","idempotentReplay":true}`},
	)
	client := testClient(t, rec, WithMaxRetries(2))

	sent, err := client.Send(context.Background(), &SendEmailRequest{
		From: "a@acme.test", To: Address("b@example.com"), Text: "hi",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if rec.count() != 2 || !sent.IdempotentReplay {
		t.Errorf("calls=%d replay=%v", rec.count(), sent.IdempotentReplay)
	}
}

func TestConflictOutsideSendIsNotRetried(t *testing.T) {
	rec := newRecorder(t, stubResponse{
		status: 409, body: `{"error":"Domain acme.test is already registered."}`,
	})
	client := testClient(t, rec, WithMaxRetries(2))

	if _, err := client.Domains.Create(context.Background(), "acme.test"); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
	if rec.count() != 1 {
		t.Errorf("a duplicate domain is a real conflict, not a transient one; made %d calls", rec.count())
	}
}

func TestValidationIsNotRetried(t *testing.T) {
	rec := newRecorder(t, stubResponse{
		status: 400, body: `{"status":400,"errors":{"from":["Not a valid address."]}}`,
	})
	client := testClient(t, rec, WithMaxRetries(2))

	if _, err := client.Send(context.Background(), &SendEmailRequest{
		From: "junk", To: Address("b@example.com"), Text: "hi",
	}); !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
	if rec.count() != 1 {
		t.Errorf("made %d calls, want 1", rec.count())
	}
}

func TestRetryAfterBeatsTheComputedBackoff(t *testing.T) {
	rec := newRecorder(t,
		stubResponse{status: 429, headers: map[string]string{"Retry-After": "1"}, body: `{"status":429}`},
		stubResponse{body: `{"sentToday":1}`},
	)
	client := testClient(t, rec, WithMaxRetries(1))

	started := time.Now()
	if _, err := client.Usage(context.Background()); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)

	if rec.count() != 2 {
		t.Fatalf("made %d calls", rec.count())
	}
	// The point is that it waited roughly the second the server asked for
	// rather than its own sub-second guess.
	if elapsed < 900*time.Millisecond {
		t.Errorf("expected to wait ~1s, waited %v", elapsed)
	}
}

func TestContextCancellationStopsTheRetryLoop(t *testing.T) {
	rec := newRecorder(t,
		stubResponse{status: 500, body: `{"status":500}`},
		stubResponse{status: 500, body: `{"status":500}`},
		stubResponse{status: 500, body: `{"status":500}`},
	)
	client := testClient(t, rec, WithMaxRetries(5))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := client.Usage(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the context error to surface, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Recipients: cc, bcc, reply-to

func TestSendCarriesCcBccAndReplyTo(t *testing.T) {
	rec := newRecorder(t, stubResponse{
		body: `{"id":"m","status":"sent","recipients":4}`,
	})
	client := testClient(t, rec)

	sent, err := client.Send(context.Background(), &SendEmailRequest{
		From:    "billing@acme.test",
		To:      []string{"a@example.com", "b@example.com"},
		Cc:      Address("accounting@acme.test"),
		Bcc:     AddressList("archive@acme.test"),
		ReplyTo: Address("support@acme.test"),
		Subject: "Your receipt",
		Text:    "Thanks!",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	body := decodeBody(t, rec.call(0).body)
	want := map[string]any{
		"from":     "billing@acme.test",
		"to":       []any{"a@example.com", "b@example.com"},
		"cc":       []any{"accounting@acme.test"},
		"bcc":      []any{"archive@acme.test"},
		"reply_to": []any{"support@acme.test"},
		"subject":  "Your receipt",
		"text":     "Thanks!",
	}
	if !reflect.DeepEqual(body, want) {
		t.Errorf("body = %#v\nwant %#v", body, want)
	}
	if sent.Recipients != 4 {
		t.Errorf("Recipients = %d, want 4", sent.Recipients)
	}
}

func TestSendNeedsAtLeastOneRecipient(t *testing.T) {
	rec := newRecorder(t)
	client := testClient(t, rec)

	_, err := client.Send(context.Background(), &SendEmailRequest{
		From: "a@acme.test",
		To:   Addresses{},
		Text: "hi",
	})
	if err == nil {
		t.Fatal("expected a validation error")
	}
	if rec.count() != 0 {
		t.Errorf("should not have called the API, made %d calls", rec.count())
	}
}

func TestSendReportsPartiallySuppressedRecipients(t *testing.T) {
	rec := newRecorder(t, stubResponse{
		body: `{"id":"m","status":"sent","recipients":2,"suppressed":["dead@example.com"]}`,
	})
	client := testClient(t, rec)

	sent, err := client.Send(context.Background(), &SendEmailRequest{
		From: "a@acme.test",
		To:   []string{"good@example.com", "dead@example.com"},
		Cc:   Address("copied@example.com"),
		Text: "hi",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	// The message still went out; only the bad address was dropped.
	if sent.Recipients != 2 {
		t.Errorf("Recipients = %d, want 2", sent.Recipients)
	}
	if !reflect.DeepEqual(sent.Suppressed, []string{"dead@example.com"}) {
		t.Errorf("Suppressed = %#v", sent.Suppressed)
	}
}

func TestFullySuppressedSendListsEveryRefusedAddress(t *testing.T) {
	rec := newRecorder(t, stubResponse{
		status: 422,
		body: `{"status":422,"detail":"All 2 recipients are on your suppression list.",` +
			`"suppressed":["one@example.com","two@example.com"]}`,
	})
	client := testClient(t, rec)

	_, err := client.Send(context.Background(), &SendEmailRequest{
		From: "a@acme.test",
		To:   []string{"one@example.com", "two@example.com"},
		Text: "hi",
	})

	if !errors.Is(err, ErrSuppressedRecipient) {
		t.Fatalf("expected ErrSuppressedRecipient, got %v", err)
	}
	var apiErr *Error
	errors.As(err, &apiErr)
	// Read from the API's extension member, not parsed out of the prose.
	if !reflect.DeepEqual(apiErr.Suppressed, []string{"one@example.com", "two@example.com"}) {
		t.Errorf("Suppressed = %#v", apiErr.Suppressed)
	}
	if apiErr.Recipient != "one@example.com" {
		t.Errorf("Recipient = %q", apiErr.Recipient)
	}
}

func TestMessageListReportsThePrimaryAndTheCount(t *testing.T) {
	rec := newRecorder(t, stubResponse{body: `[{
		"id":"m1","message_id":"<x@acme.test>","header_from":"billing@acme.test",
		"recipient":"primary@example.com","recipient_count":3,"reply_to":"support@acme.test",
		"subject":"Receipt","status":"delivered","created_at":"2026-09-01T10:00:00Z"
	}]`})
	client := testClient(t, rec)

	messages, err := client.Emails.List(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}

	m := messages[0]
	if m.To != "primary@example.com" || m.RecipientCount != 3 {
		t.Errorf("to=%q count=%d", m.To, m.RecipientCount)
	}
	if m.ReplyTo == nil || *m.ReplyTo != "support@acme.test" {
		t.Errorf("ReplyTo = %v", m.ReplyTo)
	}
}

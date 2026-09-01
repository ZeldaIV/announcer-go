package announcer

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

const testSecret = "whsec_0123456789abcdef"

var testNow = time.Unix(1_800_000_000, 0)

const testBody = `{"event":"bounced","occurredAt":"2026-09-01T10:00:00Z",` +
	`"message":{"id":"8f3a0000-0000-0000-0000-000000000001","from":"billing@acme.test",` +
	`"to":"customer@example.com","subject":"Your receipt","status":"bounced"},` +
	`"detail":{"dsnStatus":"5.1.1 user unknown"}}`

// sign builds the header exactly the way the API's signature_header does.
func sign(body string, timestamp int64, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.%s", timestamp, body)
	return fmt.Sprintf("t=%d,v1=%s", timestamp, hex.EncodeToString(mac.Sum(nil)))
}

func TestVerifyWebhookAcceptsAGoodSignature(t *testing.T) {
	header := sign(testBody, testNow.Unix(), testSecret)

	event, err := VerifyWebhook([]byte(testBody), header, testSecret, WithClock(testNow))
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}

	if event.Event != EventBounced {
		t.Errorf("Event = %q", event.Event)
	}
	if event.Message.To != "customer@example.com" || event.Message.From != "billing@acme.test" {
		t.Errorf("message = %+v", event.Message)
	}
	if event.Detail["dsnStatus"] != "5.1.1 user unknown" {
		t.Errorf("Detail = %v", event.Detail)
	}
	if event.OccurredAt.Year() != 2026 {
		t.Errorf("OccurredAt = %v", event.OccurredAt)
	}
}

func TestSignatureMatchesTheAPITestVector(t *testing.T) {
	// Pinned against announcer's own signature_header unit test, so a change
	// on either side shows up here rather than in production.
	mac := hmac.New(sha256.New, []byte("key"))
	mac.Write([]byte("1700000000.{}"))
	want := "t=1700000000,v1=" + hex.EncodeToString(mac.Sum(nil))

	if got := sign("{}", 1_700_000_000, "key"); got != want {
		t.Errorf("sign() = %q, want %q", got, want)
	}
}

func TestVerifyWebhookRejectsATamperedBody(t *testing.T) {
	header := sign(testBody, testNow.Unix(), testSecret)
	tampered := strings.Replace(testBody, "customer@example.com", "attacker@evil.test", 1)

	_, err := VerifyWebhook([]byte(tampered), header, testSecret, WithClock(testNow))

	if !errors.Is(err, ErrSignatureVerification) {
		t.Fatalf("expected ErrSignatureVerification, got %v", err)
	}
}

func TestVerifyWebhookRejectsTheWrongSecret(t *testing.T) {
	header := sign(testBody, testNow.Unix(), testSecret)

	_, err := VerifyWebhook([]byte(testBody), header, "whsec_wrong", WithClock(testNow))

	if !errors.Is(err, ErrSignatureVerification) {
		t.Fatalf("expected ErrSignatureVerification, got %v", err)
	}
}

func TestVerifyWebhookRejectsReplays(t *testing.T) {
	for _, offset := range []time.Duration{-time.Hour, time.Hour} {
		stamp := testNow.Add(offset).Unix()
		header := sign(testBody, stamp, testSecret)

		_, err := VerifyWebhook([]byte(testBody), header, testSecret, WithClock(testNow))

		if !errors.Is(err, ErrSignatureVerification) {
			t.Errorf("offset %v: expected rejection, got %v", offset, err)
		}
		if err != nil && !strings.Contains(err.Error(), "tolerance") {
			t.Errorf("offset %v: error should mention the tolerance, got %q", offset, err)
		}
	}
}

func TestVerifyWebhookToleranceCanBeDisabled(t *testing.T) {
	old := testNow.Add(-24 * time.Hour).Unix()
	header := sign(testBody, old, testSecret)

	event, err := VerifyWebhook([]byte(testBody), header, testSecret,
		WithClock(testNow), WithTolerance(0))
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	if event.Event != EventBounced {
		t.Errorf("Event = %q", event.Event)
	}
}

func TestVerifyWebhookRejectsMalformedHeaders(t *testing.T) {
	for _, header := range []string{"", "garbage", "t=123", "v1=deadbeef", "t=notanumber,v1=x"} {
		_, err := VerifyWebhook([]byte(testBody), header, testSecret, WithClock(testNow))
		if !errors.Is(err, ErrSignatureVerification) {
			t.Errorf("header %q: expected rejection, got %v", header, err)
		}
	}
}

func TestVerifyWebhookIgnoresUnknownSchemes(t *testing.T) {
	// A future v2 must not break v1 consumers.
	header := sign(testBody, testNow.Unix(), testSecret) + ",v2=somethingelse"

	event, err := VerifyWebhook([]byte(testBody), header, testSecret, WithClock(testNow))
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	if event.Event != EventBounced {
		t.Errorf("Event = %q", event.Event)
	}
}

func TestVerifyWebhookRequiresASecret(t *testing.T) {
	header := sign(testBody, testNow.Unix(), testSecret)

	_, err := VerifyWebhook([]byte(testBody), header, "", WithClock(testNow))

	if !errors.Is(err, ErrSignatureVerification) {
		t.Fatalf("expected ErrSignatureVerification, got %v", err)
	}
}

func TestWebhooksServiceVerify(t *testing.T) {
	rec := newRecorder(t)
	client := testClient(t, rec)
	header := sign(testBody, testNow.Unix(), testSecret)

	event, err := client.Webhooks.Verify([]byte(testBody), header, testSecret, WithClock(testNow))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if event.Event != EventBounced {
		t.Errorf("Event = %q", event.Event)
	}
}

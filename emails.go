package announcer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// EmailsService covers sending mail and reading send history. Both work with
// either key scope.
type EmailsService struct{ client *Client }

// validateSend catches the mistakes worth catching before spending an API call.
func validateSend(req *SendEmailRequest) error {
	if req == nil {
		return errors.New("announcer: Send needs a *SendEmailRequest, got nil")
	}
	if req.From == "" {
		return errors.New("announcer: Send needs a From address")
	}
	if req.To == "" {
		return errors.New(
			"announcer: Send needs a To address. Announcer takes one recipient per call — " +
				"use Emails.SendMany to fan out")
	}
	if req.Text == "" && req.HTML == "" {
		return errors.New("announcer: provide Text, HTML, or both — an email needs a body")
	}
	return nil
}

// Send sends one email.
//
//	sent, err := client.Emails.Send(ctx, &announcer.SendEmailRequest{
//	    From:    "Acme <billing@acme.com>",
//	    To:      "customer@example.com",
//	    Subject: "Your receipt",
//	    Text:    "Thanks!",
//	})
//
// An Idempotency-Key is generated when SendEmailRequest.IdempotencyKey is
// empty, so the SDK's automatic retries can never send twice. Set it yourself
// — an order id, a job id — to extend that guarantee across process restarts.
//
// Failures worth branching on: ErrPermission (the From domain is not
// registered or not verified), ErrSuppressedRecipient (they bounced or
// complained before), ErrRateLimit (a limit or a quota; see Error.RetryAfter).
func (s *EmailsService) Send(ctx context.Context, req *SendEmailRequest) (*SentEmail, error) {
	if err := validateSend(req); err != nil {
		return nil, err
	}

	key := req.IdempotencyKey
	if key == "" {
		key = newIdempotencyKey()
	}

	var out SentEmail
	err := s.client.do(ctx, requestSpec{
		method:  http.MethodPost,
		path:    pathEmails,
		body:    req,
		headers: map[string]string{"Idempotency-Key": key},
		// Carrying a key makes a 409 mean "the original is still in flight",
		// so waiting and asking again is right. Without one it is a real
		// conflict and retrying would be wrong.
		retryOn409: true,
		recipient:  req.To,
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// SendManyOptions tunes SendMany.
type SendManyOptions struct {
	// StopOnError abandons the remaining recipients after the first failure.
	// Off by default: one suppressed address should not sink a batch.
	StopOnError bool
}

// SendMany sends the same message to several recipients, one API call each,
// and returns a result per recipient in the order given.
//
// These are separate emails: no recipient can see the others. Announcer has no
// CC or BCC.
//
// The returned error is non-nil only when the context was cancelled; the
// results gathered up to that point are still returned. Per-recipient failures
// live in BatchSendResult.Err.
func (s *EmailsService) SendMany(
	ctx context.Context,
	recipients []string,
	msg *SendEmailRequest,
	opts *SendManyOptions,
) ([]BatchSendResult, error) {
	if opts == nil {
		opts = &SendManyOptions{}
	}

	results := make([]BatchSendResult, 0, len(recipients))
	for i, to := range recipients {
		if err := ctx.Err(); err != nil {
			return results, err
		}

		// Copied so the caller's struct is untouched, and so each recipient
		// gets its own derived key: one key across the batch would make every
		// recipient after the first an idempotent replay of the first, and
		// only one person would get the mail.
		attempt := *msg
		attempt.To = to
		if msg.IdempotencyKey != "" {
			attempt.IdempotencyKey = fmt.Sprintf("%s-%d", msg.IdempotencyKey, i)
		} else {
			attempt.IdempotencyKey = ""
		}

		sent, err := s.Send(ctx, &attempt)
		results = append(results, BatchSendResult{To: to, Result: sent, Err: err})
		if err != nil && opts.StopOnError {
			break
		}
	}
	return results, nil
}

// List returns send history, newest first. A nil opts asks for the API's
// defaults.
func (s *EmailsService) List(ctx context.Context, opts *ListMessagesOptions) ([]Message, error) {
	query := url.Values{}
	if opts != nil {
		if opts.Limit > 0 {
			query.Set("limit", strconv.Itoa(opts.Limit))
		}
		if opts.Status != "" {
			query.Set("status", opts.Status)
		}
		if opts.Search != "" {
			query.Set("search", opts.Search)
		}
	}

	var out []Message
	err := s.client.do(ctx, requestSpec{
		method: http.MethodGet,
		path:   "/v1/messages",
		query:  query,
	}, &out)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Events returns the audit trail for one message: every status transition,
// oldest first.
func (s *EmailsService) Events(ctx context.Context, messageID string) ([]MessageEvent, error) {
	var out []MessageEvent
	err := s.client.do(ctx, requestSpec{
		method: http.MethodGet,
		path:   "/v1/messages/" + url.PathEscape(messageID) + "/events",
	}, &out)
	if err != nil {
		return nil, err
	}
	return out, nil
}

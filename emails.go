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
	if len(req.To) == 0 {
		return errors.New("announcer: Send needs at least one To address")
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
//	    To:      []string{"customer@example.com", "partner@example.com"},
//	    Cc:      announcer.Address("accounting@acme.com"),
//	    ReplyTo: announcer.Address("support@acme.com"),
//	    Subject: "Your receipt",
//	    Text:    "Thanks!",
//	})
//
// To and Cc go out as one email whose recipients see each other; Bcc
// recipients see nobody. At most 50 addresses across the three.
//
// An Idempotency-Key is generated when SendEmailRequest.IdempotencyKey is
// empty, so the SDK's automatic retries can never send twice. Set it yourself
// — an order id, a job id — to extend that guarantee across process restarts.
//
// A recipient on the account's suppression list is dropped and reported in
// SentEmail.Suppressed; the rest of the message still goes out. Only when every
// recipient is suppressed does this return an error.
//
// Failures worth branching on: ErrPermission (the From domain is not
// registered or not verified), ErrSuppressedRecipient (every recipient bounced
// or complained before), ErrRateLimit (a limit or a quota; see
// Error.RetryAfter — quota counts recipients, so one call can consume several).
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
		recipient:  req.To.First(),
	}, &out)
	if err != nil {
		return nil, err
	}
	// Older deployments predate the field; a successful send is at least one
	// recipient.
	if out.Recipients == 0 {
		out.Recipients = 1
	}
	return &out, nil
}

// SendManyOptions tunes SendMany.
type SendManyOptions struct {
	// StopOnError abandons the remaining recipients after the first failure.
	// Off by default: one suppressed address should not sink a batch.
	StopOnError bool
}

// SendMany sends the same message to several recipients as separate emails,
// one API call each, and returns a result per recipient in the order given.
//
// This is not the same as putting several addresses in SendEmailRequest.To:
//
//   - To: []string{a, b} is one email. A and B see each other in the To
//     header, it costs one request, and one bounce marks one message.
//   - SendMany([]string{a, b}, …) is two emails. Neither knows the other
//     exists, each gets its own idempotency key and its own bounce, and one
//     failure leaves the other untouched.
//
// Use this one for anything list-shaped — a newsletter, a digest, a fan-out.
// A Cc on msg is copied on every message, so a three-recipient batch sends the
// Cc three copies; that is usually not what you want.
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
		attempt.To = Address(to)
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

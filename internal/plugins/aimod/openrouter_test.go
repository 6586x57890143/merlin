package aimod

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The HTTP client, against a stub OpenRouter. These are the cases that
// actually turned up in production rather than the happy path.

func stubOpenRouter(t *testing.T, status int, body string) (*Client, *chatRequest) {
	t.Helper()
	var got chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat/completions" {
			_ = json.NewDecoder(r.Body).Decode(&got)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	c := NewClient()
	c.base = srv.URL
	return c, &got
}

// The failure that prompted all of this: a 200 carrying no content. It is
// not a parse error, and reporting it as one told nobody anything. It must
// also be distinguishable with errors.Is, so a caller can tell "the model
// said nothing" from "the model said something malformed".
func TestEmptyCompletionIsItsOwnError(t *testing.T) {
	c, _ := stubOpenRouter(t, http.StatusOK, `{
		"model": "some/model",
		"choices": [{"message": {"role": "assistant", "content": ""}, "finish_reason": "length"}],
		"usage": {"prompt_tokens": 400, "completion_tokens": 400, "cost": 0.00002}
	}`)

	out, usage, err := c.Chat(context.Background(), "k", chatRequest{})
	if !errors.Is(err, ErrEmptyCompletion) {
		t.Fatalf("err = %v, want ErrEmptyCompletion", err)
	}
	if out != "" {
		t.Errorf("out = %q, want empty", out)
	}
	// The two facts that make the next occurrence diagnosable.
	if !strings.Contains(err.Error(), "some/model") {
		t.Errorf("error does not name the model that answered: %v", err)
	}
	if !strings.Contains(err.Error(), "length") {
		t.Errorf("error does not carry finish_reason, so truncation is indistinguishable from silence: %v", err)
	}
	// The call was billed either way, and the budget has to know.
	if usage.Cost == 0 || usage.CompletionTokens == 0 {
		t.Errorf("usage = %+v, want the billed usage returned alongside the error", usage)
	}
}

// Whitespace-only is the same condition wearing a hat: it parses to nothing
// and means nothing.
func TestWhitespaceOnlyCompletionIsEmpty(t *testing.T) {
	c, _ := stubOpenRouter(t, http.StatusOK, `{
		"choices": [{"message": {"content": "  \n  "}, "finish_reason": "stop"}]
	}`)
	if _, _, err := c.Chat(context.Background(), "k", chatRequest{}); !errors.Is(err, ErrEmptyCompletion) {
		t.Errorf("err = %v, want ErrEmptyCompletion", err)
	}
}

// The ceilings have to leave room for a model that reasons before it
// answers, or it returns empty content and the scan silently did not happen.
func TestTokenCeilingsLeaveRoomForReasoning(t *testing.T) {
	if fastMaxTokens < 1000 {
		t.Errorf("fastMaxTokens = %d, too tight for a reasoning model to answer after thinking", fastMaxTokens)
	}
	if deepMaxTokens <= fastMaxTokens {
		t.Errorf("deepMaxTokens = %d, not above the fast ceiling despite also returning a rewrite", deepMaxTokens)
	}
}

// A bad key, a rate limit and an outage are three different things and the
// caller has to be able to tell them apart: this plugin must never read
// "temporarily unavailable" as "nothing to moderate".
func TestAPIErrorCarriesStatus(t *testing.T) {
	c, _ := stubOpenRouter(t, http.StatusTooManyRequests,
		`{"error": {"code": 429, "message": "Rate limit exceeded"}}`)

	_, _, err := c.Chat(context.Background(), "k", chatRequest{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want an *APIError", err)
	}
	if apiErr.Status != http.StatusTooManyRequests {
		t.Errorf("Status = %d, want 429", apiErr.Status)
	}
	// The human half, not the JSON envelope: an admin who pasted a bad key
	// wants to read what went wrong.
	if !strings.Contains(apiErr.Message, "Rate limit") {
		t.Errorf("Message = %q, want the upstream message", apiErr.Message)
	}
}

// An error delivered inside a 200 body is still an error.
func TestErrorInsideASuccessfulResponse(t *testing.T) {
	c, _ := stubOpenRouter(t, http.StatusOK,
		`{"error": {"code": 402, "message": "Insufficient credits"}, "usage": {"cost": 0}}`)

	var apiErr *APIError
	if _, _, err := c.Chat(context.Background(), "k", chatRequest{}); !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want an *APIError", err)
	} else if apiErr.Status != 402 {
		t.Errorf("Status = %d, want 402", apiErr.Status)
	}
}

// Every request pins zero data retention. Asserted against the wire format
// rather than the struct, because that is what the provider actually sees.
func TestPrivacyPrefsReachTheWire(t *testing.T) {
	raw, err := json.Marshal(chatRequest{Provider: strictProvider()})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"zdr":true`, `"data_collection":"deny"`, `"require_parameters":true`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("request body is missing %s: %s", want, raw)
		}
	}
}

// Reasoning cannot be decided from metadata: a model can accept the
// parameter for raising effort while refusing to have it disabled, and
// /models does not distinguish those. So it is tried once per stack and the
// answer remembered.
func TestReasoningIsDisabledUntilAnEndpointRefuses(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		if strings.Contains(string(raw), `"reasoning"`) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":400,"message":"Reasoning is mandatory for this endpoint and cannot be disabled."}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"v\":[]}"}}]}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient()
	c.base = srv.URL
	req := chatRequest{Models: []string{"some/reasoner"}}

	// First call: tries to disable, is refused, retries without and succeeds.
	out, _, err := c.Chat(context.Background(), "k", req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if out == "" {
		t.Error("the retry did not produce an answer")
	}
	if len(bodies) != 2 {
		t.Fatalf("made %d requests, want 2 (the attempt and the retry)", len(bodies))
	}
	if !strings.Contains(bodies[0], `"reasoning"`) || strings.Contains(bodies[1], `"reasoning"`) {
		t.Error("expected the first request to carry the preference and the retry to drop it")
	}

	// Second call: the answer is remembered, so no wasted attempt.
	if _, _, err := c.Chat(context.Background(), "k", req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(bodies) != 3 {
		t.Errorf("made %d requests total, want 3: the rejection was not remembered", len(bodies))
	}
	if strings.Contains(bodies[2], `"reasoning"`) {
		t.Error("still sending a preference the endpoint has already rejected")
	}
	if c.ReasoningDisabled(req.spec, req.Models) {
		t.Error("ReasoningDisabled still reports true, so /aimod models show would claim thinking is not billed")
	}
}

// An unrelated 400 must not be mistaken for the reasoning rejection, or a
// genuinely bad request gets silently retried and reported as something else.
func TestOtherBadRequestsAreNotRetried(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":400,"message":"model not found"}}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient()
	c.base = srv.URL
	if _, _, err := c.Chat(context.Background(), "k", chatRequest{Models: []string{"a/b"}}); err == nil {
		t.Fatal("a bad request was reported as success")
	}
	if calls != 1 {
		t.Errorf("made %d requests for an unrelated 400, want 1", calls)
	}
	if !c.ReasoningDisabled(nil, []string{"a/b"}) {
		t.Error("an unrelated 400 was recorded as a reasoning rejection")
	}
}

// A 429 or a 5xx used to lose the whole batch: one log line, and twenty
// messages nothing ever judged. APIError has carried the status all along
// precisely so this could be told apart from a permanent failure, and
// nothing acted on it. The Models fallback array does not cover this, since
// it moves between models on a model error, not on an account-level rate
// limit or an OpenRouter outage.
func TestTransientFailuresGetOneRetry(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls == 1 {
					w.WriteHeader(status)
					_, _ = w.Write([]byte(`{"error":{"message":"try again"}}`))
					return
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"model":"a/b","choices":[{"message":{"content":"{}"}}]}`))
			}))
			t.Cleanup(srv.Close)

			c := NewClient()
			c.base = srv.URL
			out, _, err := c.Chat(context.Background(), "k", chatRequest{Models: []string{"a/b"}})
			if err != nil {
				t.Fatalf("a transient %d was not retried: %v", status, err)
			}
			if out != "{}" {
				t.Errorf("content = %q, want the retry's answer", out)
			}
			if calls != 2 {
				t.Errorf("made %d requests, want exactly one retry", calls)
			}
		})
	}
}

// Exactly one retry, never a loop. This call sits inside a two minute batch
// context with nineteen other messages waiting behind it, so a provider that
// is genuinely down has to fail fast rather than hold the slot.
func TestTransientFailuresAreRetriedOnlyOnce(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"down"}}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient()
	c.base = srv.URL
	if _, _, err := c.Chat(context.Background(), "k", chatRequest{Models: []string{"a/b"}}); err == nil {
		t.Fatal("a persistent outage was reported as success")
	}
	if calls != 2 {
		t.Errorf("made %d requests, want 2: one attempt and one retry", calls)
	}
}

// A bad key fails identically the second time, so retrying it doubles a
// pointless bill and delays the log line saying what is actually wrong.
func TestPermanentFailuresAreNotRetried(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":{"message":"nope"}}`))
			}))
			t.Cleanup(srv.Close)

			c := NewClient()
			c.base = srv.URL
			if _, _, err := c.Chat(context.Background(), "k", chatRequest{Models: []string{"a/b"}}); err == nil {
				t.Fatalf("a %d was reported as success", status)
			}
			if calls != 1 {
				t.Errorf("made %d requests for a permanent %d, want 1", calls, status)
			}
		})
	}
}

// A cancelled context is the batch's own deadline, not a transient failure.
// Retrying past it is work nobody is waiting for, and it would run on into
// the next batch's budget.
func TestACancelledContextIsNotRetried(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"down"}}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient()
	c.base = srv.URL

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := c.Chat(ctx, "k", chatRequest{Models: []string{"a/b"}}); err == nil {
		t.Fatal("a cancelled call was reported as success")
	}
	if calls > 1 {
		t.Errorf("made %d requests on a cancelled context", calls)
	}
}

// Retry-After is honoured but clamped: a provider asking for ten minutes is
// telling this batch it is over, and sleeping on it would burn the whole
// spawn context holding a slot nineteen other messages need.
func TestRetryAfterIsClamped(t *testing.T) {
	if got := parseRetryAfter("2"); got != 2*time.Second {
		t.Errorf("parseRetryAfter(\"2\") = %v, want 2s", got)
	}
	for _, in := range []string{"", "later", "0", "-1"} {
		if got := parseRetryAfter(in); got != 0 {
			t.Errorf("parseRetryAfter(%q) = %v, want 0", in, got)
		}
	}
	long := &APIError{Status: http.StatusTooManyRequests, RetryAfter: 10 * time.Minute}
	if got := retryAfter(long); got != maxRetryPause {
		t.Errorf("retryAfter for a 10 minute request = %v, want the %v cap", got, maxRetryPause)
	}
	if got := retryAfter(&APIError{Status: 500}); got != retryPause {
		t.Errorf("retryAfter with no header = %v, want the default %v", got, retryPause)
	}
}

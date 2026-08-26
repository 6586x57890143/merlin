package aimod

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

// Reasoning is billed and counts against max_tokens, so a reasoning model
// can spend the whole ceiling thinking and return nothing. Both passes send
// it explicitly disabled; omitting the field lets the model decide, which is
// the behaviour being ruled out.
func TestClassifierRequestsDisableReasoning(t *testing.T) {
	c, got := stubOpenRouter(t, http.StatusOK, `{"choices":[{"message":{"content":"{\"v\":[]}"}}]}`)
	p := testPlugin(t, newFakeStore(), c, newFakeOps(), &fakeAudit{})

	if _, _, err := p.classifyFast(context.Background(), "k", enforcingConfig(),
		[]candidate{{MessageID: "m1", Content: "a message long enough to scan"}}); err != nil {
		t.Fatalf("classifyFast: %v", err)
	}
	if got.Reasoning.Enabled {
		t.Error("the fast pass asked for reasoning, which is billed and eats max_tokens for no gain")
	}

	raw, err := json.Marshal(chatRequest{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Explicitly present rather than omitted, which is the whole point.
	if !strings.Contains(string(raw), `"reasoning":{"enabled":false}`) {
		t.Errorf("reasoning is not being sent on every request: %s", raw)
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

package ratelimit

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/disgoorg/disgo/rest"
)

type inner struct {
	rest.RateLimiter
	retryAfter string
}

func (i *inner) Unlock(_ *rest.CompiledEndpoint, rs *http.Response) error {
	if rs != nil {
		i.retryAfter = rs.Header.Get("Retry-After")
	}
	return nil
}

// body is a response body that remembers being closed.
type body struct {
	io.Reader
	closed bool
}

func (b *body) Close() error { b.closed = true; return nil }

func answer(code int, h map[string]string, payload string) (*http.Response, *body) {
	b := &body{Reader: strings.NewReader(payload)}
	rs := &http.Response{StatusCode: code, Header: http.Header{"Via": {"1.1 google"}}, Body: b}
	for k, v := range h {
		rs.Header.Set(k, v)
	}
	return rs, b
}

func unlock(rs *http.Response) (time.Duration, string) { return unlockWith(rs, true) }

func unlockWith(rs *http.Response, userReset bool) (time.Duration, string) {
	in := &inner{}
	slept := time.Duration(-1)
	p := &precise{RateLimiter: in, userReset: userReset, sleep: func(d time.Duration) { slept = d }}
	_ = p.Unlock(nil, rs)
	return slept, in.retryAfter
}

// A user-scope 429 is waited out for its body's retry_after, disgo is told
// it is over, and disgo can still read the body, while the original is
// closed so its connection is not leaked.
func TestAUserScope429WaitsItsRetryAfter(t *testing.T) {
	payload := `{"message": "You are being rate limited.", "retry_after": 0.037, "global": false}`
	rs, orig := answer(429, map[string]string{"Retry-After": "1", "X-RateLimit-Reset-After": "0.25", "X-RateLimit-Scope": "user"}, payload)
	slept, told := unlock(rs)
	if slept != 37*time.Millisecond || told != "0" {
		t.Fatalf("slept %v, disgo told %q", slept, told)
	}
	if got, _ := io.ReadAll(rs.Body); string(got) != payload || !orig.closed {
		t.Fatalf("body after %q, original closed %v", got, orig.closed)
	}
}

// A user-scope 429 is the route's own per-user bucket, so its reset, when
// shorter than retry_after, is when it reopens. A 429 with no scope gets
// retry_after whatever the reset says.
func TestOnlyAUserScope429TrustsItsReset(t *testing.T) {
	user, _ := answer(429, map[string]string{"Retry-After": "1", "X-RateLimit-Reset-After": "0.03", "X-RateLimit-Scope": "user"}, `{"retry_after": 0.3}`)
	if slept, _ := unlock(user); slept != 30*time.Millisecond {
		t.Errorf("user scope slept %v, want the reset", slept)
	}
	strict, _ := answer(429, map[string]string{"Retry-After": "1", "X-RateLimit-Reset-After": "0.03", "X-RateLimit-Scope": "user"}, `{"retry_after": 0.3}`)
	if slept, _ := unlockWith(strict, false); slept != 300*time.Millisecond {
		t.Errorf("without userReset slept %v, want retry_after", slept)
	}
	bare, _ := answer(429, map[string]string{"Retry-After": "1", "X-RateLimit-Reset-After": "0.03"}, `{"retry_after": 0.3}`)
	if slept, _ := unlock(bare); slept != 300*time.Millisecond {
		t.Errorf("no scope slept %v, want retry_after", slept)
	}
}

// Discord's own documented shared 429: a reset far shorter than the wait.
// Trusting the reset would retry twenty minutes early, so it is left to
// disgo, and the body is left where it was.
func TestASharedScope429IsLeftToDisgo(t *testing.T) {
	payload := `{"message": "You are being rate limited.", "retry_after": 1336.57, "global": false}`
	rs, orig := answer(429, map[string]string{"Retry-After": "1337", "X-RateLimit-Reset-After": "64.57", "X-RateLimit-Scope": "shared"}, payload)
	slept, told := unlock(rs)
	if slept >= 0 || told != "1337" {
		t.Fatalf("slept %v, disgo told %q", slept, told)
	}
	if got, _ := io.ReadAll(rs.Body); string(got) != payload || orig.closed {
		t.Fatal("a shared 429's body was touched")
	}
}

// Anything else passes through as it came.
func TestEverythingElsePassesThrough(t *testing.T) {
	user := map[string]string{"Retry-After": "1", "X-RateLimit-Scope": "user"}
	for name, c := range map[string]struct {
		code    int
		h       map[string]string
		payload string
	}{
		"ok":             {204, map[string]string{"X-RateLimit-Reset-After": "0.25"}, ""},
		"global":         {429, map[string]string{"Retry-After": "1", "X-RateLimit-Global": "true"}, `{"retry_after": 0.03}`},
		"global scope":   {429, map[string]string{"Retry-After": "1", "X-RateLimit-Scope": "global"}, `{"retry_after": 0.03}`},
		"no retry_after": {429, user, `{"message": "x"}`},
		"not json":       {429, user, `<html>`},
		"longer anyway":  {429, map[string]string{"Retry-After": "2", "X-RateLimit-Scope": "user"}, `{"retry_after": 2.5}`},
		"no Retry-After": {429, map[string]string{"X-RateLimit-Scope": "user"}, `{"retry_after": 0.03}`},
		"negative":       {429, user, `{"retry_after": -1}`},
	} {
		rs, _ := answer(c.code, c.h, c.payload)
		if slept, told := unlock(rs); slept >= 0 || told != rs.Header.Get("Retry-After") {
			t.Errorf("%s: slept %v, disgo told %q", name, slept, told)
		}
	}
	cf, _ := answer(429, map[string]string{"Retry-After": "1"}, `{"retry_after": 0.03}`)
	cf.Header.Del("Via")
	if slept, _ := unlock(cf); slept >= 0 {
		t.Error("cloudflare: shortened")
	}
	if slept, _ := unlock(nil); slept >= 0 {
		t.Error("no answer: shortened")
	}
	broken, _ := answer(429, user, "")
	broken.Body = io.NopCloser(errReader{})
	if slept, _ := unlock(broken); slept >= 0 {
		t.Error("an unreadable body: shortened")
	}
	if Precise(&inner{}, false) == nil {
		t.Fatal("no limiter")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("broken") }

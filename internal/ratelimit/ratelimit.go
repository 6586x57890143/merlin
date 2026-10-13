// Package ratelimit makes disgo wait out a 429 for as long as Discord says,
// not a whole second more.
//
// disgo reads a 429's wait from Retry-After, which Discord rounds up to
// whole seconds, so a 429 with 30ms left on it holds the bucket for a full
// second. skua's tools/reactbench measured what that costs: Discord's per-user
// reaction limit draws a 429 every few birds, and each one cost a second,
// which was most of a fill. The 429's body carries the real wait,
// retry_after, to the millisecond.
package ratelimit

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/disgoorg/disgo/rest"
)

// maxBody bounds what is read of a 429's body; Discord's is a few dozen bytes.
const maxBody = 4 << 10

type precise struct {
	rest.RateLimiter
	userReset bool
	sleep     func(time.Duration)
}

// Precise wraps inner. On a 429 whose body gives a wait shorter than
// Retry-After, it waits that while it still holds the bucket, then tells
// inner the wait is over, so the bucket reopens when Discord does. A
// global, shared or Cloudflare 429 goes to inner untouched: those waits are
// long or not the route's, and being a second late there costs nothing.
//
// userReset also trusts a user-scope 429's X-RateLimit-Reset-After when it is
// shorter than retry_after; see routeWait. skua's tools/reactbench -compare policy
// measures both.
func Precise(inner rest.RateLimiter, userReset bool) rest.RateLimiter {
	return &precise{RateLimiter: inner, userReset: userReset, sleep: time.Sleep}
}

func (p *precise) Unlock(ep *rest.CompiledEndpoint, rs *http.Response) error {
	if wait, ok := routeWait(rs, p.userReset); ok {
		p.sleep(wait)
		rs.Header.Set("Retry-After", "0")
	}
	return p.RateLimiter.Unlock(ep, rs)
}

// routeWait is a route 429's precise wait, when it is shorter than what
// Retry-After would make disgo wait: the body's retry_after, which Discord
// documents as the wait for any 429. With userReset, a user-scope 429's
// X-RateLimit-Reset-After wins when shorter: that 429 is the route's own
// per-user bucket, and live its reset ran around 40ms against retry_after's
// 300ms. Anywhere else the reset is not the wait (a shared 429's can be far
// shorter than the limit that was hit). It reads the body and puts a copy
// back, since disgo reads it after Unlock. disgo takes an answer with no via
// header for Cloudflare's, and so does this.
func routeWait(rs *http.Response, userReset bool) (time.Duration, bool) {
	if rs == nil || rs.StatusCode != http.StatusTooManyRequests || rs.Body == nil ||
		rs.Header.Get("X-RateLimit-Global") != "" || rs.Header.Get("via") == "" ||
		rs.Header.Get("X-RateLimit-Scope") == "shared" || rs.Header.Get("X-RateLimit-Scope") == "global" {
		return 0, false
	}
	whole, err := strconv.Atoi(rs.Header.Get("Retry-After"))
	if err != nil {
		return 0, false
	}
	body, err := io.ReadAll(io.LimitReader(rs.Body, maxBody))
	// The original is closed here: disgo's deferred close reaches only the
	// copy put back in its place.
	_ = rs.Body.Close()
	rs.Body = io.NopCloser(bytes.NewReader(body))
	var b struct {
		RetryAfter *float64 `json:"retry_after"`
	}
	if err != nil || json.Unmarshal(body, &b) != nil || b.RetryAfter == nil || *b.RetryAfter < 0 || *b.RetryAfter >= float64(whole) {
		return 0, false
	}
	wait := *b.RetryAfter
	if reset, err := strconv.ParseFloat(rs.Header.Get("X-RateLimit-Reset-After"), 64); err == nil && userReset && rs.Header.Get("X-RateLimit-Scope") == "user" && reset >= 0 {
		wait = min(wait, reset)
	}
	return time.Duration(wait * float64(time.Second)), true
}

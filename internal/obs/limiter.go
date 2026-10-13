package obs

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/disgoorg/disgo/rest"
)

// limiter records how long disgo's rate limiter held each request back.
type limiter struct {
	rest.RateLimiter
	rec *Recorder
}

// Limiter wraps inner, recording each request's wait for its bucket as wait
// against its route, so /perf splits Discord's pacing from the round trip
// the transport times. From skua.
func Limiter(inner rest.RateLimiter, rec *Recorder) rest.RateLimiter {
	return &limiter{RateLimiter: inner, rec: rec}
}

func (l *limiter) Wait(ctx context.Context, ep *rest.CompiledEndpoint) error {
	start := time.Now()
	err := l.RateLimiter.Wait(ctx, ep)
	path := ep.URL
	if u, perr := url.Parse(ep.URL); perr == nil {
		path = u.Path
	}
	l.rec.Add(Route(ep.Endpoint.Method, path), Wait, time.Since(start))
	return err
}

func (l *limiter) Unlock(ep *rest.CompiledEndpoint, rs *http.Response) error {
	return l.RateLimiter.Unlock(ep, rs)
}

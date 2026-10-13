package obs

import (
	"context"
	"testing"
	"time"

	"github.com/disgoorg/disgo/rest"
)

type slowBucket struct{ rest.RateLimiter }

func (slowBucket) Wait(context.Context, *rest.CompiledEndpoint) error {
	time.Sleep(20 * time.Millisecond)
	return nil
}

// The time a request spends held for its bucket is recorded as wait, by
// route with the IDs dropped.
func TestLimiterRecordsTheWait(t *testing.T) {
	rec := New()
	l := Limiter(slowBucket{}, rec)
	ep := rest.CreateMessage.Compile(nil, 1549707140753793054)
	if err := l.Wait(context.Background(), ep); err != nil {
		t.Fatal(err)
	}
	for _, r := range rec.Rows() {
		if r.Who == "post channels/messages" && r.Stage == Wait && r.P50 >= 15*time.Millisecond {
			return
		}
	}
	t.Fatalf("no wait recorded for post channels/messages: %+v", rec.Rows())
}

package obs

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestQuantilesAreWithinABucket(t *testing.T) {
	r := New()
	for i := 1; i <= 100; i++ {
		r.Add("p", Run, time.Duration(i)*time.Millisecond)
	}
	rows := r.Rows()
	if len(rows) != 1 || rows[0].N != 100 {
		t.Fatalf("rows %+v", rows)
	}
	within := func(got, want time.Duration) bool { return got > want*7/10 && got < want*13/10 }
	if !within(rows[0].P50, 50*time.Millisecond) || !within(rows[0].P99, 99*time.Millisecond) {
		t.Errorf("p50 %v p99 %v", rows[0].P50, rows[0].P99)
	}
	if rows[0].Max != 100*time.Millisecond {
		t.Errorf("max %v", rows[0].Max)
	}
}

func TestRowsAreSlowestFirst(t *testing.T) {
	r := New()
	r.Add("fast", Run, time.Millisecond)
	r.Add("slow", HTTP, time.Second)
	if rows := r.Rows(); rows[0].Who != "slow" {
		t.Errorf("first row %q, want the slowest", rows[0].Who)
	}
}

// One route is one row however many channels and interactions it touched:
// IDs and tokens never become part of the name.
func TestRouteDropsIDsAndTokens(t *testing.T) {
	for path, want := range map[string]string{
		"/api/v10/channels/123456789012345678/messages":                                  "post channels/messages",
		"/api/v10/interactions/123456789012345678/aW50ZXJhY3Rpb246MTIzNDU2Nzg5/callback": "post interactions/callback",
		"/api/v10/applications/1/guilds/2/commands":                                      "post guilds/commands",
	} {
		if got := Route("POST", path); got != want {
			t.Errorf("Route(%q) = %q, want %q", path, got, want)
		}
	}
}

type answer struct {
	status int
	header http.Header
}

func (a answer) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: a.status, Header: a.header, Body: io.NopCloser(strings.NewReader("{}")), Request: r}, nil
}

// The transport times the round trip only: a 429's wait is the limiter's
// to record, so counting it here as well would count it twice.
func TestTransportTimesRequests(t *testing.T) {
	r := New()
	c := &http.Client{Transport: Transport(answer{429, http.Header{"Retry-After": {"1.5"}}}, r)}
	resp, err := c.Post("https://discord.com/api/v10/channels/1/messages", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	var http_, wait Row
	for _, row := range r.Rows() {
		switch row.Stage {
		case HTTP:
			http_ = row
		case Wait:
			wait = row
		}
	}
	if http_.N != 1 || http_.Who != "post channels/messages" {
		t.Errorf("http row %+v", http_)
	}
	if wait.N != 0 {
		t.Errorf("the transport recorded a wait: %+v", wait)
	}
}

func TestSinceReadsTheSnowflakeClock(t *testing.T) {
	// 2015-01-01 plus one day, in Discord's epoch milliseconds, shifted in.
	id := uint64(86_400_000) << 22
	d, ok := Since(strconvU(id))
	if !ok || d < 24*time.Hour {
		t.Errorf("Since = %v %v", d, ok)
	}
	if _, ok := Since("not a snowflake"); ok {
		t.Error("garbage read as a time")
	}
}

func strconvU(v uint64) string {
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

// The debug listener only ever binds loopback.
func TestServeRefusesAnythingButLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:6060", ":6060", "10.0.0.1:6060", "example.com:6060", "nonsense"} {
		if err := Serve(addr); err == nil {
			t.Errorf("Serve(%q) was allowed", addr)
		}
	}
}

func TestRuntimeReportsSomething(t *testing.T) {
	sched, gc := Runtime()
	if sched < 0 || gc < 0 {
		t.Errorf("sched %v gc %v", sched, gc)
	}
}

// A sample has to be cheap enough to leave on: no allocation once its row
// exists.
func BenchmarkAdd(b *testing.B) {
	r := New()
	r.Add("p", Run, time.Millisecond)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			r.Add("p", Run, time.Millisecond)
		}
	})
}

func TestAddDoesNotAllocate(t *testing.T) {
	r := New()
	r.Add("p", Run, time.Millisecond)
	if n := testing.AllocsPerRun(100, func() { r.Add("p", Run, time.Millisecond) }); n != 0 {
		t.Errorf("Add allocates %v times", n)
	}
}

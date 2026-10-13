package discordguard

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
)

// wire records what reaches Discord and answers every request with body.
type wire struct {
	mu       sync.Mutex
	requests []string
	body     string
}

func (w *wire) RoundTrip(r *http.Request) (*http.Response, error) {
	var b []byte
	if r.Body != nil {
		b, _ = io.ReadAll(r.Body)
	}
	w.mu.Lock()
	w.requests = append(w.requests, r.Method+" "+r.URL.String()+" "+string(b))
	w.mu.Unlock()
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(w.body)),
		Header: http.Header{"Content-Type": []string{"application/json"}}, Request: r}, nil
}

func restGuard(gate GuildGate, body string) (*Guard, *wire) {
	w := &wire{body: body}
	g := New(nil, gate, testLogger(), false).WithRest(rest.New(rest.NewClient("t", rest.WithHTTPClient(&http.Client{Transport: w}))))
	return g, w
}

// A reposted member's text pings nobody, whatever the caller asked for.
func TestWebhookPostsPingNobody(t *testing.T) {
	g, w := restGuard(fakeGate{}, `{"id":"9","channel_id":"2","content":"x","author":{"id":"3","username":"u"}}`)
	id, err := g.For("100").CreateWebhookMessage("5", "tok", discord.WebhookMessageCreate{
		Content: "@everyone hi", AllowedMentions: &discord.AllowedMentions{Parse: []discord.AllowedMentionType{discord.AllowedMentionTypeEveryone}},
	}, true)
	if err != nil || id == nil || id.ID != 9 {
		t.Fatalf("posted %+v, err %v", id, err)
	}
	if len(w.requests) != 1 || !strings.Contains(w.requests[0], `"allowed_mentions":{"parse":[],`) ||
		!strings.Contains(w.requests[0], "/webhooks/5/tok") || !strings.Contains(w.requests[0], "wait=true") {
		t.Fatalf("sent %v", w.requests)
	}
}

// A paused guild's writes never reach Discord; its reads still do.
func TestPausedWritesStayHome(t *testing.T) {
	g, w := restGuard(fakeGate{paused: map[string]bool{"100": true}}, `[]`)
	ops := g.For("100")
	if err := ops.DeleteMessage("2", "3"); !Skipped(err) {
		t.Fatalf("a paused delete returned %v", err)
	}
	until := time.Now().Add(time.Hour)
	if err := ops.TimeoutMember("100", "3", &until); !Skipped(err) {
		t.Fatalf("a paused timeout returned %v", err)
	}
	if len(w.requests) != 0 {
		t.Fatalf("paused writes reached Discord: %v", w.requests)
	}
	if _, err := ops.GetMessages("2", "", 10); err != nil || len(w.requests) != 1 {
		t.Fatalf("a read was refused: %v %v", err, w.requests)
	}
}

// Lifting a timeout sends null, which is what clears it.
func TestTimeoutNilLiftsIt(t *testing.T) {
	g, w := restGuard(fakeGate{}, `{"user":{"id":"3","username":"u"},"roles":[],"joined_at":"2026-01-01T00:00:00Z"}`)
	if err := g.For("100").TimeoutMember("100", "3", nil); err != nil {
		t.Fatal(err)
	}
	if len(w.requests) != 1 || !strings.Contains(w.requests[0], `"communication_disabled_until":null`) {
		t.Fatalf("sent %v", w.requests)
	}
}

// An ID that is not a snowflake is refused before anything is sent.
func TestUnparseableIDsAreRefused(t *testing.T) {
	g, w := restGuard(fakeGate{}, `{}`)
	if err := g.For("100").DeleteMessage("not-an-id", "3"); err == nil || len(w.requests) != 0 {
		t.Fatalf("err %v, sent %v", err, w.requests)
	}
}

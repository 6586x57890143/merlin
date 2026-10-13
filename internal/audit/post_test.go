package audit

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/disgoorg/disgo/rest"
)

type wire struct{ sent []string }

func (w *wire) RoundTrip(r *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(r.Body)
	w.sent = append(w.sent, r.URL.Path+" "+string(b))
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"id":"9","channel_id":"2"}`)),
		Header: http.Header{"Content-Type": []string{"application/json"}}, Request: r}, nil
}

// An audit post carries the mood icon its embed points at and pings
// nobody, whatever guild text the values carry.
func TestPostAttachesItsIconAndPingsNobody(t *testing.T) {
	w := &wire{}
	wr := New(nil, rest.New(rest.NewClient("t", rest.WithHTTPClient(&http.Client{Transport: w}))), nil)
	if err := wr.post(2, buildEmbed("418", "config.admin_added", "", "<@99> @everyone")); err != nil {
		t.Fatal(err)
	}
	if len(w.sent) != 1 {
		t.Fatalf("sent %v", w.sent)
	}
	got := w.sent[0]
	for _, want := range []string{"/channels/2/messages", `"allowed_mentions":{"parse":[]`, `filename="merlin_ok.png"`, "attachment://merlin_ok.png", `filename="merlin_rule.png"`} {
		if !strings.Contains(got, want) {
			t.Errorf("post lacks %s", want)
		}
	}
}

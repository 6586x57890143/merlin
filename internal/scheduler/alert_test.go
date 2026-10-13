package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/disgoorg/disgo/rest"
)

type statusIn string

func (c statusIn) StatusChannelID(string) string { return string(c) }

type wire struct{ sent []string }

func (w *wire) RoundTrip(r *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(r.Body)
	w.sent = append(w.sent, r.URL.Path+" "+string(b))
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"id":"9","channel_id":"2"}`)),
		Header: http.Header{"Content-Type": []string{"application/json"}}, Request: r}, nil
}

// A failing job's alert reaches the status channel, names the job without
// its guild prefix, and pings nothing its error text mentions.
func TestAlertPostsToTheStatusChannel(t *testing.T) {
	w := &wire{}
	s := New(newFakeStore(), statusIn("2"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.rest = rest.New(rest.NewClient("t", rest.WithHTTPClient(&http.Client{Transport: w})))
	s.alert(context.Background(), "100:rotation", 3, errors.New("<@&5> is gone"))
	if len(w.sent) != 1 {
		t.Fatalf("sent %v", w.sent)
	}
	for _, want := range []string{"/channels/2/messages", `"allowed_mentions":{"parse":[]`, `"value":"rotation"`, `filename="merlin_error.png"`} {
		if !strings.Contains(w.sent[0], want) {
			t.Errorf("alert lacks %s", want)
		}
	}
}

// No status channel, or one that is not a snowflake, sends nothing.
func TestAlertWithoutAChannelSendsNothing(t *testing.T) {
	w := &wire{}
	for _, ch := range []string{"", "general"} {
		s := New(newFakeStore(), statusIn(ch), slog.New(slog.NewTextHandler(io.Discard, nil)))
		s.rest = rest.New(rest.NewClient("t", rest.WithHTTPClient(&http.Client{Transport: w})))
		s.alert(context.Background(), "100:rotation", 3, errors.New("x"))
	}
	if len(w.sent) != 0 {
		t.Fatalf("sent %v", w.sent)
	}
}

package audit

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/disgoorg/disgo/rest"
)

type wire struct{ sent []string }

func (w *wire) RoundTrip(r *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(r.Body)
	w.sent = append(w.sent, r.Method+" "+r.URL.Path+" "+string(b))
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"id":"9","channel_id":"2"}`)),
		Header: http.Header{"Content-Type": []string{"application/json"}}, Request: r}, nil
}

type channels map[string]string

func (c channels) AuditLogChannelID(guildID string) string { return c[guildID] }

func writer(w *wire, ch channels) *Writer {
	return New(nil, rest.New(rest.NewClient("t", rest.WithHTTPClient(&http.Client{Transport: w}))), ch)
}

// A card goes out as Components V2 wearing merlin's face for its mood and
// her rule, and pings nobody, whatever member text it carries; the edit
// that grows it pings nobody either, and replaces the images rather than
// adding another copy of each.
func TestCardsPingNobodyAndEditInPlace(t *testing.T) {
	w := &wire{}
	wr := writer(w, channels{"100": "2"})
	ctx := context.Background()
	if err := wr.cards.Add(ctx, toEntry("100", "418", "roles.jail", "", `user=<@99> reason="@everyone"`, at)); err != nil {
		t.Fatal(err)
	}
	if err := wr.cards.Add(ctx, toEntry("100", "418", "roles.jail", "", "user=<@98>", at)); err != nil {
		t.Fatal(err)
	}
	if len(w.sent) != 2 {
		t.Fatalf("sent %v", w.sent)
	}
	post, edit := w.sent[0], w.sent[1]
	for _, want := range []string{"POST /api/v10/channels/2/messages", `"allowed_mentions":{"parse":[]`, `"flags":32768`, `"type":17`, "member jailed"} {
		if !strings.Contains(post, want) {
			t.Errorf("post lacks %s:\n%s", want, post)
		}
	}
	for _, msg := range []string{post, edit} {
		for _, want := range []string{"attachment://merlin_warn.png", "attachment://merlin_rule.png", `filename="merlin_warn.png"`, `filename="merlin_rule.png"`} {
			if !strings.Contains(msg, want) {
				t.Errorf("card lacks %s:\n%s", want, msg)
			}
		}
	}
	if !strings.Contains(edit, `"attachments":[]`) {
		t.Errorf("an edit must replace the attachments:\n%s", edit)
	}
	for _, want := range []string{"PATCH /api/v10/channels/2/messages/9", `"allowed_mentions":{"parse":[]`, "roles · 2", `\u003c@98\u003e`} {
		if !strings.Contains(edit, want) {
			t.Errorf("edit lacks %s:\n%s", want, edit)
		}
	}
}

// A guild with no log channel set says so, and sends nothing.
func TestNoChannelIsAnError(t *testing.T) {
	w := &wire{}
	err := writer(w, channels{}).cards.Add(context.Background(), toEntry("100", "418", "roles.jail", "", "", at))
	if err == nil || !strings.Contains(err.Error(), "no audit log channel") || len(w.sent) != 0 {
		t.Fatalf("err %v, sent %v", err, w.sent)
	}
}

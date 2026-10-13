package core

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
)

// answering decodes an interaction of typ whose answers land on cap.
func answering(t *testing.T, typ int, data map[string]any) (*Ix, *captureTransport) {
	t.Helper()
	cap := &captureTransport{}
	ix := decode(t, map[string]any{"type": typ, "data": data})
	ix.Rest = rest.New(rest.NewClient("test", rest.WithHTTPClient(&http.Client{Transport: cap})))
	return ix, cap
}

// Discord fixes ephemerality when an interaction is acknowledged, so the
// difference between these two is decided at defer time and cannot be
// corrected by the follow-up that lands afterwards. A tip jar deferred
// privately stays private no matter what is edited into it, which is why the
// public variant exists as a separate call rather than a flag further down.
func TestDeferPublicIsNotEphemeral(t *testing.T) {
	const ephemeralFlag = 64
	for _, tc := range []struct {
		name     string
		deferral func(*Ix) error
		private  bool
	}{
		{"private", (*Ix).Defer, true},
		{"public", (*Ix).DeferPublic, false},
	} {
		ix, cap := answering(t, 2, map[string]any{"id": "6", "name": "x", "type": 1})
		if err := tc.deferral(ix); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(cap.bodies) != 1 {
			t.Fatalf("%s: sent %d requests, want 1", tc.name, len(cap.bodies))
		}
		var sent struct {
			Type int `json:"type"`
			Data *struct {
				Flags int `json:"flags"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(cap.bodies[0]), &sent); err != nil {
			t.Fatalf("%s: %v in %s", tc.name, err, cap.bodies[0])
		}
		got := sent.Data != nil && sent.Data.Flags&ephemeralFlag != 0
		if sent.Type != 5 || got != tc.private {
			t.Errorf("%s: type %d, ephemeral %v: %s", tc.name, sent.Type, got, cap.bodies[0])
		}
	}
}

// An edit that omits attachments keeps what the message had and appends the
// new upload, until Discord's cap of ten refuses the eleventh, so every edit
// says "attachments: []" and replaces the last.
func TestEditsReplaceAttachments(t *testing.T) {
	ix, cap := answering(t, 2, map[string]any{"id": "6", "name": "x", "type": 1})
	if err := ix.FollowUpEmbed(Embed(ColorInfo, "Still counting", "9369 messages so far")); err != nil {
		t.Fatal(err)
	}
	pn := Panel{Color: ColorInfo, Title: "t", Parts: []discord.ContainerSubComponent{discord.TextDisplayComponent{Content: "x"}}}
	if err := ix.FollowUpPanel(pn); err != nil {
		t.Fatal(err)
	}
	click, ccap := answering(t, 3, map[string]any{"custom_id": "c", "component_type": 2})
	if err := click.UpdatePanel(pn); err != nil {
		t.Fatal(err)
	}
	for _, body := range append(cap.bodies, ccap.bodies...) {
		if !strings.Contains(body, `"attachments":[]`) {
			t.Errorf("edit does not clear the previous upload:\n%.400s", body)
		}
	}
}

// Every panel goes out as Components V2, accented, and pinging nobody,
// whatever its text holds.
func TestPanelsGoOutAsComponentsV2AndPingNobody(t *testing.T) {
	pn := Panel{Color: ColorInfo, Title: "t", Parts: []discord.ContainerSubComponent{discord.TextDisplayComponent{Content: "<@1>"}}}
	ix, cap := answering(t, 2, map[string]any{"id": "6", "name": "x", "type": 1})
	if err := ix.RespondPanel(pn); err != nil {
		t.Fatal(err)
	}
	click, ccap := answering(t, 3, map[string]any{"custom_id": "c", "component_type": 2})
	if err := click.UpdatePanel(pn); err != nil {
		t.Fatal(err)
	}
	for _, body := range append(cap.bodies, ccap.bodies...) {
		for _, want := range []string{`"parse":[]`, `"accent_color"`, `"flags":32768`} {
			if want == `"flags":32768` && strings.Contains(body, `"flags":32832`) {
				continue // the fresh one is private as well
			}
			if !strings.Contains(body, want) {
				t.Errorf("request missing %s:\n%.400s", want, body)
			}
		}
	}
}

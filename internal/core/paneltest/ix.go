package paneltest

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/disgoorg/disgo/rest"

	"github.com/6586x57890143/merlin/internal/core"
)

// The builders below are Click, Submit and a slash command for a plugin on
// disgo: each is Discord's JSON for the interaction, decoded by core.NewIx
// exactly as the router decodes a real one, answered through a REST client
// whose requests land on a Wire. disgo's IDs are snowflakes, so the guild
// and members here are numeric: IxGuild, and whatever user ID a test names.

// IxGuild is the guild every Ix builder's interaction comes from.
const IxGuild = "100"

// Rest is a disgo REST client that sends nothing anywhere.
func Rest() (rest.Rest, *Wire) {
	w := &Wire{}
	return rest.New(rest.NewClient("test", rest.WithHTTPClient(&http.Client{Transport: w}))), w
}

// payload is the parts every interaction carries, from user in IxGuild.
func payload(typ int, user string, data map[string]any) map[string]any {
	return map[string]any{
		"id": "1000", "application_id": "2", "type": typ, "token": "t", "version": 1,
		"guild_id": IxGuild,
		"channel":  map[string]any{"id": "101", "type": 0},
		"member": map[string]any{
			"user":        map[string]any{"id": user, "username": "member"},
			"permissions": "0", "roles": []any{}, "joined_at": "2026-01-01T00:00:00Z",
		},
		"data": data,
	}
}

func decode(t testing.TB, p map[string]any) (*core.Ix, *Wire) {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	r, w := Rest()
	ix, err := core.NewIx(raw, r)
	if err != nil {
		t.Fatal(err)
	}
	return ix, w
}

// IxCommand is user running /name. edit changes the raw payload before it
// is decoded, which is where a test adds options:
// p["data"].(map[string]any)["options"] = ...
func IxCommand(t testing.TB, user, name string, edit func(p map[string]any)) (*core.Ix, *Wire) {
	t.Helper()
	p := payload(2, user, map[string]any{"id": "6", "name": name, "type": 1})
	if edit != nil {
		edit(p)
	}
	return decode(t, p)
}

// IxClick is user pressing the button customID on pn, as Discord hands the
// panel back. A nil pn is a click on a message with no components.
func IxClick(t testing.TB, user, customID string, pn *core.Panel) (*core.Ix, *Wire) {
	t.Helper()
	return IxComponent(t, user, customID, 2, pn)
}

// IxPick is user submitting the string select customID on pn with values.
func IxPick(t testing.TB, user, customID string, pn *core.Panel, values ...string) (*core.Ix, *Wire) {
	t.Helper()
	return IxComponent(t, user, customID, 3, pn, values...)
}

// IxComponent is a component interaction of Discord's componentType (2 a
// button, 3 a string select, 5 a user, 6 a role, 8 a channel select).
func IxComponent(t testing.TB, user, customID string, componentType int, pn *core.Panel, values ...string) (*core.Ix, *Wire) {
	t.Helper()
	data := map[string]any{"custom_id": customID, "component_type": componentType}
	if values != nil {
		data["values"] = values
	}
	p := payload(3, user, data)
	msg := map[string]any{
		"id": "102", "channel_id": "101", "content": "", "flags": 64,
		"author": map[string]any{"id": "2", "username": "merlin"},
	}
	if pn != nil {
		msg["components"] = pn.Layout()
	}
	p["message"] = msg
	return decode(t, p)
}

// IxSubmit is user submitting the form customID with fields.
func IxSubmit(t testing.TB, user, customID string, fields map[string]string) (*core.Ix, *Wire) {
	t.Helper()
	var rows []any
	for id, v := range fields {
		rows = append(rows, map[string]any{"type": 1, "components": []any{
			map[string]any{"type": 4, "custom_id": id, "value": v},
		}})
	}
	return decode(t, payload(5, user, map[string]any{"custom_id": customID, "components": rows}))
}

package ui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/disgoorg/disgo/discord"
)

// Each builder draws exactly what Discord was sent before disgo: these are
// the payloads the discordgo builders produced, byte for byte. A change
// here changes what members see on every panel.
func TestEachBuilderDrawsWhatItAlwaysDrew(t *testing.T) {
	opts := []discord.StringSelectMenuOption{{Label: "a", Value: "a", Default: true}, {Label: "b", Value: "b"}}
	ids := []string{"5", "", "6"}
	for _, c := range []struct {
		name string
		got  any
		want string
	}{
		{"text", Text("hi"), `{"type":10,"content":"hi"}`},
		{"gap", Gap(), `{"type":14}`},
		{"button", Button("go", "x:go", discord.ButtonStyleDanger, true), `{"type":2,"style":4,"label":"go","custom_id":"x:go","disabled":true}`},
		{"row", Row(Button("go", "x", discord.ButtonStylePrimary, false)), `{"type":1,"components":[{"type":2,"style":1,"label":"go","custom_id":"x"}]}`},
		{"switch", Switch("on", Button("off", "x", discord.ButtonStyleSecondary, false)), `{"type":9,"components":[{"type":10,"content":"on"}],"accessory":{"type":2,"style":2,"label":"off","custom_id":"x"}}`},
		{"string select", StringSelect("s", "pick", opts, true, false), `{"type":3,"custom_id":"s","placeholder":"pick","min_values":0,"max_values":2,"options":[{"label":"a","value":"a","default":true},{"label":"b","value":"b"}],"required":false,"disabled":false}`},
		{"single string select", StringSelect("s", "pick", opts, false, true), `{"type":3,"custom_id":"s","placeholder":"pick","options":[{"label":"a","value":"a","default":true},{"label":"b","value":"b"}],"required":false,"disabled":true}`},
		{"user select", UserSelect("u", "who", ids, 5, false), `{"type":5,"custom_id":"u","placeholder":"who","default_values":[{"type":"user","id":"5"},{"type":"user","id":"6"}],"min_values":0,"max_values":5,"required":false,"disabled":false}`},
		{"role select", RoleSelect("r", "which", ids[:1], 1, false), `{"type":6,"custom_id":"r","placeholder":"which","default_values":[{"type":"role","id":"5"}],"max_values":1,"required":false,"disabled":false}`},
		{"channel select, many", ChannelSelectMany("c", "which", ids, 25, true), `{"type":8,"custom_id":"c","placeholder":"which","default_values":[{"type":"channel","id":"5"},{"type":"channel","id":"6"}],"min_values":0,"max_values":25,"required":false,"disabled":true}`},
		{"channel select", ChannelSelect("c", "where", "7", false, discord.ChannelTypeGuildText), `{"type":8,"custom_id":"c","placeholder":"where","default_values":[{"type":"channel","id":"7"}],"max_values":1,"channel_types":[0],"required":false,"disabled":false}`},
		{"clearable", Clearable(ChannelSelect("c", "where", "", false)), `{"type":8,"custom_id":"c","placeholder":"where","min_values":0,"max_values":1,"required":false,"disabled":false}`},
		{"pagination", PaginationRow("p:", 1, 3), `[{"type":1,"components":[{"type":2,"style":2,"label":"◀ Prev","custom_id":"p:0"},{"type":2,"style":2,"label":"Page 2/3","custom_id":"p:noop","disabled":true},{"type":2,"style":2,"label":"Next ▶","custom_id":"p:2"}]}]`},
	} {
		raw, err := json.Marshal(c.got)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != c.want {
			t.Errorf("%s:\n got  %s\n want %s", c.name, raw, c.want)
		}
	}
}

// One page needs no controls; at either end the button that would leave the
// list is disabled.
func TestPaginationRowBounds(t *testing.T) {
	if PaginationRow("p:", 0, 1) != nil {
		t.Error("one page drew controls")
	}
	buttons := func(page int) (prev, next discord.ButtonComponent) {
		row := PaginationRow("p:", page, 3)[0].(discord.ActionRowComponent)
		return row.Components[0].(discord.ButtonComponent), row.Components[2].(discord.ButtonComponent)
	}
	if prev, next := buttons(0); !prev.Disabled || next.Disabled {
		t.Errorf("first page: prev disabled %v, next disabled %v", prev.Disabled, next.Disabled)
	}
	if prev, next := buttons(2); prev.Disabled || !next.Disabled {
		t.Errorf("last page: prev disabled %v, next disabled %v", prev.Disabled, next.Disabled)
	}
}

// Field is the one builder that differs, on purpose: the label wraps the
// input. Everything the member sees and submits is the same.
func TestFieldIsALabelledInput(t *testing.T) {
	raw, err := json.Marshal(Field("code", "Prize code", "X", "paste it", true, true))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"type":18`, `"label":"Prize code"`, `"type":4`, `"custom_id":"code"`, `"style":2`, `"max_length":4000`, `"required":true`, `"value":"X"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("field is missing %s: %s", want, raw)
		}
	}
}

// One character over Discord's caps refuses the whole form, so a field is
// clipped to them rather than sent to be refused.
func TestFieldStaysInsideDiscordsCaps(t *testing.T) {
	f := Field("k", strings.Repeat("é", 60), "", strings.Repeat("x", 150), false, false)
	in := f.Component.(discord.TextInputComponent)
	if n := len([]rune(f.Label)); n != maxFieldLabel {
		t.Errorf("label is %d characters", n)
	}
	if n := len([]rune(in.Placeholder)); n != maxFieldPlaceholder {
		t.Errorf("placeholder is %d characters", n)
	}
}

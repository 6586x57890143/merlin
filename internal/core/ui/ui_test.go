package ui

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/disgoorg/disgo/discord"

	"github.com/6586x57890143/merlin/internal/core"
)

// wireOf is what x marshals to, with every false, zero and empty value
// dropped: Discord reads an absent field as its zero value, and the two
// libraries differ only in which zero values they write.
func wireOf(t *testing.T, x any) any {
	t.Helper()
	raw, err := json.Marshal(x)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return prune(v)
}

func prune(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if e = prune(e); e == nil {
				delete(x, k)
			} else {
				x[k] = e
			}
		}
		if len(x) == 0 {
			return nil
		}
	case []any:
		for n := range x {
			x[n] = prune(x[n])
		}
		if len(x) == 0 {
			return nil
		}
	case bool:
		if !x {
			return nil
		}
	case float64:
		if x == 0 {
			return nil
		}
	case string:
		if x == "" {
			return nil
		}
	}
	return v
}

func TestEachBuilderMatchesItsDiscordgoTwin(t *testing.T) {
	opts := []discordgo.SelectMenuOption{{Label: "a", Value: "a", Default: true}, {Label: "b", Value: "b"}}
	newOpts := []discord.StringSelectMenuOption{{Label: "a", Value: "a", Default: true}, {Label: "b", Value: "b"}}
	ids := []string{"5", "", "6"}
	for _, c := range []struct {
		name     string
		old, new any
	}{
		{"text", core.Text("hi"), Text("hi")},
		{"gap", core.Gap(), Gap()},
		{"button", core.Button("go", "x:go", discordgo.DangerButton, true), Button("go", "x:go", discord.ButtonStyleDanger, true)},
		{"row", core.Row(core.Button("go", "x", discordgo.PrimaryButton, false)), Row(Button("go", "x", discord.ButtonStylePrimary, false))},
		{"switch", core.Switch("on", core.Button("off", "x", discordgo.SecondaryButton, false)), Switch("on", Button("off", "x", discord.ButtonStyleSecondary, false))},
		{"string select", core.StringSelect("s", "pick", opts, true, false), StringSelect("s", "pick", newOpts, true, false)},
		{"single string select", core.StringSelect("s", "pick", opts, false, true), StringSelect("s", "pick", newOpts, false, true)},
		{"user select", core.EntitySelect(discordgo.UserSelectMenu, "u", "who", ids, 5, false), UserSelect("u", "who", ids, 5, false)},
		{"role select", core.EntitySelect(discordgo.RoleSelectMenu, "r", "which", ids[:1], 1, false), RoleSelect("r", "which", ids[:1], 1, false)},
		{"channel select", core.ChannelSelect("c", "where", "7", false, discordgo.ChannelTypeGuildText), ChannelSelect("c", "where", "7", false, discord.ChannelTypeGuildText)},
		{"clearable", core.Clearable(core.ChannelSelect("c", "where", "", false)), Clearable(ChannelSelect("c", "where", "", false))},
		{"pagination", core.PaginationRow("p:", 1, 3), PaginationRow("p:", 1, 3)},
	} {
		if o, n := wireOf(t, c.old), wireOf(t, c.new); !reflect.DeepEqual(o, n) {
			t.Errorf("%s:\n discordgo %v\n disgo     %v", c.name, o, n)
		}
	}
	if PaginationRow("p:", 0, 1) != nil {
		t.Error("one page drew controls")
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

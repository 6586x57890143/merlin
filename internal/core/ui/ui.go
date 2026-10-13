// Package ui is the panel builders of internal/core (Text, Row, Gap,
// Button, Switch, the selects, Field, PaginationRow) on disgo's types, for
// a plugin answering through a core.Ix and drawing a Panel's Parts. Each
// produces the same Discord JSON as its discordgo twin (ui_test.go), so a
// plugin moved onto them looks exactly as it did. The one exception is
// Field: disgo writes a form's label the way Discord now asks for it, as a
// Label wrapped around the input.
package ui

import (
	"fmt"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"

	"github.com/6586x57890143/merlin/internal/core"
)

// MaxSelectValues is Discord's cap on a select's options and picks.
const MaxSelectValues = core.MaxSelectValues

// Text is a TextDisplay.
func Text(s string) discord.TextDisplayComponent { return discord.TextDisplayComponent{Content: s} }

// Row is an ActionsRow.
func Row(c ...discord.InteractiveComponent) discord.ActionRowComponent {
	return discord.ActionRowComponent{Components: c}
}

// Gap is a small divider between a panel's sections.
func Gap() discord.SeparatorComponent { return discord.SeparatorComponent{} }

// Button is a panel button; a disabled one is still drawn.
func Button(label, customID string, style discord.ButtonStyle, disabled bool) discord.ButtonComponent {
	return discord.ButtonComponent{Label: label, Style: style, CustomID: customID, Disabled: disabled}
}

// Switch is a section with a button beside its text.
func Switch(text string, b discord.ButtonComponent) discord.SectionComponent {
	return discord.SectionComponent{Components: []discord.SectionSubComponent{Text(text)}, Accessory: b}
}

// StringSelect is a select over fixed options; multi lets any number be
// picked, none included.
func StringSelect(customID, placeholder string, opts []discord.StringSelectMenuOption, multi, disabled bool) discord.StringSelectMenuComponent {
	m := discord.StringSelectMenuComponent{CustomID: customID, Placeholder: placeholder, Options: opts[:min(len(opts), MaxSelectValues)], Disabled: disabled}
	if multi {
		zero := 0
		m.MinValues, m.MaxValues = &zero, len(m.Options)
	}
	return m
}

// defaults is ids preselected as kind, up to the cap, skipping any that
// is not a snowflake (an unset setting reads as "").
func defaults(kind discord.SelectMenuDefaultValueType, ids []string) []discord.SelectMenuDefaultValue {
	var out []discord.SelectMenuDefaultValue
	for _, id := range ids[:min(len(ids), MaxSelectValues)] {
		if sf, err := snowflake.Parse(id); err == nil {
			out = append(out, discord.SelectMenuDefaultValue{ID: sf, Type: kind})
		}
	}
	return out
}

// picks is the min/max pair EntitySelect sets: up to max, and none allowed
// once more than one is.
func picks(max int) (*int, int) {
	max = min(max, MaxSelectValues)
	if max > 1 {
		zero := 0
		return &zero, max
	}
	return nil, max
}

// UserSelect, RoleSelect and ChannelSelect are EntitySelect for each kind:
// ids preselected, so what changed comes back from Ix.SelectDiff.
func UserSelect(customID, placeholder string, ids []string, max int, disabled bool) discord.UserSelectMenuComponent {
	lo, hi := picks(max)
	return discord.UserSelectMenuComponent{CustomID: customID, Placeholder: placeholder, MinValues: lo, MaxValues: hi, Disabled: disabled,
		DefaultValues: defaults(discord.SelectMenuDefaultValueTypeUser, ids)}
}

func RoleSelect(customID, placeholder string, ids []string, max int, disabled bool) discord.RoleSelectMenuComponent {
	lo, hi := picks(max)
	return discord.RoleSelectMenuComponent{CustomID: customID, Placeholder: placeholder, MinValues: lo, MaxValues: hi, Disabled: disabled,
		DefaultValues: defaults(discord.SelectMenuDefaultValueTypeRole, ids)}
}

// ChannelSelectMany is EntitySelect for channels: up to max of them, ids
// preselected.
func ChannelSelectMany(customID, placeholder string, ids []string, max int, disabled bool) discord.ChannelSelectMenuComponent {
	lo, hi := picks(max)
	return discord.ChannelSelectMenuComponent{CustomID: customID, Placeholder: placeholder, MinValues: lo, MaxValues: hi, Disabled: disabled,
		DefaultValues: defaults(discord.SelectMenuDefaultValueTypeChannel, ids)}
}

// ChannelSelect is one channel of the given types, current preselected.
func ChannelSelect(customID, placeholder, current string, disabled bool, types ...discord.ChannelType) discord.ChannelSelectMenuComponent {
	lo, hi := picks(1)
	return discord.ChannelSelectMenuComponent{CustomID: customID, Placeholder: placeholder, MinValues: lo, MaxValues: hi, Disabled: disabled,
		DefaultValues: defaults(discord.SelectMenuDefaultValueTypeChannel, []string{current}), ChannelTypes: types}
}

// Clearable lets a single-value picker be emptied.
func Clearable[M discord.UserSelectMenuComponent | discord.RoleSelectMenuComponent | discord.ChannelSelectMenuComponent | discord.StringSelectMenuComponent](m M) M {
	zero := 0
	switch v := any(&m).(type) {
	case *discord.UserSelectMenuComponent:
		v.MinValues = &zero
	case *discord.RoleSelectMenuComponent:
		v.MinValues = &zero
	case *discord.ChannelSelectMenuComponent:
		v.MinValues = &zero
	case *discord.StringSelectMenuComponent:
		v.MinValues = &zero
	}
	return m
}

// Field is one text input in a form, prefilled with value, under its label.
func Field(id, label, value, placeholder string, paragraph, required bool) discord.LabelComponent {
	style, maxLen := discord.TextInputStyleShort, 200
	if paragraph {
		style, maxLen = discord.TextInputStyleParagraph, 4000
	}
	return discord.LabelComponent{Label: label, Component: discord.TextInputComponent{
		CustomID: id, Value: value, Placeholder: placeholder, Style: style, Required: required, MaxLength: maxLen,
	}}
}

// PaginationRow is the Prev/Next row for page (0-based) of totalPages, with
// whichever end is already at its limit disabled and a disabled "Page X/Y"
// between them for orientation. Nil for a single page: a short list stays
// as plain as it was before pagination existed.
func PaginationRow(prefix string, page, totalPages int) []discord.LayoutComponent {
	if totalPages <= 1 {
		return nil
	}
	return []discord.LayoutComponent{Row(
		Button("◀ Prev", core.PaginationCustomID(prefix, page-1), discord.ButtonStyleSecondary, page <= 0),
		Button(fmt.Sprintf("Page %d/%d", page+1, totalPages), prefix+"noop", discord.ButtonStyleSecondary, true),
		Button("Next ▶", core.PaginationCustomID(prefix, page+1), discord.ButtonStyleSecondary, page >= totalPages-1),
	)}
}

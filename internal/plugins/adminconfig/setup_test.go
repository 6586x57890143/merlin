package adminconfig

import (
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/settings"
)

// setupComponentIDs flattens a rendered step's components into the CustomIDs
// it actually offers, so tests can assert on the controls a step exposes
// without caring how they're arranged into rows.
func setupComponentIDs(t *testing.T, components []discordgo.MessageComponent) []string {
	t.Helper()
	var ids []string
	for _, row := range components {
		ar, ok := row.(discordgo.ActionsRow)
		if !ok {
			t.Fatalf("component %T is not an ActionsRow; Discord only accepts top-level rows", row)
		}
		for _, c := range ar.Components {
			switch c := c.(type) {
			case discordgo.Button:
				ids = append(ids, c.CustomID)
			case discordgo.SelectMenu:
				ids = append(ids, c.CustomID)
			default:
				t.Fatalf("unexpected component type %T in setup step", c)
			}
		}
	}
	return ids
}

func hasID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// TestSetupStepsOfferPickBeforeCreate is the whole point of the wizard: no
// step may create anything on its own, and every step that can create
// something must offer picking an existing thing first. A regression here
// would mean a server silently gaining a duplicate channel or role it never
// asked for.
func TestSetupStepsOfferPickBeforeCreate(t *testing.T) {
	for _, tc := range []struct {
		step              int
		wantSelect        string
		wantCreate        string
		wantSelectIsFirst bool
	}{
		{setupStepAuditLog, setupAuditLogSelectCustomID, setupAuditLogCreateCustomID, true},
		{setupStepStatus, setupStatusSelectCustomID, setupStatusCreateCustomID, true},
		{setupStepModRole, setupModRoleSelectCustomID, setupModRoleCreateCustomID, true},
	} {
		_, components := renderSetupStep(settings.GuildSettings{}, tc.step, "")
		ids := setupComponentIDs(t, components)
		if !hasID(ids, tc.wantSelect) {
			t.Errorf("step %d: missing picker %q, got %v", tc.step, tc.wantSelect, ids)
		}
		if !hasID(ids, tc.wantCreate) {
			t.Errorf("step %d: missing create button %q, got %v", tc.step, tc.wantCreate, ids)
		}
		if tc.wantSelectIsFirst && ids[0] != tc.wantSelect {
			t.Errorf("step %d: picker should come before the create button, got %v", tc.step, ids)
		}
	}
}

// TestSetupAdminsStepHasNoCreateButton guards the one step that must not
// offer a default: there's no sane "create an admin for me".
func TestSetupAdminsStepHasNoCreateButton(t *testing.T) {
	_, components := renderSetupStep(settings.GuildSettings{}, setupStepAdmins, "")
	ids := setupComponentIDs(t, components)
	if !hasID(ids, setupAdminsSelectCustomID) {
		t.Fatalf("admins step missing its user picker, got %v", ids)
	}
	for _, id := range ids {
		if strings.HasSuffix(id, ":create") {
			t.Errorf("admins step should offer no create button, got %q", id)
		}
	}
}

// TestSetupEveryStepIsNavigable checks the wizard can never strand someone:
// every step renders nav, only the ends are disabled, and the step counter
// covers every step exactly once.
func TestSetupEveryStepIsNavigable(t *testing.T) {
	for step := range setupStepCount {
		_, components := renderSetupStep(settings.GuildSettings{}, step, "")
		row, ok := components[len(components)-1].(discordgo.ActionsRow)
		if !ok || len(row.Components) != 3 {
			t.Fatalf("step %d: expected a 3-button nav row last, got %#v", step, components[len(components)-1])
		}
		back := row.Components[0].(discordgo.Button)
		next := row.Components[2].(discordgo.Button)
		if wantDisabled := step == 0; back.Disabled != wantDisabled {
			t.Errorf("step %d: Back disabled=%v, want %v", step, back.Disabled, wantDisabled)
		}
		if wantDisabled := step == setupStepCount-1; next.Disabled != wantDisabled {
			t.Errorf("step %d: Next disabled=%v, want %v", step, next.Disabled, wantDisabled)
		}
		if !back.Disabled {
			if page, err := core.ParsePaginationPage(back.CustomID, setupStepPrefix); err != nil || page != step-1 {
				t.Errorf("step %d: Back targets %q (page %d, err %v), want step %d", step, back.CustomID, page, err, step-1)
			}
		}
		if !next.Disabled {
			if page, err := core.ParsePaginationPage(next.CustomID, setupStepPrefix); err != nil || page != step+1 {
				t.Errorf("step %d: Next targets %q (page %d, err %v), want step %d", step, next.CustomID, page, err, step+1)
			}
		}
	}
}

// TestSetupStepClamping keeps a hand-edited or stale CustomID from rendering
// an empty step, the same fail-safe core.Paginate applies to list pages.
func TestSetupStepClamping(t *testing.T) {
	for _, step := range []int{-5, -1, setupStepCount, setupStepCount + 99} {
		embed, components := renderSetupStep(settings.GuildSettings{}, step, "")
		if embed.Title == "" {
			t.Errorf("step %d clamped to a step with no title", step)
		}
		if len(components) == 0 {
			t.Errorf("step %d clamped to a step with no controls", step)
		}
	}
}

// TestSetupChecklistReflectsLiveSettings covers the wizard's resumability
// claim: an already-configured guild sees every item ticked off with its
// current value, so re-running /config setup reads as a review rather than a
// prompt to redo everything.
func TestSetupChecklistReflectsLiveSettings(t *testing.T) {
	empty := setupChecklist(settings.GuildSettings{})
	if strings.Contains(empty, "✅") {
		t.Errorf("unconfigured guild should have nothing ticked off:\n%s", empty)
	}

	configured := setupChecklist(settings.GuildSettings{
		AuditLogChannelID: "111",
		StatusChannelID:   "222",
		ModRoleIDs:        []string{"333"},
		AdminUserIDs:      []string{"444"},
	})
	if strings.Contains(configured, "◻️") {
		t.Errorf("fully configured guild should have everything ticked off:\n%s", configured)
	}
	for _, want := range []string{"<#111>", "<#222>", "<@&333>", "<@444>"} {
		if !strings.Contains(configured, want) {
			t.Errorf("checklist missing %s:\n%s", want, configured)
		}
	}
}

// TestMentionListTextTruncates keeps a guild with a long list from blowing
// past Discord's embed field length limit.
func TestMentionListTextTruncates(t *testing.T) {
	ids := []string{"1", "2", "3", "4", "5", "6", "7"}
	got := mentionListText(ids, "<@&%s>", "none")
	if !strings.HasSuffix(got, "+2 more") {
		t.Errorf("mentionListText(%v) = %q, want a '+2 more' suffix", ids, got)
	}
	if strings.Contains(got, "<@&6>") {
		t.Errorf("mentionListText(%v) = %q, should have stopped listing before the 6th", ids, got)
	}
	if got := mentionListText(nil, "<@&%s>", "none"); got != "none" {
		t.Errorf("empty mentionListText = %q, want the empty placeholder", got)
	}
}

// TestSetupNoticeIsShownAboveStepText verifies the one-off "here's what just
// happened" line survives into the rendered embed: it's the only feedback an
// admin gets that a pick actually took effect, since the wizard advances
// rather than posting a separate confirmation.
func TestSetupNoticeIsShownAboveStepText(t *testing.T) {
	const notice = "✅ Audit log is now <#123>."
	embed, _ := renderSetupStep(settings.GuildSettings{AuditLogChannelID: "123"}, setupStepStatus, notice)
	if !strings.HasPrefix(embed.Description, notice) {
		t.Errorf("notice not rendered at the top of the step:\n%s", embed.Description)
	}
}

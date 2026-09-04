package aimod

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
)

// /aimod status, now that it is several screens.
//
// The assertions here are about the page arithmetic and about which page a
// given fact lands on, not about wording. What the report says is checked by
// the existing handler tests; what this file exists to stop is a page count
// that disagrees with the buttons, an out-of-range page panicking, and the
// opt-out list quietly not being shown.

// statusPagesFor builds the report for a guild whose config is cfg.
func statusPagesFor(t *testing.T, cfg Config) ([]statusPage, int) {
	t.Helper()
	store := newFakeStore()
	store.setConfig(cfg)
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})

	pages, color, err := p.statusPages(context.Background(), cfg.GuildID)
	if err != nil {
		t.Fatalf("statusPages: %v", err)
	}
	return pages, color
}

func pageNames(pages []statusPage) []string {
	out := make([]string, 0, len(pages))
	for _, pg := range pages {
		out = append(out, pg.name)
	}
	return out
}

// The four questions the split is by: is it working, what does it act on,
// what is it costing and through whom, and who is not covered.
func TestStatusHasAPageForEachQuestion(t *testing.T) {
	pages, _ := statusPagesFor(t, enforcingConfig())

	want := []string{"overview", "what it acts on", "provider and cost", "opted out"}
	if got := strings.Join(pageNames(pages), " | "); got != strings.Join(want, " | ") {
		t.Errorf("pages are %q, want %q", got, strings.Join(want, " | "))
	}
}

// The opt-out page exists even with the feature off and nobody on the list.
// It is the only surface that answers "is anyone exempt from the filter
// here", and answering that by not being there is not an answer a moderator
// can act on.
func TestOptOutPageIsShownEvenWhenNobodyHasOptedOut(t *testing.T) {
	pages, _ := statusPagesFor(t, optOutConfig(false))

	last := pages[len(pages)-1]
	if last.name != "opted out" {
		t.Fatalf("last page is %q, want the opt-out page", last.name)
	}
	if !strings.Contains(last.body, "Member opt-out is off") {
		t.Errorf("body does not say the feature is off: %q", last.body)
	}
	if got := last.fields[0].Value; got != "nobody" {
		t.Errorf("listed %q, want nobody", got)
	}
}

// A list kept alive by a switch that has since been turned off has to be
// labelled as such. Under a bare "Opted out (2)" heading it reads as two
// people who are exempt right now, and they are not.
func TestKeptOptOutListSaysItIsNotInEffect(t *testing.T) {
	pages, _ := statusPagesFor(t, optOutConfig(false, "u1", "u2"))

	last := pages[len(pages)-1]
	if !strings.Contains(last.fields[0].Name, "not in effect") {
		t.Errorf("heading is %q, want it to say the list is not in effect", last.fields[0].Name)
	}
	// And the same list under a live switch is not hedged.
	on, _ := statusPagesFor(t, optOutConfig(true, "u1", "u2"))
	if strings.Contains(on[len(on)-1].fields[0].Name, "not in effect") {
		t.Errorf("heading is %q on a live switch, want no hedge", on[len(on)-1].fields[0].Name)
	}
}

// Everybody who opted out is reachable. An embed field caps at 1024 bytes and
// this list is unbounded, so it pages rather than being truncated into a
// silent lie about who is exempt.
func TestOptOutListPagesRatherThanTruncating(t *testing.T) {
	ids := make([]string, 0, core.PageSize*2+3)
	for n := range cap(ids) {
		ids = append(ids, fmt.Sprintf("user%02d", n))
	}
	pages, _ := statusPagesFor(t, optOutConfig(true, ids...))

	var listed []string
	for _, pg := range pages {
		if !strings.HasPrefix(pg.name, "opted out") {
			continue
		}
		listed = append(listed, pg.fields[0].Value)
	}
	if len(listed) != 3 {
		t.Fatalf("%d opt-out pages for %d members, want 3", len(listed), len(ids))
	}
	joined := strings.Join(listed, " ")
	for _, id := range ids {
		if !strings.Contains(joined, id) {
			t.Errorf("%s is opted out but appears on no page", id)
		}
	}
	// Numbered, or a moderator on page two of three has no way to tell there
	// is a page three.
	if !strings.Contains(pages[len(pages)-1].name, "3/3") {
		t.Errorf("last page is named %q, want it numbered", pages[len(pages)-1].name)
	}
}

// The severity is the whole report's, not the page's: paging away from the
// problem must not turn the embed green, because nothing was fixed by
// scrolling.
func TestEveryPageWearsTheWholeReportsSeverity(t *testing.T) {
	cfg := enforcingConfig()
	cfg.Mode = ModeOff
	pages, color := statusPagesFor(t, cfg)

	if color != core.ColorWarning {
		t.Fatalf("colour is %d, want ColorWarning for a guild scanning nothing", color)
	}
	for n := range pages {
		embed, _ := renderStatusPage(pages, color, n)
		if embed.Color != core.ColorWarning {
			t.Errorf("page %d (%s) is colour %d, want the report's own", n, pages[n].name, embed.Color)
		}
	}
}

// "Nothing is being scanned" must not be painted over by a milder problem
// found further down the report. It is the line somebody reading this screen
// most needs to be told first.
func TestTheWorstProblemWins(t *testing.T) {
	store := newFakeStore()
	cfg := enforcingConfig()
	cfg.DailyBudgetUSD = 0 // also a warning, discovered later
	store.setConfig(cfg)
	p := testPlugin(t, store, &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	p.scanning = false

	_, color, err := p.statusPages(context.Background(), "g1")
	if err != nil {
		t.Fatalf("statusPages: %v", err)
	}
	if color != core.ColorError {
		t.Errorf("colour is %d, want ColorError: a missing intent outranks a spent budget", color)
	}
}

// A page number out of range can only arrive from a hand-built CustomID, and
// it must clamp rather than panic. core.Paginate makes the same promise for
// the same reason.
func TestRenderStatusPageClampsOutOfRange(t *testing.T) {
	pages, color := statusPagesFor(t, enforcingConfig())

	for _, page := range []int{-5, -1, len(pages), 999} {
		embed, _ := renderStatusPage(pages, color, page)
		if embed == nil || embed.Title == "" {
			t.Fatalf("page %d rendered nothing", page)
		}
	}
}

// The buttons have to agree with the report they are attached to, or Next
// stops one page early and the last page is unreachable.
func TestPaginationButtonsCoverEveryPage(t *testing.T) {
	ids := make([]string, 0, core.PageSize+1)
	for n := range cap(ids) {
		ids = append(ids, fmt.Sprintf("user%02d", n))
	}
	pages, color := statusPagesFor(t, optOutConfig(true, ids...))

	_, components := renderStatusPage(pages, color, len(pages)-1)
	next := buttonByLabel(t, components, "Next ▶")
	if !next.Disabled {
		t.Error("Next is live on the last page")
	}
	_, components = renderStatusPage(pages, color, 0)
	if prev := buttonByLabel(t, components, "◀ Prev"); !prev.Disabled {
		t.Error("Prev is live on the first page")
	}
	next = buttonByLabel(t, components, "Next ▶")
	got, err := core.ParsePaginationPage(next.CustomID, statusPagePrefix)
	if err != nil || got != 1 {
		t.Errorf("Next from page 0 targets %d (%v), want 1", got, err)
	}
}

func buttonByLabel(t *testing.T, components []discordgo.MessageComponent, label string) discordgo.Button {
	t.Helper()
	for _, row := range components {
		ar, ok := row.(discordgo.ActionsRow)
		if !ok {
			continue
		}
		for _, c := range ar.Components {
			if b, ok := c.(discordgo.Button); ok && b.Label == label {
				return b
			}
		}
	}
	t.Fatalf("no %q button in %#v", label, components)
	return discordgo.Button{}
}

// The handler end to end, over the stub transport: it has to defer, build and
// answer without panicking on a guild that has configured nothing at all.
func TestStatusHandlerAnswersAnEmptyGuild(t *testing.T) {
	p := testPlugin(t, newFakeStore(), &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	p.handleStatus(context.Background(), testSession(t), interaction("g1", "", "status"))
}

// And the click path, including a CustomID that will not parse: page 0 rather
// than an error, since the worst outcome is somebody seeing the overview.
func TestStatusPageClickSurvivesAJunkCustomID(t *testing.T) {
	p := testPlugin(t, newFakeStore(), &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	i := interaction("g1", "", "status")
	i.Type = discordgo.InteractionMessageComponent

	p.handleStatusPage(context.Background(), testSession(t), i, statusPagePrefix+"banana")
	p.handleStatusPage(context.Background(), testSession(t), i, core.PaginationCustomID(statusPagePrefix, 2))
}

package dragoman

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/language"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/core/paneltest"
)

const danish = "det er ikke normalt at toget altid er for sent på den her linje"

// patois is a guide a model might write, marked as one of several by n.
func patois(n int) guide {
	g := guide{
		Avoid: []string{"mon after every sentence"},
		Marks: []string{"wah", "nuh", "dem", "gwaan", "deh", "mi", "fi"},
		Words: []pair{{"I", "mi"}, {"not", "nuh"}},
	}
	for i := range 6 {
		g.Rules = append(g.Rules, rule{fmt.Sprintf("rule %d of guide %d", i, n), "He is going.", "him a go"})
		g.Examples = append(g.Examples, pair{fmt.Sprintf("They are late %d.", i), fmt.Sprintf("dem late %d", i)})
	}
	return g
}

func written(g guide, refuse string) string {
	raw, _ := json.Marshal(struct {
		Refuse string `json:"refuse"`
		guide
	}{refuse, g})
	return string(raw)
}

func batch(s string) string {
	texts := make([]string, benchSize)
	for i := range texts {
		texts[i] = s
	}
	raw, _ := json.Marshal(map[string]any{"texts": texts})
	return string(raw)
}

func verdicts(better string, a, b bool) string {
	var vs []map[string]any
	for range benchSize {
		vs = append(vs, map[string]any{"better": better, "a_same_meaning": a, "b_same_meaning": b})
	}
	raw, _ := json.Marshal(map[string]any{"verdicts": vs})
	return string(raw)
}

// answering is an engine that writes guide 1, and has a round of which won
// is the outcome: the rival, guide 2, is A in the second comparison.
func (r *rig) answering(won bool) {
	first, second := "A", "B"
	if won {
		first, second = "B", "A"
	}
	r.engine.answers = map[string][]string{
		"guide":    {written(patois(1), ""), written(patois(2), "")},
		"dialects": {batch("dem deh yah"), batch("mi nuh know"), batch("wah gwaan")},
		"verdicts": {verdicts(first, true, true), verdicts(second, true, true)},
	}
}

func (r *rig) pick(t *testing.T, lang, name string) string {
	t.Helper()
	id := panelDialect + "pick:" + lang
	ix, w := paneltest.IxPick(t, op, id, nil, name)
	r.p.handlePanelDialect(t.Context(), ix, id)
	said := w.All()
	r.p.wg.Wait()
	return said
}

// From a pick to the dialect being spoken, with nothing else asked of anyone.
func TestPickingADialectIsAllItTakes(t *testing.T) {
	r := newRig(t)
	r.answering(true)
	if said := r.pick(t, "da", "Jamaican English"); !strings.Contains(said, "Danish becomes Jamaican English") {
		t.Errorf("said %s", said)
	}
	d := r.store.langs["da"]
	if d.Guide == nil || d.Guide.Rules[0].Rule != "rule 0 of guide 2" || d.Previous == nil || d.Version != 3 {
		t.Fatalf("after its first look at itself: %+v", d)
	}
	for _, want := range []string{`dragoman.dialect_set||language="Danish" english="Jamaican English"`,
		`dragoman.dialect_improved||language="Danish" english="Jamaican English" revision=3 better=16 worse=0`} {
		if !strings.Contains(r.audit.all(), want) {
			t.Errorf("no %s in\n%s", want, r.audit.all())
		}
	}
	if u := r.store.used[rungTend]; u.Calls != 7 {
		t.Errorf("%d calls on the guide", u.Calls)
	}

	r.engine.out = strings.Replace(good, `"nl"`, `"da"`, 1)
	r.engine.styled = `{"text":"di train dem late every time"}`
	before := r.engine.asked()
	r.say(msg(danish))
	if got := r.engine.reposts(); len(got) != 1 || got[0].Text != "di train dem late every time" {
		t.Fatalf("posted %+v", got)
	}
	if q := r.engine.prompts[before+1]; !strings.Contains(q.System, "into Jamaican English") || !strings.Contains(q.System, "rule 0 of guide 2") || !strings.Contains(q.System, fairPlay) {
		t.Errorf("prompt %s", q.System)
	}
	// What a member wrote is in nothing that made or judged a guide.
	for _, q := range r.engine.prompts {
		if q.SchemaName != "translation" && q.SchemaName != "dialect" && strings.Contains(q.User+q.System, "toget") {
			t.Errorf("a member's words went into %s", q.SchemaName)
		}
	}
}

// The one that comes written needs no model, under any of its names.
func TestAAVEComesWritten(t *testing.T) {
	for _, name := range []string{"AAVE", "african american vernacular english"} {
		r := newRig(t)
		r.pick(t, "nl", name)
		if d := r.store.langs["nl"]; d.Guide == nil || r.engine.asked() != 0 {
			t.Errorf("%s: %+v after %d calls", name, d, r.engine.asked())
		}
	}
	// And a guild the migration gave the row to gets it at its first message,
	// which itself goes out as plain English.
	r := newRig(t)
	_ = r.store.SetDialect(t.Context(), guild, "nl", "African American Vernacular English")
	r.engine.styled = `{"text":"that train be late"}`
	r.say(msg(dutch))
	r.say(msg(dutch))
	if got := r.engine.reposts(); len(got) != 2 || got[0].Text != "the train's always late" || got[1].Text != "that train be late" {
		t.Errorf("posted %+v", got)
	}
	g := builtin["african american vernacular english"]
	n := len(g.Marks)
	if err := g.validate(); err != nil || len(g.Marks) != n {
		t.Errorf("the built-in guide doesn't hold up: %v, %d marks of %d", err, len(g.Marks), n)
	}
}

func TestAGuideThatCannotBeWritten(t *testing.T) {
	thin := patois(1)
	thin.Rules = thin.Rules[:2]
	// One that isn't usable is put right in a second call, told why.
	r := newRig(t)
	r.answering(false)
	r.engine.answers["guide"] = append([]string{written(thin, "")}, r.engine.answers["guide"]...)
	r.pick(t, "da", "Jamaican English")
	if d := r.store.langs["da"]; d.Guide == nil || !strings.Contains(r.engine.prompts[1].User, "2 usable rules") {
		t.Errorf("%+v, asked %q", d, r.engine.prompts[1].User)
	}

	for what, tc := range map[string]struct {
		change func(*rig)
		says   string
	}{
		"never usable":  {func(r *rig) { r.engine.answers["guide"] = []string{"sure!", written(thin, "")} }, "2 usable rules"},
		"not a dialect": {func(r *rig) { r.engine.answers["guide"] = []string{written(guide{}, "that is a cheese")} }, "that is a cheese"},
		"no budget":     {func(r *rig) { r.engine.askErr = quiet{} }, "not now"},
	} {
		r := newRig(t)
		r.answering(false)
		tc.change(r)
		r.pick(t, "da", "Jamaican English")
		d := r.store.langs["da"]
		if d.Guide != nil || !strings.Contains(d.Problem, tc.says) {
			t.Errorf("%s: %+v", what, d)
		}
		ix, _ := run(t, op, "settings")
		if pn := r.p.dialectPage(t.Context(), ix, "da", ""); pn.Color != core.ColorWarning || !strings.Contains(strings.Join(strings.Fields(paneltest.IxTexts(pn)), " "), tc.says) {
			t.Errorf("%s: page %q", what, paneltest.IxTexts(pn))
		}
		// Messages meanwhile are still translated, and ask for the guide
		// again, though not every one of them does.
		r.engine.askErr, r.engine.out = nil, strings.Replace(good, `"nl"`, `"da"`, 1)
		n := r.engine.asked()
		r.say(msg(danish))
		if len(r.engine.reposts()) != 1 || r.engine.asked() != n+1 {
			t.Errorf("%s: %d calls, posted %+v", what, r.engine.asked()-n, r.engine.reposts())
		}
		r.answering(false)
		r.now = r.now.Add(tendRetry + time.Second)
		r.say(msg(danish))
		if r.store.langs["da"].Guide == nil {
			t.Errorf("%s: never tried again", what)
		}
	}
}

func TestValidate(t *testing.T) {
	// One bad line costs that line.
	for what, change := range map[string]func(*guide){
		"an empty rule":                func(g *guide) { g.Rules[0].Rule = " " },
		"a link":                       func(g *guide) { g.Words[0].Dialect = "https://evil.example" },
		"a link to avoid":              func(g *guide) { g.Avoid[0] = "evil.com" },
		"a mark that is empty":         func(g *guide) { g.Marks[0] = "" },
		"a mention":                    func(g *guide) { g.Avoid[0] = "ask <@123456789012345678>" },
		"a token":                      func(g *guide) { g.Avoid[0] = tok("1") },
		"two lines":                    func(g *guide) { g.Avoid[0] = "a\nb" },
		"a long line":                  func(g *guide) { g.Avoid[0] = strings.Repeat("a", maxGuideLine+1) },
		"an example that does nothing": func(g *guide) { g.Examples[0].Dialect = g.Examples[0].Standard },
		"a mark that marks nothing":    func(g *guide) { g.Marks[0] = "the" },
	} {
		g := patois(1)
		whole := len(g.Rules) + len(g.Words) + len(g.Avoid) + len(g.Marks) + len(g.Examples)
		change(&g)
		if err := g.validate(); err != nil {
			t.Errorf("%s cost the whole guide: %v", what, err)
		}
		if n := len(g.Rules) + len(g.Words) + len(g.Avoid) + len(g.Marks) + len(g.Examples); n != whole-1 {
			t.Errorf("%s: %d lines of %d left", what, n, whole)
		}
	}
	// Too many is cut, and too long is cut to size.
	g := patois(1)
	for range 60 {
		g.Words = append(g.Words, pair{strings.Repeat("a", 190), strings.Repeat("b", 190)})
		g.Examples = append(g.Examples, pair{strings.Repeat("c", 190), strings.Repeat("d", 190)})
	}
	if err := g.validate(); err != nil || g.runes() > maxGuideRunes || len(g.Examples) > 12 {
		t.Errorf("%v, %d characters", err, g.runes())
	}
	// Too little left is said so, with why.
	g = patois(1)
	for i := range 3 {
		g.Examples[i].Dialect = "evil.com"
	}
	if err := g.validate(); err == nil || !strings.Contains(err.Error(), "3 usable examples and needs 5 (left out: 3 had a link, a mention or a token)") {
		t.Errorf("%v", err)
	}
	// Words are the dialect's own business: what is posted is what's checked.
	g = patois(1)
	g.Examples[0].Dialect, g.Avoid[0] = "im gwaan inna road", "any slur a filter knows"
	if err := g.validate(); err != nil || len(g.Examples) != 6 {
		t.Errorf("%v, %d examples", err, len(g.Examples))
	}
	g = patois(1)
	if !g.marked("Wah gwaan, boss?") || !g.marked("he workin again") || g.marked("what is happening at the dementia clinic") {
		t.Error("marked")
	}
}

// Only a rival that is plainly better, and says the same things, takes over.
func TestARoundKeepsTheBetterGuide(t *testing.T) {
	half := func() string { // the rival is better in half of them and worse in half
		var v struct {
			Verdicts []map[string]any `json:"verdicts"`
		}
		_ = json.Unmarshal([]byte(verdicts("A", true, true)), &v)
		for i := range benchSize / 2 {
			v.Verdicts[i]["better"] = "B"
		}
		raw, _ := json.Marshal(v)
		return string(raw)
	}
	for what, tc := range map[string]struct {
		verdicts []string
		forced   bool
		won      bool
	}{
		"better":              {[]string{verdicts("B", true, true), verdicts("A", true, true)}, false, true},
		"worse":               {[]string{verdicts("A", true, true), verdicts("B", true, true)}, false, false},
		"the same":            {[]string{verdicts("same", true, true), verdicts("same", true, true)}, false, false},
		"a judge of position": {[]string{verdicts("A", true, true), verdicts("A", true, true)}, false, false},
		"meaning lost":        {[]string{verdicts("B", true, false), verdicts("A", false, true)}, false, false},
		"no worse, and asked": {[]string{half(), half()}, true, true},
		"worse, and asked":    {[]string{verdicts("A", true, true), verdicts("B", true, true)}, true, false},
		"nonsense":            {[]string{"{}"}, false, false},
	} {
		r := newRig(t)
		r.spoken("da", "Jamaican English")
		r.answering(false)
		r.engine.answers["guide"] = []string{written(patois(2), "")}
		r.engine.answers["verdicts"] = tc.verdicts
		r.p.tend(guild, "da", tc.forced)
		r.p.wg.Wait()
		d := r.store.langs["da"]
		if (d.Version == 2) != tc.won || (d.Losses == 1) == tc.won {
			t.Errorf("%s: version %d, losses %d", what, d.Version, d.Losses)
		}
		// Judged on sentences neither guide was shown.
		for i, q := range r.engine.prompts {
			if q.SchemaName == "verdicts" && strings.Contains(r.engine.prompts[1].User, strings.Split(strings.Split(q.User, "Standard: ")[1], "\n")[0]) {
				t.Errorf("%s: call %d judges a sentence the reviser read", what, i)
			}
		}
	}
}

// Rounds come when they are due, further apart each time nothing changes.
func TestRoundsThinOut(t *testing.T) {
	r := newRig(t)
	r.spoken("nl", "Jamaican English")
	r.store.due["nl"] = 2
	r.engine.styled = `{"text":"di train dem late"}`
	var rounds []int
	for range 4 {
		r.answering(false)
		r.engine.answers["guide"] = []string{written(patois(2), "")}
		for len(r.engine.answers["verdicts"]) > 0 {
			r.now = r.now.Add(memberWindow)
			r.say(msg(dutch))
		}
		rounds = append(rounds, r.store.langs["nl"].Uses)
	}
	if fmt.Sprint(rounds) != "[2 52 152 352]" {
		t.Errorf("rounds after %v messages", rounds)
	}

	// And a day holds only so many, whoever asks.
	r = newRig(t)
	r.spoken("nl", "Jamaican English")
	for range tendMax + 3 {
		r.answering(false)
		r.p.tend(guild, "nl", true)
		r.p.wg.Wait()
	}
	if n := r.p.rounds[guild].n; n != tendMax {
		t.Errorf("%d rounds in a day", n)
	}
	ix, _ := run(t, op, "settings")
	if note := r.p.improveNow(t.Context(), ix, "nl"); !strings.Contains(note, "rounds today") {
		t.Errorf("said %q", note)
	}
	r.now = r.now.Add(24 * time.Hour)
	if note := r.p.improveNow(t.Context(), ix, "nl"); !strings.Contains(note, "Working on it") {
		t.Errorf("said %q", note)
	}
	r.p.wg.Wait()
}

// What is picked while a guide is being written is what gets written next,
// and the one under way lands nowhere.
func TestChangingYourMindMeanwhile(t *testing.T) {
	r := newRig(t)
	r.answering(false)
	r.engine.block = make(chan struct{})
	ix, _ := run(t, op, "settings")
	r.p.setDialect(t.Context(), ix, "da", "Jamaican English")
	for r.engine.asked() == 0 {
		time.Sleep(time.Millisecond)
	}
	if note := r.p.setDialect(t.Context(), ix, "da", "Scottish English"); !strings.Contains(note, "Scottish") {
		t.Error(note)
	}
	close(r.engine.block)
	r.p.wg.Wait()
	if d := r.store.langs["da"]; d.Name != "Scottish English" || d.Guide == nil || d.Guide.Rules[0].Rule != "rule 0 of guide 2" {
		t.Errorf("%+v", d)
	}
	// Nothing is started once shutdown has begun.
	if err := r.p.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	n := r.engine.asked()
	r.p.tend(guild, "da", true)
	if r.p.wg.Wait(); r.engine.asked() != n {
		t.Error("tended after shutdown")
	}
}

func TestAdminsSteerADialect(t *testing.T) {
	r := newRig(t)
	r.spoken("da", "Jamaican English")
	form := func(verb string, fields map[string]string) string {
		t.Helper()
		id := formDialect + verb
		ix, w := paneltest.IxSubmit(t, op, id, fields)
		r.p.handleDialectForm(t.Context(), ix, id)
		said := w.All()
		r.p.wg.Wait()
		return said
	}
	press := func(verb string) string {
		t.Helper()
		id := panelDialect + verb
		ix, w := paneltest.IxClick(t, op, id, nil)
		r.p.handlePanelDialect(t.Context(), ix, id)
		said := w.All()
		r.p.wg.Wait()
		return said
	}

	// Trying a sentence shows it and nothing more.
	r.engine.styled = `{"text":"mi nuh tink him a come"}`
	if said := form("try:da", map[string]string{"text": "I don't think he's coming"}); !strings.Contains(said, "mi nuh tink him a come") {
		t.Errorf("tried: %s", said)
	}
	if d := r.store.langs["da"]; d.Uses != 0 || len(r.engine.reposts()) != 0 {
		t.Error("a try counted or was posted")
	}
	// What is said about it reaches the reviser and the judge.
	r.answering(false)
	r.engine.answers["guide"] = []string{written(patois(2), "")}
	r.engine.answers["verdicts"] = []string{verdicts("same", true, true), verdicts("same", true, true)}
	start := r.engine.asked()
	if said := form("note:da", map[string]string{"note": "a bit  lighter"}); !strings.Contains(said, "working it into the guide") {
		t.Errorf("noted: %s", said)
	}
	heard := map[string]bool{}
	for _, q := range r.engine.prompts[start:] {
		heard[q.SchemaName] = strings.Contains(q.User, "- a bit lighter")
	}
	if d := r.store.langs["da"]; !heard["guide"] || !heard["verdicts"] || d.Version != 2 || len(d.Notes) != 1 {
		t.Errorf("heard by %v, %+v", heard, d)
	}
	for i := range maxNotes + 2 {
		r.now = r.now.Add(24 * time.Hour)
		form("note:da", map[string]string{"note": fmt.Sprint("note ", i)})
	}
	if n := r.store.langs["da"].Notes; len(n) != maxNotes || n[maxNotes-1] != fmt.Sprint("note ", maxNotes+1) {
		t.Errorf("notes %v", n)
	}

	if said := press("undo:da"); !strings.Contains(said, "guide before") || r.store.langs["da"].Previous != nil {
		t.Errorf("undone: %s", said)
	}
	if said := press("undo:da"); !strings.Contains(said, "nothing to go back") {
		t.Errorf("undone twice: %s", said)
	}
	for verb, form := range map[string]string{"try:da": formDialect + "try:da", "note:da": formDialect + "note:da"} {
		if said := press(verb); !strings.Contains(said, form) {
			t.Errorf("%s opened %s", verb, said)
		}
	}
	if said := press("improve:da"); !strings.Contains(said, "Working on it") {
		t.Errorf("improve: %s", said)
	}
	if said := press("remove:da"); !strings.Contains(said, "Standard American English again") || len(r.store.langs) != 0 {
		t.Errorf("removed: %s", said)
	}
	for _, verb := range []string{"remove:da", "undo:da", "improve:da", "gone:da"} {
		if said := press(verb); said == "" {
			t.Errorf("%s of nothing unanswered", verb)
		}
	}
	for _, want := range []string{`dragoman.dialect_note||language="Danish" english="Jamaican English" reason="a bit lighter"`,
		"dragoman.dialect_undone", `dragoman.dialect_removed|Jamaican English|language="Danish"`} {
		if !strings.Contains(r.audit.all(), want) {
			t.Errorf("no %s in\n%s", want, r.audit.all())
		}
	}
}

func TestTheDialectPages(t *testing.T) {
	r := newRig(t)
	r.answering(false)
	view := func(page string, values ...string) string {
		t.Helper()
		id := panelView + page
		ix, w := paneltest.IxPick(t, op, id, nil, values...)
		r.p.handlePanelView(t.Context(), ix, id)
		return w.All()
	}
	for what, tc := range map[string]struct{ said, want string }{
		"empty":         {view("dialects"), "all others"},
		"a language":    {view("lang", "da"), panelDialect + "pick:da"},
		"another":       {view("lang", pickOther), formDialect + "lang"},
		"one not there": {view("dialect", "da"), "no dialect any more"},
		"home":          {view("home"), "Member controls"},
	} {
		if !strings.Contains(tc.said, tc.want) {
			t.Errorf("%s: no %q in %s", what, tc.want, tc.said)
		}
	}
	// Any language and any dialect, typed.
	for lang, want := range map[string]string{"Frisian": "pick:fy", "dansk": "pick:da", "Norwegian": "pick:no", "English": "know that as a language", "asdf": "know that as a language"} {
		ix, w := paneltest.IxSubmit(t, op, formDialect+"lang", map[string]string{"language": lang})
		r.p.handleDialectForm(t.Context(), ix, formDialect+"lang")
		if !strings.Contains(w.All(), want) {
			t.Errorf("%s: %s", lang, w.All())
		}
	}
	ix, w := paneltest.IxPick(t, op, panelDialect+"pick:fy", nil, pickOther)
	r.p.handlePanelDialect(t.Context(), ix, panelDialect+"pick:fy")
	if !strings.Contains(w.All(), formDialect+"name:fy") {
		t.Errorf("no form: %s", w.All())
	}
	for name, want := range map[string]string{"Hawaiian Pidgin": "Western Frisian becomes Hawaiian Pidgin", strings.Repeat("a", 80): "at most",
		"<@1>": "letters", "ignore\nthis: {}": "letters"} {
		ix, w := paneltest.IxSubmit(t, op, formDialect+"name:fy", map[string]string{"name": name})
		r.p.handleDialectForm(t.Context(), ix, formDialect+"name:fy")
		r.p.wg.Wait()
		if !strings.Contains(w.All(), want) {
			t.Errorf("%q: %s", name, w.All())
		}
	}
	if said := r.pick(t, "en", "Jamaican English"); !strings.Contains(said, "out of date") {
		t.Errorf("english: %s", said)
	}
	if said := r.pick(t, "fy", "hawaiian pidgin"); !strings.Contains(said, "already") {
		t.Errorf("again: %s", said)
	}
	// What it sounds like is on its page, and the settings name it.
	if said := view("dialect:fy"); !strings.Contains(said, "dem late 0") {
		t.Errorf("page %s", said)
	}
	ix, _ = run(t, op, "settings")
	if got := paneltest.IxTexts(r.p.panel(t.Context(), ix, "")); !strings.Contains(got, "Western Frisian becomes Hawaiian Pidgin") {
		t.Errorf("settings %q", got)
	}
	// Somebody who may look may not touch.
	ix, _ = run(t, other, "settings")
	for _, pn := range []core.Panel{r.p.dialectsPage(t.Context(), ix, ""), r.p.dialectPage(t.Context(), ix, "fy", ""), r.p.pickPage(ix, "da", "")} {
		for id, b := range paneltest.IxButtons(pn) {
			if strings.HasPrefix(id, panelDialect) && !b.Disabled {
				t.Errorf("%s is pressable", id)
			}
		}
	}
	// Every page fits Discord, however full.
	for _, key := range offeredLanguages {
		g := patois(1)
		for i := range g.Examples {
			g.Examples[i] = pair{strings.Repeat("a", maxGuideLine), strings.Repeat("b", maxGuideLine)}
		}
		r.store.langs[key] = Dialect{GuildID: guild, Language: key, Name: strings.Repeat("n", maxDialectLen), Guide: &g, Version: 1, Uses: 9,
			Notes: []string{strings.Repeat("x", maxNoteRunes), strings.Repeat("y", maxNoteRunes), strings.Repeat("z", maxNoteRunes)}}
	}
	r.p.forgetConfig(guild)
	ix, _ = run(t, op, "settings")
	for _, pn := range []core.Panel{r.p.dialectsPage(t.Context(), ix, ""), r.p.dialectPage(t.Context(), ix, "da", ""), r.p.panel(t.Context(), ix, "")} {
		if n := len([]rune(paneltest.IxTexts(pn))); n > 2*4000 {
			t.Errorf("%s is %d characters", pn.Title, n/2)
		}
	}
	r.p.forgetConfig(guild)
	r.store.err = errors.New("down")
	if pn := r.p.dialectsPage(t.Context(), ix, ""); pn.Color != core.ColorError {
		t.Error("unreadable dialects look fine")
	}
	if pn := r.p.dialectPage(t.Context(), ix, "da", ""); pn.Color != core.ColorError {
		t.Error("an unreadable dialect looks fine")
	}
	// And translation goes on in Standard English.
	r.store.err = nil
	if _, ok := r.p.dialectFor(t.Context(), guild, mustTag(t, "da")); !ok {
		t.Error("not found")
	}
}

func TestLanguagesByAnyName(t *testing.T) {
	for raw, key := range map[string]string{"Danish": "da", " DANSK ": "da", "da": "da", "nb": "no", "nn": "no", "no": "no", "norsk bokmål": "no",
		"Flemish": "nl", "Frisian": "fy", "Low German": "nds", "tl": "fil", "Swiss German": "gsw", "pt-BR": "pt"} {
		if tag, ok := parseLanguage(raw); !ok || languageKey(tag) != key {
			t.Errorf("%q is %v, %v", raw, languageKey(tag), ok)
		}
	}
	for _, raw := range []string{"", "English", "en", "xx", "klingonish", "und"} {
		if tag, ok := parseLanguage(raw); ok {
			t.Errorf("%q is %v", raw, tag)
		}
	}
	for _, key := range offeredLanguages {
		tag, ok := parseLanguage(key)
		if _, emoji := keyName(key); !ok || languageKey(tag) != key || len([]rune(emoji)) != 2 {
			t.Errorf("%s is offered as %v with %q", key, languageKey(tag), emoji)
		}
	}
	if len(offeredDialects) >= maxDialects || len(offeredLanguages) >= maxDialects {
		t.Error("no room for the other one")
	}
	for _, d := range offeredDialects {
		if got, err := cleanName(d); err != nil || got != d {
			t.Errorf("%s is offered and refused: %v", d, err)
		}
	}
}

func mustTag(t *testing.T, s string) language.Tag {
	t.Helper()
	tag, ok := parseSource(s)
	if !ok {
		t.Fatalf("%s is no language", s)
	}
	return tag
}

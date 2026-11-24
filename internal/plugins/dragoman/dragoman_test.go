package dragoman

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/core/paneltest"
	"github.com/6586x57890143/merlin/internal/discordguard"
)

const (
	guild  = paneltest.IxGuild
	op     = "900"
	author = "901"
	other  = "902"
	repost = "100000000000000777"
	dutch  = "de trein is echt altijd te laat, dat is niet normaal"
)

type memStore struct {
	mu    sync.Mutex
	cfg   Config
	rows  []Translation
	used  map[string]Usage
	langs map[string]Dialect
	due   map[string]int
	reads int
	err   error
}

func (s *memStore) Config(context.Context, string) (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	return s.cfg, s.err
}

func (s *memStore) SetEnabled(_ context.Context, _ string, on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.Enabled = on
	return s.err
}

func toggled(list []string, id string, on bool) []string {
	list = slices.DeleteFunc(slices.Clone(list), func(x string) bool { return x == id })
	if on {
		list = append(list, id)
	}
	return list
}

func (s *memStore) SetMemberControls(_ context.Context, _ string, on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.MemberControls = on
	return s.err
}

func (s *memStore) Dialects(context.Context, string) ([]Dialect, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Collect(maps.Values(s.langs)), s.err
}

func (s *memStore) Dialect(_ context.Context, _, lang string) (Dialect, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.langs[lang]
	if s.err == nil && !ok {
		return d, errStale
	}
	return d, s.err
}

func (s *memStore) SetDialect(_ context.Context, g, lang, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.langs == nil {
		s.langs, s.due = map[string]Dialect{}, map[string]int{}
	}
	s.langs[lang], s.due[lang] = Dialect{GuildID: g, Language: lang, Name: name, Version: s.langs[lang].Version + 1}, 0
	return s.err
}

func (s *memStore) RemoveDialect(_ context.Context, _, lang string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.langs, lang)
	return s.err
}

func (s *memStore) edit(d Dialect, f func(*Dialect)) (Dialect, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now, ok := s.langs[d.Language]
	if !ok || now.Version != d.Version || now.Name != d.Name {
		return d, errStale
	}
	f(&now)
	s.langs[d.Language] = now
	return now, s.err
}

func (s *memStore) SaveGuide(_ context.Context, d Dialect, g guide, after int) (Dialect, error) {
	return s.edit(d, func(d *Dialect) {
		d.Previous, d.Guide, d.Version, d.Uses, d.Missed, d.Losses, d.Problem = d.Guide, &g, d.Version+1, 0, 0, 0, ""
		s.due[d.Language] = after
	})
}

func (s *memStore) UndoGuide(_ context.Context, d Dialect, after int) error {
	_, err := s.edit(d, func(d *Dialect) {
		d.Guide, d.Previous, d.Version = d.Previous, nil, d.Version+1
		s.due[d.Language] = after
	})
	return err
}

func (s *memStore) DeferReview(_ context.Context, d Dialect, after int) error {
	_, err := s.edit(d, func(d *Dialect) { d.Losses++; s.due[d.Language] = d.Uses + after })
	return err
}

func (s *memStore) SetDialectProblem(_ context.Context, d Dialect, why string) error {
	_, err := s.edit(d, func(d *Dialect) { d.Problem = why })
	return err
}

func (s *memStore) SetDialectNotes(_ context.Context, d Dialect, notes []string) error {
	_, err := s.edit(d, func(d *Dialect) { d.Notes, d.Losses = notes, 0 })
	return err
}

func (s *memStore) CountDialect(_ context.Context, d Dialect, missed bool) (bool, error) {
	due := false
	_, err := s.edit(d, func(d *Dialect) {
		d.Uses++
		if missed {
			d.Missed++
		}
		due = d.Uses >= s.due[d.Language]
	})
	return due, err
}

func (s *memStore) SetModel(_ context.Context, _, model string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.Model = model
	return s.err
}

func (s *memStore) AddUsage(_ context.Context, _ string, _ time.Time, rung string, cost float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.used == nil {
		s.used = map[string]Usage{}
	}
	u := s.used[rung]
	s.used[rung] = Usage{u.Calls + 1, u.Cost + cost}
	return s.err
}

func (s *memStore) Usage(context.Context, string, time.Time) (map[string]Usage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.used, s.err
}

func (s *memStore) SetExempt(_ context.Context, _, id string, on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.ExemptChannelIDs = toggled(s.cfg.ExemptChannelIDs, id, on)
	return s.err
}

func (s *memStore) SetOptOut(_ context.Context, _, id string, on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.OptOutUserIDs = toggled(s.cfg.OptOutUserIDs, id, on)
	return s.err
}

func (s *memStore) Record(_ context.Context, t Translation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, t)
	return s.err
}

func (s *memStore) Find(_ context.Context, g, id string) (Translation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return Translation{}, s.err
	}
	for _, t := range s.rows {
		if t.GuildID == g && t.RepostID == id {
			return t, nil
		}
	}
	return Translation{}, errNotFound
}

func (s *memStore) CountSince(context.Context, string, time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rows), s.err
}

func (s *memStore) PruneBefore(context.Context, time.Time) (int64, error) { return 0, nil }

type fakeEngine struct {
	mu       sync.Mutex
	out      string
	styled   string // the answer to a dialect call
	styleErr error
	askErr   error
	postErr  error
	evidence int
	noKey    bool
	prompts  []core.Prompt
	posted   []core.Repost
	block    chan struct{}
	cost     float64
	outs     []string // answers by call, before out
	skip     bool
	taught   []bool
	orca     bool
	prices   map[string]core.ModelPrice
	answers  map[string][]string // answers by schema name, in order
	inAsk    int
	maxAsk   int
}

func (e *fakeEngine) Ask(_ context.Context, _ string, q core.Prompt) (core.Answer, error) {
	e.mu.Lock()
	e.prompts = append(e.prompts, q)
	e.inAsk++
	e.maxAsk = max(e.maxAsk, e.inAsk)
	block := e.block
	e.mu.Unlock()
	if block != nil {
		<-block
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.inAsk--
	a := core.Answer{Cost: e.cost}
	switch {
	case len(e.answers[q.SchemaName]) > 0:
		a.Text = e.answers[q.SchemaName][0]
		e.answers[q.SchemaName] = e.answers[q.SchemaName][1:]
		return a, e.askErr
	case q.SchemaName == "dialect":
		a.Text = e.styled
		return a, e.styleErr
	case len(e.outs) > 0:
		a.Text, e.outs = e.outs[0], e.outs[1:]
		return a, nil
	}
	a.Text = e.out
	return a, e.askErr
}

func (e *fakeEngine) Unlike(context.Context, string, string, string) bool { return e.skip }
func (e *fakeEngine) Teach(_ context.Context, _, _, _ string, worth bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.taught = append(e.taught, worth)
}
func (e *fakeEngine) HasOpenRouter(context.Context, string) bool { return !e.orca }
func (e *fakeEngine) Prices(context.Context, string) (map[string]core.ModelPrice, error) {
	if e.prices == nil {
		return nil, errors.New("no prices")
	}
	return e.prices, nil
}

func (e *fakeEngine) Context(string, string, string, string) ([]string, string) {
	return []string{"B: waar blijf je"}, "A"
}
func (e *fakeEngine) Policies(context.Context, string) string { return "- threats: no threats\n" }
func (e *fakeEngine) Repost(_ context.Context, r core.Repost) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.posted = append(e.posted, r)
	if e.postErr != nil {
		return "", e.postErr
	}
	return repost, nil
}
func (e *fakeEngine) EvidenceHours(context.Context, string) int     { return e.evidence }
func (e *fakeEngine) Budget(context.Context, string) (float64, float64, bool) {
	return 0.1, 0.5, !e.noKey
}

func (e *fakeEngine) reposts() []core.Repost {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.posted)
}

func (e *fakeEngine) asked() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.prompts)
}

type fakeGate struct{ off bool }

func (g *fakeGate) PluginEnabled(string, string) bool { return !g.off }

type fakeOps struct {
	deleted []string
	err     error
}

func (o *fakeOps) DeleteMessage(_, m snowflake.ID) error {
	if o.err == nil {
		o.deleted = append(o.deleted, m.String())
	}
	return o.err
}

type audits struct {
	mu      sync.Mutex
	entries []string
}

func (a *audits) Record(_ context.Context, _, actor, action, was, now string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, strings.Join([]string{actor, action, was, now}, "|"))
	return nil
}

func (a *audits) all() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return strings.Join(a.entries, "\n")
}

type rig struct {
	p      *Plugin
	store  *memStore
	engine *fakeEngine
	gate   *fakeGate
	ops    *fakeOps
	audit  *audits
	held   bool
	now    time.Time
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{
		store:  &memStore{cfg: Config{Enabled: true}},
		engine: &fakeEngine{out: good, evidence: 24},
		gate:   &fakeGate{}, ops: &fakeOps{}, audit: &audits{},
		now: time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC),
	}
	r.p = New(r.store, r.engine, r.gate, func(string) Ops { return r.ops }, func(string) bool { return r.held }, true)
	r.p.now = func() time.Time { return r.now }
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := core.NewCommandRouter(core.NewPermissions(nil, noAuth{}, op), nil, log)
	if err := r.p.Init(core.Deps{Audit: r.audit, Logger: log, Commands: router, Bus: core.NewEventBus(log)}); err != nil {
		t.Fatal(err)
	}
	return r
}

// noAuth is a guild that has configured no one.
type noAuth struct{}

func (noAuth) ModRoleIDs(string) []string   { return nil }
func (noAuth) AdminUserIDs(string) []string { return nil }
func (noAuth) ActionPolicy(string, string) core.ActionPolicy {
	return core.ActionPolicy{}
}

func msg(text string) discord.Message {
	g := snowflake.MustParse(guild)
	return discord.Message{
		ID: 555, GuildID: &g, ChannelID: 101, Content: text, Type: discord.MessageTypeDefault,
		Author: discord.User{ID: snowflake.MustParse(author)},
		Member: &discord.Member{RoleIDs: []snowflake.ID{7}},
	}
}

func (r *rig) say(m discord.Message) {
	r.p.HandleMessage(m)
	r.p.wg.Wait()
}

func TestAMessageIsTranslatedAndReposted(t *testing.T) {
	r := newRig(t)
	m := msg(dutch + " <@123456789012345678>")
	m.Type = discord.MessageTypeReply
	m.ReferencedMessage = &discord.Message{Content: "waar ben je"}
	r.engine.out = strings.Replace(good, "always late", "always late "+tok("1"), 1)
	r.say(m)

	got := r.engine.reposts()
	if len(got) != 1 {
		t.Fatalf("reposted %+v", got)
	}
	want := core.Repost{
		GuildID: guild, ChannelID: "101", MessageID: "555", AuthorID: author, RoleIDs: []string{"7"}, ReplyTo: "waar ben je",
		Original: m.Content, Text: "the train's always late <@123456789012345678>",
		Marker: "\n-# translated from \U0001F1F3\U0001F1F1 - edited by merlin",
	}
	if fmt.Sprintf("%+v", got[0]) != fmt.Sprintf("%+v", want) {
		t.Errorf("got  %+v\nwant %+v", got[0], want)
	}
	q := r.engine.prompts[0]
	if q.Reserve != reserve || q.Schema == nil || strings.Contains(q.User, "1234567") ||
		!strings.Contains(q.User, "waar blijf je") || !strings.Contains(q.User, "waar ben je") {
		t.Errorf("prompt %+v", q)
	}
	if len(r.store.rows) != 1 || r.store.rows[0].RepostID != repost || r.store.rows[0].AuthorID != author || r.store.rows[0].Language != "nl" {
		t.Errorf("rows %+v", r.store.rows)
	}
	// The log keeps what was really written.
	if log := r.audit.all(); !strings.Contains(log, core.ActorSystem+"|dragoman.translated") || !strings.Contains(log, dutch) ||
		!strings.Contains(log, `language="Dutch"`) || strings.Contains(log, "english=") {
		t.Errorf("audit %s", log)
	}
}

func TestWhatIsNeverTranslated(t *testing.T) {
	hook := snowflake.ID(9)
	for name, change := range map[string]func(*rig, *discord.Message){
		"a dm":                func(_ *rig, m *discord.Message) { m.GuildID = nil },
		"a webhook":           func(_ *rig, m *discord.Message) { m.WebhookID = &hook },
		"a bot":               func(_ *rig, m *discord.Message) { m.Author.Bot = true },
		"no author":           func(_ *rig, m *discord.Message) { m.Author.ID = 0 },
		"no text":             func(_ *rig, m *discord.Message) { m.Content = "" },
		"a pin notice":        func(_ *rig, m *discord.Message) { m.Type = discord.MessageTypeChannelPinnedMessage },
		"a forward":           func(_ *rig, m *discord.Message) { m.MessageSnapshots = make([]discord.MessageSnapshot, 1) },
		"a poll":              func(_ *rig, m *discord.Message) { m.Poll = &discord.Poll{} },
		"a voice message":     func(_ *rig, m *discord.Message) { m.Flags = discord.MessageFlagIsVoiceMessage },
		"english":             func(_ *rig, m *discord.Message) { m.Content = "what are you on about" },
		"a guild that's off":  func(r *rig, _ *discord.Message) { r.store.cfg.Enabled = false },
		"a plugin that's off": func(r *rig, _ *discord.Message) { r.gate.off = true },
		"an exempt channel":   func(r *rig, _ *discord.Message) { r.store.cfg.ExemptChannelIDs = []string{"101"} },
		"an opted out author": func(r *rig, _ *discord.Message) {
			r.store.cfg.MemberControls, r.store.cfg.OptOutUserIDs = true, []string{author}
		},
		"a paused guild":      func(r *rig, _ *discord.Message) { r.held = true },
		"unreadable settings": func(r *rig, _ *discord.Message) { r.store.err = errors.New("down") },
		"no content intent":   func(r *rig, _ *discord.Message) { r.p.scanning = false },
		"a masked lookalike":  func(_ *rig, m *discord.Message) { m.Content += tok("1") },
	} {
		r, m := newRig(t), msg(dutch)
		change(r, &m)
		r.say(m)
		if r.engine.asked() != 0 || len(r.engine.reposts()) != 0 {
			t.Errorf("%s was paid for or reposted", name)
		}
	}
}

func TestAnAttachmentIsKept(t *testing.T) {
	r := newRig(t)
	m := msg(dutch)
	m.Attachments = make([]discord.Attachment, 1)
	r.say(m)
	if got := r.engine.reposts(); len(got) != 1 || !got[0].Keep {
		t.Errorf("reposted %+v", got)
	}
	if log := r.audit.all(); !strings.Contains(log, "[Original]") {
		t.Errorf("audit %s", log)
	}
}

func TestHarmIsSoftenedAndSaidSo(t *testing.T) {
	r := newRig(t)
	r.engine.out = strings.Replace(good, `"harmful":""`, `"harmful":"threats"`, 1)
	r.say(msg(dutch))
	if got := r.engine.reposts(); len(got) != 1 || got[0].Policy != "threats" || got[0].Text == "" {
		t.Errorf("reposted %+v", got)
	}
	if log := r.audit.all(); !strings.Contains(log, `softened="threats"`) {
		t.Errorf("audit %s", log)
	}

	// Nothing left of it: moderation is told, and nothing is recorded as
	// translated.
	r = newRig(t)
	r.engine.out = `{"translate":true,"source":"nl","confidence":0.9,"harmful":"threats","text":""}`
	r.engine.postErr = quiet{}
	r.say(msg(dutch))
	if got := r.engine.reposts(); len(got) != 1 || got[0].Policy != "threats" || got[0].Text != "" {
		t.Errorf("reposted %+v", got)
	}
	if len(r.store.rows) != 0 || r.audit.all() != "" {
		t.Error("a message left alone was recorded")
	}
}

type quiet struct{}

func (quiet) Error() string     { return "not now" }
func (quiet) Unavailable() bool { return true }

// A guild that keeps no message text keeps none in its log either.
func TestNoEvidenceLogsNoText(t *testing.T) {
	r := newRig(t)
	r.engine.evidence = 0
	r.say(msg(dutch))
	if log := r.audit.all(); !strings.Contains(log, "dragoman.translated") || strings.Contains(log, dutch) {
		t.Errorf("audit %s", log)
	}
}

// spoken gives the guild one dialect, guide and all.
func (r *rig) spoken(lang, name string) {
	g := builtin["african american vernacular english"]
	r.store.langs = map[string]Dialect{lang: {GuildID: guild, Language: lang, Name: name, Guide: &g, Version: 1}}
	r.store.due = map[string]int{lang: 1000}
}

func TestADialectIsSpoken(t *testing.T) {
	const english, aave, name = "the train's always late", "that train be late every time", "African American Vernacular English"
	for what, tc := range map[string]struct {
		change func(*rig)
		calls  int
		text   string
	}{
		"picked":         {func(*rig) {}, 2, aave},
		"not picked":     {func(r *rig) { r.store.langs = nil }, 1, english},
		"another source": {func(r *rig) { r.engine.out = strings.Replace(good, `"nl"`, `"de"`, 1) }, 1, english},
		// A dialect that can't be had costs the dialect, not the translation.
		"no answer":   {func(r *rig) { r.engine.styleErr = errors.New("x") }, 2, english},
		"prose":       {func(r *rig) { r.engine.styled = "sure!" }, 3, english},
		"empty":       {func(r *rig) { r.engine.styled = `{"text":" "}` }, 3, english},
		"a link":      {func(r *rig) { r.engine.styled = `{"text":"see evil.com"}` }, 3, english},
		"a slur":      {func(r *rig) { r.engine.styled = `{"text":"nigga that train be late"}` }, 3, english},
		"much longer": {func(r *rig) { r.engine.styled = `{"text":"` + strings.Repeat("late ", 60) + `"}` }, 3, english},
	} {
		r := newRig(t)
		r.spoken("nl", name)
		r.engine.styled = `{"text":"` + aave + `"}`
		tc.change(r)
		r.say(msg(dutch))
		got := r.engine.reposts()
		if len(got) != 1 || got[0].Text != tc.text || r.engine.asked() != tc.calls {
			t.Errorf("%s: %d calls, posted %+v", what, r.engine.asked(), got)
		}
		// The log names the dialect only where it was really written, and
		// the panel says when it should have been and wasn't.
		if said := strings.Contains(r.audit.all(), `english="`+name+`"`); said != (tc.text == aave) {
			t.Errorf("%s: audit %s", what, r.audit.all())
		}
		if failed := r.p.trouble[guild] != ""; failed != (tc.calls >= 2 && tc.text == english) {
			t.Errorf("%s: trouble %q", what, r.p.trouble[guild])
		}
		// Each message counts for the guide, and a first answer that wasn't
		// good enough counts against it.
		if d := r.store.langs["nl"]; d.Uses != min(tc.calls-1, 1) || (d.Missed == 1) != (tc.calls >= 2 && tc.text == english) {
			t.Errorf("%s: uses %d, missed %d", what, d.Uses, d.Missed)
		}
	}

	r := newRig(t)
	r.spoken("nl", name)
	r.engine.styled = `{"text":"` + aave + ` ` + tok("1") + `"}`
	r.engine.out = strings.Replace(good, "always late", "always late "+tok("1"), 1)
	r.say(msg(dutch + " <@123456789012345678>"))
	if got := r.engine.reposts(); len(got) != 1 || got[0].Text != aave+" <@123456789012345678>" {
		t.Errorf("posted %+v", got)
	}
	if q := r.engine.prompts[1]; strings.Contains(q.User, "1234567") || q.Reserve != reserve || !strings.Contains(q.User, "the train's always late") {
		t.Errorf("dialect prompt %+v", q)
	}

	// No money says nothing about the guide.
	r = newRig(t)
	r.spoken("nl", name)
	r.engine.styleErr = quiet{}
	r.say(msg(dutch))
	if d := r.store.langs["nl"]; d.Uses != 0 || len(r.engine.reposts()) != 1 {
		t.Errorf("uses %d, posted %+v", d.Uses, r.engine.reposts())
	}
}

func TestFailuresLeaveTheMessageAndReachThePanel(t *testing.T) {
	r := newRig(t)
	r.engine.askErr = quiet{}
	r.say(msg(dutch))
	ix, _ := paneltest.IxCommand(t, op, "dragoman", nil)
	if pn := r.p.panel(t.Context(), ix, ""); pn.Color != core.ColorWarning || !strings.Contains(paneltest.IxTexts(pn), "not now") {
		t.Errorf("panel %q", paneltest.IxTexts(pn))
	}
	if len(r.engine.reposts()) != 0 {
		t.Error("reposted with no translation")
	}
	for _, err := range []error{errors.New("boom"), fmt.Errorf("x: %w", discordguard.ErrPaused)} {
		r.engine.askErr, r.engine.postErr = nil, err
		r.say(msg(dutch))
	}
	r.engine.postErr = nil
	r.say(msg(dutch))
	if len(r.store.rows) != 1 {
		t.Errorf("rows %+v, want only the one that landed", r.store.rows)
	}
	if pn := r.p.panel(t.Context(), ix, ""); pn.Color == core.ColorWarning {
		t.Error("the trouble outlived the translation that worked")
	}
}

// A short answer is noticed while its author is speaking Dutch, and not
// after.
func TestAConversationIsFollowed(t *testing.T) {
	r := newRig(t)
	r.say(msg("nee echt niet"))
	r.say(msg("nee"))
	r.now = r.now.Add(stickyFor + time.Second)
	r.say(msg("nee"))
	if n := r.engine.asked(); n != 2 {
		t.Errorf("asked %d times, want 2", n)
	}
}

func TestOneMemberCannotSpendItAll(t *testing.T) {
	r := newRig(t)
	for range memberMax + 5 {
		r.say(msg(dutch))
	}
	if n := r.engine.asked(); n != memberMax {
		t.Errorf("asked %d times, want %d", n, memberMax)
	}
	r.now = r.now.Add(memberWindow + time.Second)
	r.say(msg(dutch))
	if n := r.engine.asked(); n != memberMax+1 {
		t.Errorf("asked %d times after the window", n)
	}
}

// A channel keeps its order, and a guild only so many at once.
func TestChannelsAreSerialAndGuildsBounded(t *testing.T) {
	r := newRig(t)
	r.engine.block = make(chan struct{})
	for ch := range 4 {
		for range 2 {
			m := msg(dutch)
			m.ChannelID = snowflake.ID(200 + ch)
			r.p.HandleMessage(m)
		}
	}
	close(r.engine.block)
	r.p.wg.Wait()
	if r.engine.maxAsk > guildParallel || r.engine.asked() != 8 {
		t.Errorf("%d at once, %d asked", r.engine.maxAsk, r.engine.asked())
	}

	// What waited too long is left.
	r = newRig(t)
	r.p.run(t.Context(), job{guildID: guild, content: dutch, at: r.now.Add(-staleAfter - time.Second)})
	if r.engine.asked() != 0 {
		t.Error("a stale message was translated")
	}
}

func TestAPanicIsOneMessage(t *testing.T) {
	r := newRig(t)
	r.p.held = func(string) bool { panic("boom") }
	r.say(msg(dutch))
	r.p.held = func(string) bool { return false }
	r.say(msg(dutch))
	if len(r.engine.reposts()) != 1 {
		t.Error("the channel did not survive a panic")
	}
}

func TestSettingsAreReadOnceAndForgotten(t *testing.T) {
	r := newRig(t)
	r.say(msg("hello there you"))
	r.say(msg("hello there you"))
	if r.store.reads != 1 {
		t.Errorf("read %d times", r.store.reads)
	}
	if p := r.p.Perms(guild); !p.Has(discord.PermissionManageWebhooks) {
		t.Errorf("perms %v", p)
	}
	r.say(msg(dutch))
	r.p.ForgetGuild(guild)
	if len(r.p.sticky)+len(r.p.meter)+len(r.p.slots)+len(r.p.cfgs) != 0 {
		t.Error("something outlived the guild")
	}
	r.store.cfg.Enabled = false
	if r.p.Perms(guild) != 0 {
		t.Error("asks for permissions where it is off")
	}
	if err := r.p.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	r.store.cfg.Enabled = true
	r.p.forgetConfig(guild)
	n := r.engine.asked()
	r.say(msg(dutch))
	if r.engine.asked() != n {
		t.Error("translated after shutdown")
	}
}

func run(t *testing.T, user, sub string, args ...map[string]any) (*core.Ix, *paneltest.Wire) {
	t.Helper()
	return paneltest.IxCommand(t, user, "dragoman", func(p map[string]any) {
		opts := []any{}
		for _, a := range args {
			opts = append(opts, a)
		}
		p["data"].(map[string]any)["options"] = []any{map[string]any{"name": sub, "type": 1, "options": opts}}
	})
}

func TestDeleteIsForItsAuthorAndMods(t *testing.T) {
	link := map[string]any{"name": "message", "type": 3, "value": "https://discord.com/channels/1/2/" + repost}
	for _, tc := range []struct {
		user string
		on   bool
		want int
	}{{author, true, 1}, {other, true, 0}, {op, true, 1}, {author, false, 0}, {op, false, 1}} {
		r := newRig(t)
		r.store.cfg.MemberControls = tc.on
		r.store.rows = []Translation{{GuildID: guild, ChannelID: "101", RepostID: repost, AuthorID: author}}
		ix, w := run(t, tc.user, "delete", link)
		r.p.handleDelete(t.Context(), ix)
		if !w.Answered() || len(r.ops.deleted) != tc.want {
			t.Errorf("%s deleted %v", tc.user, r.ops.deleted)
		}
		if got := strings.Contains(r.audit.all(), "dragoman.deleted"); got != (tc.want == 1) {
			t.Errorf("%s: audit %q", tc.user, r.audit.all())
		}
	}
	r := newRig(t)
	for _, v := range []string{"nonsense", repost} {
		ix, w := run(t, author, "delete", map[string]any{"name": "message", "type": 3, "value": v})
		r.p.handleDelete(t.Context(), ix)
		if !w.Answered() || len(r.ops.deleted) != 0 {
			t.Errorf("%q deleted %v", v, r.ops.deleted)
		}
	}
	r.store.rows = []Translation{{GuildID: guild, ChannelID: "101", RepostID: repost, AuthorID: author}}
	r.store.cfg.MemberControls, r.ops.err = true, errors.New("no")
	ix, w := run(t, author, "delete", link)
	r.p.handleDelete(t.Context(), ix)
	r.store.err = errors.New("down")
	ix2, w2 := run(t, author, "delete", link)
	r.p.handleDelete(t.Context(), ix2)
	if !w.Answered() || !w2.Answered() || r.audit.all() != "" {
		t.Errorf("a failure went unanswered or was audited: %q", r.audit.all())
	}
}

// Off until an admin offers it: nobody is taken for opted out, nobody can
// opt out, and coming back in still works.
func TestMemberControlsAreOffByDefault(t *testing.T) {
	r := newRig(t)
	r.store.cfg.OptOutUserIDs = []string{author}
	r.say(msg(dutch))
	if len(r.engine.reposts()) != 1 {
		t.Error("an opt-out counted where the server offers none")
	}
	for _, on := range []bool{false, true} {
		ix, w := run(t, author, "opt-out", map[string]any{"name": "enabled", "type": 5, "value": on})
		r.p.handleOptOut(t.Context(), ix)
		if !w.Answered() || len(r.store.cfg.OptOutUserIDs) != 0 {
			t.Errorf("%v: %v", on, r.store.cfg.OptOutUserIDs)
		}
	}
	r.p.forgetConfig(guild)
	r.store.err = errors.New("down")
	ix, w := run(t, author, "opt-out", map[string]any{"name": "enabled", "type": 5, "value": true})
	r.p.handleOptOut(t.Context(), ix)
	if !w.Answered() {
		t.Error("unanswered")
	}
}

func TestOptOutSetsTheCaller(t *testing.T) {
	r := newRig(t)
	r.store.cfg.MemberControls = true
	for _, on := range []bool{true, false} {
		ix, w := run(t, author, "opt-out", map[string]any{"name": "enabled", "type": 5, "value": on})
		r.p.handleOptOut(t.Context(), ix)
		if !w.Answered() || slices.Contains(r.store.cfg.OptOutUserIDs, author) != on {
			t.Errorf("%v: %v", on, r.store.cfg.OptOutUserIDs)
		}
	}
	r.store.err = errors.New("down")
	ix, w := run(t, author, "opt-out", map[string]any{"name": "enabled", "type": 5, "value": true})
	r.p.handleOptOut(t.Context(), ix)
	if !w.Answered() {
		t.Error("unanswered")
	}
}

func TestThePanelDisablesWhatTheViewerCannotDo(t *testing.T) {
	r := newRig(t)
	r.spoken("nl", "Jamaican English")
	for user, can := range map[string]bool{op: true, other: false} {
		ix, w := run(t, user, "settings")
		r.p.handleSettings(t.Context(), ix)
		if !w.Answered() {
			t.Error("unanswered")
		}
		pn := r.p.panel(t.Context(), ix, "")
		for id, b := range paneltest.IxButtons(pn) {
			if b.Disabled == can && !strings.HasPrefix(id, panelView) {
				t.Errorf("%s: %s disabled %v", user, id, b.Disabled)
			}
		}
		if !strings.Contains(paneltest.IxTexts(pn), "Dutch becomes Jamaican English") {
			t.Errorf("panel %q", paneltest.IxTexts(pn))
		}
	}
	r.engine.noKey, r.p.scanning = true, false
	ix, _ := run(t, op, "settings")
	if pn := r.p.panel(t.Context(), ix, ""); pn.Color != core.ColorWarning || !strings.Contains(paneltest.IxTexts(pn), "no key") {
		t.Errorf("panel %q", paneltest.IxTexts(pn))
	}
	r.p.forgetConfig(guild)
	r.store.err = errors.New("down")
	if pn := r.p.panel(t.Context(), ix, ""); pn.Color != core.ColorError {
		t.Error("unreadable settings look fine")
	}
}

func TestEveryControlAnswersAndSaves(t *testing.T) {
	r := newRig(t)
	r.store.cfg = Config{ExemptChannelIDs: []string{"301"}}
	click := func(id string, h func(context.Context, *core.Ix, string)) {
		t.Helper()
		ix, w := paneltest.IxClick(t, op, id, nil)
		h(t.Context(), ix, id)
		if !w.Answered() {
			t.Errorf("%s unanswered", id)
		}
	}
	click(panelSet+"on", r.p.handlePanelSet)
	if !r.store.cfg.Enabled {
		t.Error("not on")
	}
	click(panelSet+"off", r.p.handlePanelSet)
	if r.store.cfg.Enabled {
		t.Error("not off")
	}
	click(panelSet+"members:on", r.p.handlePanelSet)
	if !r.store.cfg.MemberControls {
		t.Error("member controls not on")
	}
	click(panelSet+"members:off", r.p.handlePanelSet)
	if r.store.cfg.MemberControls {
		t.Error("member controls not off")
	}
	click(panelSet+"gone", r.p.handlePanelSet)

	ix, _ := run(t, op, "settings")
	pn := r.p.panel(t.Context(), ix, "")
	pick, _ := paneltest.IxComponent(t, op, panelSet+"exempt", 8, &pn, "302")
	r.p.handlePanelSet(t.Context(), pick, panelSet+"exempt")
	if !slices.Equal(r.store.cfg.ExemptChannelIDs, []string{"302"}) {
		t.Errorf("exempt %v", r.store.cfg.ExemptChannelIDs)
	}
	same := r.p.panel(t.Context(), ix, "")
	pick, _ = paneltest.IxComponent(t, op, panelSet+"exempt", 8, &same, "302")
	r.p.handlePanelSet(t.Context(), pick, panelSet+"exempt")
	for _, want := range []string{"dragoman.enabled_set|off|on", "dragoman.enabled_set|on|off", "dragoman.member_controls_set|off|on",
		"dragoman.channel_exempted||<#302>", "dragoman.channel_included||<#301>"} {
		if !strings.Contains(r.audit.all(), want) {
			t.Errorf("no audit of %s in\n%s", want, r.audit.all())
		}
	}

	r.store.err = errors.New("down")
	click(panelSet+"on", r.p.handlePanelSet)
	click(panelSet+"members:on", r.p.handlePanelSet)
	pick, _ = paneltest.IxComponent(t, op, panelSet+"exempt", 8, &same, "303")
	r.p.handlePanelSet(t.Context(), pick, panelSet+"exempt")
}

// Every leaf has a handler and a tier, which Finalize enforces.
func TestInitRegistersAFullyWiredCommandTree(t *testing.T) {
	r := newRig(t)
	if err := r.p.commands.Finalize(); err != nil {
		t.Fatal(err)
	}
	if err := r.p.Start(t.Context()); err != nil || r.p.Name() != "dragoman" || r.p.Help().Title == "" {
		t.Error("not a plugin")
	}
}

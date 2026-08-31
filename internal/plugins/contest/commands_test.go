package contest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/secret"
)

// A discordgo Session pointed at a RoundTripper instead of Discord.
//
// The alternative is extracting each handler's decision into a pure function
// and testing that, which is what roles does. It is worse here: what these
// handlers mostly do is decide which of several sentences a member reads, and
// a test that asserts a return value rather than the sentence that was
// actually put on the wire would not have caught any of the mistakes worth
// catching. So the whole handler runs, and this records what it sent.
type recordingTransport struct {
	mu    sync.Mutex
	calls []recordedCall
	// status lets a test make Discord fail, since a handler that assumes the
	// response landed is a handler that swallows its own error.
	status int
}

type recordedCall struct {
	method string
	path   string
	body   string
}

func (rt *recordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var body string
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}
	rt.mu.Lock()
	rt.calls = append(rt.calls, recordedCall{r.Method, r.URL.Path, body})
	status := rt.status
	rt.mu.Unlock()

	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"id":"1"}`)),
		Request:    r,
	}, nil
}

// said returns everything the handler put on the wire, joined, so an
// assertion can be about the sentence a member reads rather than about which
// REST call carried it.
func (rt *recordingTransport) said() string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	var b strings.Builder
	for _, c := range rt.calls {
		b.WriteString(c.body)
		b.WriteString("\n")
	}
	return b.String()
}

func stubSession() (*discordgo.Session, *recordingTransport) {
	rt := &recordingTransport{}
	return &discordgo.Session{
		Client:      &http.Client{Transport: rt},
		Ratelimiter: discordgo.NewRatelimiter(),
		Token:       "Bot test",
	}, rt
}

// interaction builds a command invocation for one leaf of /contest.
func interaction(leaf string, opts ...*discordgo.ApplicationCommandInteractionDataOption) *discordgo.InteractionCreate {
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID:        "i1",
		Token:     "tok",
		GuildID:   "g1",
		ChannelID: "chan-1",
		Type:      discordgo.InteractionApplicationCommand,
		Member: &discordgo.Member{
			Nick: "mod",
			User: &discordgo.User{ID: "mod-1", Username: "mod", GlobalName: "mod"},
		},
		Data: discordgo.ApplicationCommandInteractionData{
			Name: "contest",
			Options: []*discordgo.ApplicationCommandInteractionDataOption{{
				Name: leaf, Type: discordgo.ApplicationCommandOptionSubCommand, Options: opts,
			}},
		},
	}}
}

func strOpt(name, v string) *discordgo.ApplicationCommandInteractionDataOption {
	return &discordgo.ApplicationCommandInteractionDataOption{
		Name: name, Type: discordgo.ApplicationCommandOptionString, Value: v,
	}
}

func intOpt(name string, v int) *discordgo.ApplicationCommandInteractionDataOption {
	return &discordgo.ApplicationCommandInteractionDataOption{
		// Discord sends numbers as float64 over JSON, and IntValue asserts
		// exactly that, so a test passing an int here would panic in a way
		// production never does.
		Name: name, Type: discordgo.ApplicationCommandOptionInteger, Value: float64(v),
	}
}

func modalSubmit(customID string, fields map[string]string) *discordgo.InteractionCreate {
	var rows []discordgo.MessageComponent
	for id, v := range fields {
		rows = append(rows, &discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			&discordgo.TextInput{CustomID: id, Value: v},
		}})
	}
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID: "i2", Token: "tok", GuildID: "g1", ChannelID: "chan-1",
		Type: discordgo.InteractionModalSubmit,
		Member: &discordgo.Member{
			Nick: "dana", User: &discordgo.User{ID: "u9", Username: "dana"},
		},
		Data: discordgo.ModalSubmitInteractionData{CustomID: customID, Components: rows},
	}}
}

// --- /contest new ---------------------------------------------------------

func TestNewCreatesAForumAndAnnouncesIt(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	p := newTestPlugin(t, store, ops, sched, audit, "")
	p.now = func() time.Time { return base }
	s, _ := stubSession()

	p.handleNew(context.Background(), s, interaction("new",
		strOpt("title", "Neon Cats"), strOpt("theme", "cats, but neon"),
		strOpt("announce-for", "1h"), strOpt("submit-for", "48h"), strOpt("vote-for", "24h"),
		intOpt("picks", 2)))

	c, err := store.LiveContest(context.Background(), "g1")
	if err != nil {
		t.Fatalf("no contest was created: %v", err)
	}
	if c.Title != "Neon Cats" || c.Theme != "cats, but neon" || c.MaxVotes != 2 {
		t.Errorf("contest = %+v", c)
	}
	if !c.SubmitAt.Equal(base.Add(time.Hour)) || !c.VoteAt.Equal(base.Add(49*time.Hour)) {
		t.Errorf("deadlines are not stacked: %v %v %v", c.SubmitAt, c.VoteAt, c.ResultsAt)
	}
	if c.Phase != PhaseAnnounce {
		t.Errorf("phase = %s, want announce", c.Phase)
	}
	if c.AnnounceChannelID != "chan-1" {
		t.Errorf("announcements went to %q, want the channel it was run in", c.AnnounceChannelID)
	}

	if len(ops.created) != 1 || ops.created[0].Type != discordgo.ChannelTypeGuildForum {
		t.Fatalf("created = %+v, want one forum channel", ops.created)
	}
	if ops.created[0].Name != "contest-neon-cats" {
		t.Errorf("forum name = %q", ops.created[0].Name)
	}
	// Posting starts denied: a contest in its announce phase is a thing
	// people are being told about, not a thing they can enter yet.
	if len(ops.created[0].PermissionOverwrites) != 1 ||
		ops.created[0].PermissionOverwrites[0].Deny != postingPerms {
		t.Errorf("forum did not start closed to posting: %+v", ops.created[0].PermissionOverwrites)
	}

	// The tick job has to exist now, or nothing ever moves the phase on.
	if !sched.has("g1:contest-tick") {
		t.Error("no tick job registered, so the contest would never advance")
	}
	if len(audit.all()) != 1 || !strings.Contains(audit.all()[0], "contest.created") {
		t.Errorf("audit = %v", audit.all())
	}
}

// announce-for 0 means start now, and the forum has to be open before the
// message telling people to post in it.
func TestNewWithNoAnnounceWindowOpensSubmissionsImmediately(t *testing.T) {
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	p := newTestPlugin(t, store, ops, sched, audit, "")
	s, _ := stubSession()

	p.handleNew(context.Background(), s, interaction("new",
		strOpt("title", "quickfire"), strOpt("announce-for", "0")))

	c, err := store.LiveContest(context.Background(), "g1")
	if err != nil {
		t.Fatalf("no contest: %v", err)
	}
	if c.Phase != PhaseSubmit {
		t.Fatalf("phase = %s, want submit", c.Phase)
	}
	if len(ops.overwrit) != 1 || ops.overwrit[0] != 0 {
		t.Errorf("forum was not opened: %v", ops.overwrit)
	}
	if len(ops.sentTo("chan-1")) != 2 {
		t.Errorf("announcements = %d, want the opening card and the submissions-open one",
			len(ops.sentTo("chan-1")))
	}
}

func TestOnlyOneContestRunsAtATime(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	if err := store.CreateContest(context.Background(), liveContest(PhaseSubmit, base)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p := newTestPlugin(t, store, ops, sched, audit, "")
	s, rt := stubSession()

	p.handleNew(context.Background(), s, interaction("new", strOpt("title", "another one")))

	if len(ops.created) != 0 {
		t.Error("a second forum was created while a contest was already running")
	}
	if !strings.Contains(rt.said(), "already") {
		t.Errorf("the refusal did not say why:\n%s", rt.said())
	}
}

func TestNewRefusesAnUnparseableWindow(t *testing.T) {
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	p := newTestPlugin(t, store, ops, sched, audit, "")
	s, _ := stubSession()

	p.handleNew(context.Background(), s, interaction("new",
		strOpt("title", "x"), strOpt("submit-for", "banana")))

	if _, err := store.LiveContest(context.Background(), "g1"); err == nil {
		t.Error("a contest was created with a window that does not parse")
	}
}

// --- prizes ---------------------------------------------------------------

func TestPledgingAPrizeSealsTheCodeAndNeverLogsIt(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	sealer, err := secret.New(testKey())
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	if err := store.CreateContest(context.Background(), liveContest(PhaseAnnounce, base)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p := newTestPlugin(t, store, ops, sched, audit, "")
	p.sealer = sealer
	s, rt := stubSession()

	p.handlePrizeModal(context.Background(), s, modalSubmit(prizeModalPrefix+"c1", map[string]string{
		prizeFieldTitle:   "a steam key",
		prizeFieldDetails: "for hollow knight",
		prizeFieldCode:    "STEAM-KEY-1234",
	}), prizeModalPrefix+"c1")

	prizes, err := store.Prizes(context.Background(), "c1")
	if err != nil || len(prizes) != 1 {
		t.Fatalf("prizes = %+v, %v", prizes, err)
	}
	if !prizes[0].HasSecret() {
		t.Fatal("the code was not stored")
	}
	got, err := sealer.Open(prizes[0].SecretSealed)
	if err != nil || got != "STEAM-KEY-1234" {
		t.Fatalf("sealed code did not round trip: %q %v", got, err)
	}
	if string(prizes[0].SecretSealed) == "STEAM-KEY-1234" {
		t.Fatal("the code was stored in the clear")
	}

	// Not in the audit row, and not in anything sent to Discord. Both of
	// those are read by other people.
	for _, row := range audit.all() {
		if strings.Contains(row, "STEAM-KEY-1234") {
			t.Errorf("a prize code reached the audit log: %s", row)
		}
	}
	if strings.Contains(rt.said(), "STEAM-KEY-1234") {
		t.Error("a prize code was echoed back into the channel")
	}
	for _, m := range ops.sentTo("announce-1") {
		blob, _ := json.Marshal(m)
		if strings.Contains(string(blob), "STEAM-KEY-1234") {
			t.Error("a prize code reached the public announcement")
		}
	}
}

// Without MERLIN_SECRET_KEY there is nowhere safe to put a code, so the
// pledge must be refused rather than stored in the clear or silently dropped.
func TestPledgingACodeWithNoSecretKeyIsRefused(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	if err := store.CreateContest(context.Background(), liveContest(PhaseAnnounce, base)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p := newTestPlugin(t, store, ops, sched, audit, "") // sealer stays nil
	s, rt := stubSession()

	p.handlePrizeModal(context.Background(), s, modalSubmit(prizeModalPrefix+"c1", map[string]string{
		prizeFieldTitle: "a steam key",
		prizeFieldCode:  "STEAM-KEY-1234",
	}), prizeModalPrefix+"c1")

	prizes, _ := store.Prizes(context.Background(), "c1")
	if len(prizes) != 0 {
		t.Fatalf("a pledge was stored anyway: %+v", prizes)
	}
	if !strings.Contains(rt.said(), "MERLIN_SECRET_KEY") {
		t.Errorf("the refusal did not say what to fix:\n%s", rt.said())
	}
}

func TestAPledgeWithoutACodeIsFine(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	if err := store.CreateContest(context.Background(), liveContest(PhaseAnnounce, base)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p := newTestPlugin(t, store, ops, sched, audit, "")
	s, _ := stubSession()

	p.handlePrizeModal(context.Background(), s, modalSubmit(prizeModalPrefix+"c1", map[string]string{
		prizeFieldTitle: "50 usdc, i will send it myself",
	}), prizeModalPrefix+"c1")

	prizes, _ := store.Prizes(context.Background(), "c1")
	if len(prizes) != 1 || prizes[0].HasSecret() {
		t.Fatalf("prizes = %+v", prizes)
	}
	if len(ops.sentTo("announce-1")) != 1 {
		t.Error("the pledge was not announced, so nobody knows the pool grew")
	}
}

func TestAModalForAFinishedContestIsRefused(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	if err := store.CreateContest(context.Background(), liveContest(PhaseAnnounce, base)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p := newTestPlugin(t, store, ops, sched, audit, "")
	s, _ := stubSession()

	// A modal left open while a contest ended, submitted against a different
	// contest id.
	p.handlePrizeModal(context.Background(), s,
		modalSubmit(prizeModalPrefix+"gone", map[string]string{prizeFieldTitle: "a key"}),
		prizeModalPrefix+"gone")

	if prizes, _ := store.Prizes(context.Background(), "c1"); len(prizes) != 0 {
		t.Error("a stale modal pledged into the wrong contest")
	}
}

func TestAnEmptyPrizeTitleIsRefused(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	if err := store.CreateContest(context.Background(), liveContest(PhaseAnnounce, base)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p := newTestPlugin(t, store, ops, sched, audit, "")
	s, _ := stubSession()

	p.handlePrizeModal(context.Background(), s,
		modalSubmit(prizeModalPrefix+"c1", map[string]string{prizeFieldTitle: "   "}),
		prizeModalPrefix+"c1")

	if prizes, _ := store.Prizes(context.Background(), "c1"); len(prizes) != 0 {
		t.Error("an unnamed prize was pledged")
	}
}

// A donor may pull their own pledge and nobody else's, and the check is part
// of the delete rather than a step before it.
func TestOnlyTheDonorCanUnpledge(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	if err := store.CreateContest(context.Background(), liveContest(PhaseAnnounce, base)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := store.AddPrize(context.Background(), Prize{
		ID: "p1", ContestID: "c1", DonorID: "someone-else", Title: "not yours",
	}); err != nil {
		t.Fatalf("seed prize: %v", err)
	}
	p := newTestPlugin(t, store, ops, sched, audit, "")
	s, _ := stubSession()

	p.handleUnpledge(context.Background(), s, interaction("unpledge"))

	if prizes, _ := store.Prizes(context.Background(), "c1"); len(prizes) != 1 {
		t.Error("somebody pulled a pledge that was not theirs")
	}
}

func TestUnpledgeTakesBackYourMostRecentByDefault(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	if err := store.CreateContest(context.Background(), liveContest(PhaseAnnounce, base)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, pr := range []Prize{
		{ID: "p1", ContestID: "c1", DonorID: "mod-1", Title: "first"},
		{ID: "p2", ContestID: "c1", DonorID: "mod-1", Title: "second"},
	} {
		if err := store.AddPrize(context.Background(), pr); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	p := newTestPlugin(t, store, ops, sched, audit, "")
	s, _ := stubSession()

	p.handleUnpledge(context.Background(), s, interaction("unpledge"))

	left, _ := store.Prizes(context.Background(), "c1")
	if len(left) != 1 || left[0].ID != "p1" {
		t.Errorf("prizes left = %+v, want just the first one", left)
	}

	// And by id when one is named.
	p.handleUnpledge(context.Background(), s, interaction("unpledge", strOpt("which", "p1")))
	if left, _ := store.Prizes(context.Background(), "c1"); len(left) != 0 {
		t.Errorf("naming a pledge did not remove it: %+v", left)
	}
}

func TestPledgeAutocompleteOffersOnlyYourOwn(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	if err := store.CreateContest(context.Background(), liveContest(PhaseAnnounce, base)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, pr := range []Prize{
		{ID: "p1", ContestID: "c1", DonorID: "mod-1", Title: "mine, a steam key"},
		{ID: "p2", ContestID: "c1", DonorID: "other", Title: "theirs, a steam key"},
	} {
		if err := store.AddPrize(context.Background(), pr); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	p := newTestPlugin(t, store, ops, sched, audit, "")

	got := p.autocompletePledges(context.Background(), interaction("unpledge"), "which", "steam")
	if len(got) != 1 || got[0].Value != "p1" {
		t.Errorf("autocomplete = %+v, want only the caller's own pledge", got)
	}
	if none := p.autocompletePledges(context.Background(), interaction("unpledge"), "which", "zzz"); len(none) != 0 {
		t.Errorf("autocomplete ignored the filter: %+v", none)
	}
}

// --- claim ----------------------------------------------------------------

// The fallback for a prize DM that never landed. It must hand the code over
// and only then wipe it, and only to the person it was awarded to.
func TestClaimHandsOverAPrizeAndWipesItAfter(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	sealer, err := secret.New(testKey())
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	sealed, err := sealer.Seal("STEAM-KEY-9999")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	if err := store.CreateContest(context.Background(), liveContest(PhaseResults, base)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	winner := "mod-1"
	when := base
	if err := store.AddPrize(context.Background(), Prize{
		ID: "p1", ContestID: "c1", DonorID: "u9", Title: "a steam key",
		SecretSealed: sealed, AwardedTo: &winner, AwardedAt: &when,
	}); err != nil {
		t.Fatalf("seed prize: %v", err)
	}
	p := newTestPlugin(t, store, ops, sched, audit, "")
	p.sealer = sealer
	s, rt := stubSession()

	p.handleClaim(context.Background(), s, interaction("claim"))

	if !strings.Contains(rt.said(), "STEAM-KEY-9999") {
		t.Errorf("the code was not handed over:\n%s", rt.said())
	}
	prizes, _ := store.Prizes(context.Background(), "c1")
	if prizes[0].HasSecret() {
		t.Error("the code survived being claimed")
	}
}

func TestClaimShowsNothingToSomebodyWhoWonNothing(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	if err := store.CreateContest(context.Background(), liveContest(PhaseResults, base)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	other := "somebody-else"
	when := base
	if err := store.AddPrize(context.Background(), Prize{
		ID: "p1", ContestID: "c1", DonorID: "u9", Title: "a steam key",
		SecretSealed: []byte("ciphertext"), AwardedTo: &other, AwardedAt: &when,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p := newTestPlugin(t, store, ops, sched, audit, "")
	s, rt := stubSession()

	p.handleClaim(context.Background(), s, interaction("claim"))

	if strings.Contains(rt.said(), "ciphertext") {
		t.Error("somebody else's prize was shown")
	}
	prizes, _ := store.Prizes(context.Background(), "c1")
	if !prizes[0].HasSecret() {
		t.Error("a claim by the wrong person wiped the code")
	}
}

// --- link -----------------------------------------------------------------

// One command that answers whatever the current phase actually calls for, so
// nobody has to know which of three things applies right now.
func TestLinkAnswersThePhaseItIsIn(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	for _, tc := range []struct {
		phase Phase
		want  string
	}{
		{PhaseAnnounce, "Submissions open"},
		{PhaseSubmit, "Post your entry"},
		{PhaseVote, "Sign in with Discord"},
		{PhaseResults, "gallery is still up"},
	} {
		store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
		if err := store.CreateContest(context.Background(), liveContest(tc.phase, base)); err != nil {
			t.Fatalf("seed: %v", err)
		}
		p := newTestPlugin(t, store, ops, sched, audit, "https://w.test")
		s, rt := stubSession()

		p.handleLink(context.Background(), s, interaction("link"))

		if !strings.Contains(rt.said(), tc.want) {
			t.Errorf("%s: answer does not mention %q:\n%s", tc.phase, tc.want, rt.said())
		}
	}
}

// The button on the announcement is the same surface, so the two cannot say
// different things about the same contest.
func TestTheVoteButtonAnswersLikeTheCommand(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	if err := store.CreateContest(context.Background(), liveContest(PhaseVote, base)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p := newTestPlugin(t, store, ops, sched, audit, "https://w.test")
	s, rt := stubSession()

	p.handleLinkButton(context.Background(), s,
		interaction("link"), linkButtonPrefix+"abcdefghijklmnop")

	if !strings.Contains(rt.said(), "https://w.test/c/abcdefghijklmnop") {
		t.Errorf("the button gave no gallery link:\n%s", rt.said())
	}
}

// A deployment with no Worker still runs contests in Discord. It must say so
// rather than handing out a link to nowhere.
func TestLinkWithNoWorkerSaysSoRatherThanLinkingNowhere(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	if err := store.CreateContest(context.Background(), liveContest(PhaseVote, base)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p := newTestPlugin(t, store, ops, sched, audit, "")
	s, rt := stubSession()

	p.handleLink(context.Background(), s, interaction("link"))

	said := rt.said()
	if !strings.Contains(said, "no voting page") {
		t.Errorf("did not say the page is missing:\n%s", said)
	}
	if strings.Contains(said, "http") {
		t.Errorf("handed out a link anyway:\n%s", said)
	}
}

func TestLinkWithNoContestSaysSo(t *testing.T) {
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	p := newTestPlugin(t, store, ops, sched, audit, "https://w.test")
	s, rt := stubSession()

	p.handleLink(context.Background(), s, interaction("link"))

	if !strings.Contains(rt.said(), "No contest") {
		t.Errorf("said something else:\n%s", rt.said())
	}
}

// --- status ---------------------------------------------------------------

// The severity is tracked as the body is built, never recovered by scanning
// the finished text, which is the bug /config status had.
func TestStatusReportsAnUnreachableWorkerAsAWarning(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	c := liveContest(PhaseVote, base)
	c.TallyError = "d1 is having a moment"
	if err := store.CreateContest(context.Background(), c); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// No server listening, so every Worker call fails.
	p := newTestPlugin(t, store, ops, sched, audit, "http://127.0.0.1:1")
	s, rt := stubSession()

	p.handleStatus(context.Background(), s, interaction("status"))

	said := rt.said()
	for _, want := range []string{"unreachable", "d1 is having a moment", "keeps retrying"} {
		if !strings.Contains(said, want) {
			t.Errorf("status omits %q:\n%s", want, said)
		}
	}
}

func TestStatusWithNoWorkerNamesTheEnvVarToSet(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	if err := store.CreateContest(context.Background(), liveContest(PhaseSubmit, base)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p := newTestPlugin(t, store, ops, sched, audit, "")
	s, rt := stubSession()

	p.handleStatus(context.Background(), s, interaction("status"))

	said := rt.said()
	if !strings.Contains(said, "MERLIN_CONTEST_WORKER_URL") {
		t.Errorf("status did not name the missing setting:\n%s", said)
	}
	// And the same for prize codes, which are off without a secret key.
	if !strings.Contains(said, "MERLIN_SECRET_KEY") {
		t.Errorf("status did not say prize codes are off:\n%s", said)
	}
}

func TestStatusWithNothingEverRunSaysSo(t *testing.T) {
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	p := newTestPlugin(t, store, ops, sched, audit, "")
	s, rt := stubSession()

	p.handleStatus(context.Background(), s, interaction("status"))

	if !strings.Contains(rt.said(), "Nothing has been run") {
		t.Errorf("said something else:\n%s", rt.said())
	}
}

// --- advance and cancel ---------------------------------------------------

func TestAdvanceMovesThePhaseAndAuditsIt(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	if err := store.CreateContest(context.Background(), liveContest(PhaseAnnounce, base)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p := newTestPlugin(t, store, ops, sched, audit, "")
	p.now = func() time.Time { return base }
	s, _ := stubSession()

	p.handleAdvance(context.Background(), s, interaction("advance"))

	got, _ := store.LiveContest(context.Background(), "g1")
	if got.Phase != PhaseSubmit {
		t.Fatalf("phase = %s, want submit", got.Phase)
	}
	if len(audit.all()) != 1 || !strings.Contains(audit.all()[0], "contest.phase_advanced") {
		t.Errorf("audit = %v", audit.all())
	}
}

// Cancelling is a decision about the contest, not about anybody's work, and
// channel deletion has no undo.
func TestCancelDeletesNothing(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	if err := store.CreateContest(context.Background(), liveContest(PhaseSubmit, base)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := store.UpsertSubmission(context.Background(), Submission{
		ID: "s1", ContestID: "c1", UserID: "u1", ThreadID: "t1", Title: "one",
	}); err != nil {
		t.Fatalf("seed submission: %v", err)
	}
	p := newTestPlugin(t, store, ops, sched, audit, "")
	p.now = func() time.Time { return base }
	p.SyncGuild(context.Background(), "g1")
	s, _ := stubSession()

	p.handleCancel(context.Background(), s, interaction("cancel"))

	got, err := store.LiveContest(context.Background(), "g1")
	if err == nil {
		t.Fatalf("contest is still live: %+v", got)
	}
	if subs, _ := store.Submissions(context.Background(), "c1"); len(subs) != 1 {
		t.Error("cancelling threw away an entry")
	}
	if len(ops.overwrit) != 1 || ops.overwrit[0] != postingPerms {
		t.Errorf("the forum was not closed to posting: %v", ops.overwrit)
	}
	if sched.has("g1:contest-tick") {
		t.Error("the tick job outlived the contest it existed for")
	}
	if len(audit.all()) != 1 || !strings.Contains(audit.all()[0], "contest.cancelled") {
		t.Errorf("audit = %v", audit.all())
	}
}

func TestCancelWithNothingRunningSaysSo(t *testing.T) {
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	p := newTestPlugin(t, store, ops, sched, audit, "")
	s, rt := stubSession()

	p.handleCancel(context.Background(), s, interaction("cancel"))

	if !strings.Contains(rt.said(), "No contest is running") {
		t.Errorf("said something else:\n%s", rt.said())
	}
}

// --- configure ------------------------------------------------------------

func TestConfigureRoundTrips(t *testing.T) {
	store, ops, sched, audit := newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}
	p := newTestPlugin(t, store, ops, sched, audit, "")
	s, rt := stubSession()

	i := interaction("set", intOpt("picks", 5))
	// configure is a subcommand group, so the leaf sits one level deeper.
	i.Data = discordgo.ApplicationCommandInteractionData{
		Name: "contest",
		Options: []*discordgo.ApplicationCommandInteractionDataOption{{
			Name: "configure", Type: discordgo.ApplicationCommandOptionSubCommandGroup,
			Options: []*discordgo.ApplicationCommandInteractionDataOption{{
				Name: "set", Type: discordgo.ApplicationCommandOptionSubCommand,
				Options: []*discordgo.ApplicationCommandInteractionDataOption{intOpt("picks", 5)},
			}},
		}},
	}
	p.handleConfigureSet(context.Background(), s, i)

	cfg, err := store.GetConfig(context.Background(), "g1")
	if err != nil || cfg.DefaultMaxVotes != 5 {
		t.Fatalf("config = %+v, %v", cfg, err)
	}
	if len(audit.all()) != 1 || !strings.Contains(audit.all()[0], "contest.configured") {
		t.Errorf("audit = %v", audit.all())
	}

	rt.mu.Lock()
	rt.calls = nil
	rt.mu.Unlock()
	p.handleConfigureShow(context.Background(), s, interaction("show"))
	if !strings.Contains(rt.said(), "5") {
		t.Errorf("show did not report the new value:\n%s", rt.said())
	}
}

// --- registration ---------------------------------------------------------

// Finalize is what refuses to boot on a leaf with no declared tier, so this
// is the test that a forgotten PermSpec cannot ship.
func TestEveryLeafIsRegisteredWithATier(t *testing.T) {
	router := core.NewCommandRouter(nil, nil, quietLog())
	p := newTestPlugin(t, newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}, "")
	p.commands = router
	p.registerCommands()

	if err := router.Finalize(); err != nil {
		t.Fatalf("the command tree does not finalize, so the bot would not start: %v", err)
	}
	for _, want := range []string{actionManage, actionConfigure} {
		found := false
		for _, a := range router.Actions() {
			if a == want {
				found = true
			}
		}
		if !found {
			t.Errorf("action %q is not registered, so /config permissions cannot name it", want)
		}
	}
}

func TestPluginLifecycleIsInert(t *testing.T) {
	p := newTestPlugin(t, newFakeStore(), newFakeOps(), newFakeSched(), &fakeAudit{}, "")
	if p.Name() != "contest" {
		t.Errorf("Name = %q", p.Name())
	}
	if err := p.Start(context.Background()); err != nil {
		t.Errorf("Start: %v", err)
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	p.ForgetGuild("g1")
}

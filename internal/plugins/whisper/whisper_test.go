package whisper

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/discordguard"
)

// --- fakes ---

type fakeOps struct {
	mu       sync.Mutex
	channels map[string]*discordgo.Channel
	hooks    map[string][]*discordgo.Webhook
	created  int
	execErr  error
	posted   []*discordgo.WebhookParams
	messages []*discordgo.Message // the channel, oldest first
}

func newFakeOps() *fakeOps {
	return &fakeOps{
		channels: map[string]*discordgo.Channel{
			"c1": {ID: "c1", GuildID: "g1", Type: discordgo.ChannelTypeGuildText},
		},
		hooks: map[string][]*discordgo.Webhook{},
	}
}

func (f *fakeOps) Channel(id string, _ ...discordgo.RequestOption) (*discordgo.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch, ok := f.channels[id]
	if !ok {
		return nil, errors.New("unknown channel")
	}
	return ch, nil
}

func (f *fakeOps) ChannelWebhooks(id string, _ ...discordgo.RequestOption) ([]*discordgo.Webhook, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hooks[id], nil
}

func (f *fakeOps) WebhookCreate(id, name, _ string, _ ...discordgo.RequestOption) (*discordgo.Webhook, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created++
	h := &discordgo.Webhook{ID: "w" + id, Name: name, Token: "tok"}
	f.hooks[id] = append(f.hooks[id], h)
	return h, nil
}

func (f *fakeOps) WhisperPost(_, _ string, data *discordgo.WebhookParams, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.execErr != nil {
		return nil, f.execErr
	}
	f.posted = append(f.posted, data)
	return f.appendMessage(data.Content), nil
}

// appendMessage adds to the channel's timeline; the caller holds f.mu.
// Every fake post lands in c1, which is the only channel the tests post to.
func (f *fakeOps) appendMessage(content string) *discordgo.Message {
	m := &discordgo.Message{ID: "m" + strconv.Itoa(len(f.messages)+1), Content: content}
	f.messages = append(f.messages, m)
	return m
}

func (f *fakeOps) WhisperEdit(_, _, messageID, content string, _ ...discordgo.RequestOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.messages {
		if m.ID == messageID {
			m.Content = content
			return nil
		}
	}
	return errors.New("unknown message")
}

func (f *fakeOps) ChannelMessages(_ string, _ int, _, _, _ string, _ ...discordgo.RequestOption) ([]*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.messages) == 0 {
		return nil, nil
	}
	return []*discordgo.Message{f.messages[len(f.messages)-1]}, nil
}

// contents is the channel as a reader sees it, oldest first.
func (f *fakeOps) contents() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, m := range f.messages {
		out = append(out, m.Content)
	}
	return out
}

func (f *fakeOps) remove(messageID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = slices.DeleteFunc(f.messages, func(m *discordgo.Message) bool { return m.ID == messageID })
}

type fakeScreener struct {
	refusal string
	err     error
	calls   int
}

func (f *fakeScreener) Screen(_ context.Context, _, _, _, _ string) (string, error) {
	f.calls++
	return f.refusal, f.err
}

type fakeAudit struct {
	mu      sync.Mutex
	entries []string // action + " " + newValue
}

func (f *fakeAudit) Record(_ context.Context, _, _, action, _, newValue string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, action+" "+newValue)
	return nil
}

var testNow = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

func testPlugin(ops *fakeOps, screener Screener) (*Plugin, *fakeAudit, *time.Time) {
	now := testNow
	audit := &fakeAudit{}
	p := New(screener, func(string) DiscordOps { return ops })
	p.audit = audit
	p.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	p.now = func() time.Time { return now }
	return p, audit, &now
}

func member(nick string) *discordgo.Member {
	return &discordgo.Member{
		Nick:        nick,
		User:        &discordgo.User{ID: "u1", Username: "realname", GlobalName: "Global"},
		Permissions: discordgo.PermissionSendMessages | discordgo.PermissionViewChannel,
	}
}

// --- the free suite ---

func TestCheckRefusesWhatItShould(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{"", "nothing to post"},
		{strings.Repeat("a", maxLen+1), "over 400"},
		{"two\nlines", "one line"},
		{"# big words", "heading"},
		{"-# whispered through merlin by @mod", "heading"},
		{"see https://example.com/x", "links"},
		{"see www.example.com", "links"},
		{"join discord.gg/abc", "links"},
		{"join evil.xyz", "links"},
		{"any host.example at all", "links"},
		{"or node.js", "links"},
		{"steam://run/440", "links"},
		{"or 10.0.0.1:8080", "links"},
		{"[masked](example.org)", "links"},
		{"hi @everyone", "mentions"},
		{"hi <@123456>", "mentions"},
		{"hi <@&123456>", "mentions"},
		{"mail me a@b.example", "email"},
		{"call 555-123-4567 now", "phone"},
		{"card 4111 1111 1111 1111", "phone"},
		{"kys", "hurt themselves"},
		{"just kill urself", "hurt themselves"},
		{"go drink bleach", "hurt themselves"},
		{"send cp", "child safety"},
		{"loli pics", "child safety"},
		{"a\u200Bb", "invisible"},
		{"a\u202Eb", "invisible"},
		{"z\u0301\u0301\u0301", "combining"},
	} {
		got := check(tc.text)
		if !strings.Contains(got, tc.want) {
			t.Errorf("check(%q) = %q, want a refusal containing %q", tc.text, got, tc.want)
		}
	}
}

func TestCheckPassesOrdinaryChat(t *testing.T) {
	for _, text := range []string{
		"hello there",
		"that was in 2024, cost 1500 and took 3.5 hours",
		"e.g. this, i.e. that",
		"ok #1 fan here",
		"cafe\u0301 is fine",
		"the year 20240101 was not a year",
		"my kids are loud",
		// Server emotes carry a snowflake id and must not read as a card.
		"<:pepe:123456789012345678> nice <a:dance:987654321098765432>",
		"a.b then c.d",
	} {
		if got := check(text); got != "" {
			t.Errorf("check(%q) = %q, want clean", text, got)
		}
	}
}

// --- the limiter ---

func TestLimiterGapAndHourly(t *testing.T) {
	l := newLimiter()
	now := testNow
	if !l.allow("k", now, time.Hour, 3*time.Second, 3) {
		t.Fatal("first attempt refused")
	}
	if l.allow("k", now.Add(time.Second), time.Hour, 3*time.Second, 3) {
		t.Error("attempt inside the gap allowed")
	}
	if !l.allow("k", now.Add(3*time.Second), time.Hour, 3*time.Second, 3) {
		t.Error("attempt at the gap refused")
	}
	if !l.allow("k", now.Add(6*time.Second), time.Hour, 3*time.Second, 3) {
		t.Error("third attempt refused")
	}
	if l.allow("k", now.Add(9*time.Second), time.Hour, 3*time.Second, 3) {
		t.Error("fourth attempt inside the hour allowed past max 3")
	}
	if !l.allow("k", now.Add(time.Hour+time.Second), time.Hour, 3*time.Second, 3) {
		t.Error("attempt after the window refused: old entries were not pruned")
	}
	if !l.allow("other", now, time.Hour, 3*time.Second, 3) {
		t.Error("a different key shares the window")
	}
}

func TestGuildHourlyScalesWithMembersAndNeverUnderTheFloor(t *testing.T) {
	for members, want := range map[int]int{0: guildFloor, 50: guildFloor, 1800: guildFloor, 2000: 2000, 5000: 5000} {
		if got := guildHourly(members); got != want {
			t.Errorf("guildHourly(%d) = %d, want %d", members, got, want)
		}
	}
}

// One member's cap grows with the guild's unused hour and shrinks back to the
// floor as it fills, and never lets one account take the whole guild cap.
func TestUserHourlyScalesWithGuildHeadroomAndNeverUnderTheFloor(t *testing.T) {
	for remaining, want := range map[int]int{0: userFloor, 200: userFloor, 1800: 900, 5000: 2500} {
		if got := userHourly(remaining); got != want {
			t.Errorf("userHourly(%d) = %d, want %d", remaining, got, want)
		}
	}
	l := newLimiter()
	now := time.Now()
	l.allow("g", now.Add(-2*time.Hour), time.Hour, 0, 10)
	l.allow("g", now, time.Hour, 0, 10)
	if got := l.count("g", now, time.Hour); got != 1 {
		t.Errorf("count = %d, want 1: an attempt outside the window was counted", got)
	}
	if got := l.count("g", now, time.Hour); got != 1 {
		t.Errorf("count = %d after a second count: counting recorded an attempt", got)
	}
}

func TestPostCapsOneChannelUnderDiscordsWebhookRate(t *testing.T) {
	ops := newFakeOps()
	ops.channels["c2"] = &discordgo.Channel{ID: "c2", GuildID: "g1", Type: discordgo.ChannelTypeGuildText}
	p, _, now := testPlugin(ops, &fakeScreener{})
	ctx := context.Background()
	// Distinct members, so only the channel cap is in play.
	whisper := func(i int, channel string) string {
		m := member("")
		m.User.ID = "u" + strconv.Itoa(i)
		r, _ := p.post(ctx, "g1", channel, m, "hi")
		return r
	}
	for i := 0; i < channelMinute; i++ {
		if r := whisper(i, "c1"); r != "" {
			t.Fatalf("whisper %d refused: %q", i, r)
		}
	}
	if r := whisper(channelMinute, "c1"); !strings.Contains(r, "channel") {
		t.Errorf("whisper past the channel cap: %q, want a channel refusal", r)
	}
	if r := whisper(channelMinute+1, "c2"); r != "" {
		t.Errorf("another channel shares the cap: %q", r)
	}
	*now = now.Add(time.Minute)
	if r := whisper(channelMinute+2, "c1"); r != "" {
		t.Errorf("channel cap did not refill after a minute: %q", r)
	}
}

// --- post ---

func TestPostPublishesUnderTheDisplayNameWithTheUsernameMarked(t *testing.T) {
	ops := newFakeOps()
	p, audit, _ := testPlugin(ops, &fakeScreener{})

	refusal, err := p.post(context.Background(), "g1", "c1", member("Nicky"), "  hello there  ")
	if refusal != "" || err != nil {
		t.Fatalf("post: refusal=%q err=%v", refusal, err)
	}
	if len(ops.posted) != 1 {
		t.Fatalf("posted %d messages, want 1", len(ops.posted))
	}
	got := ops.posted[0]
	if got.Username != "Nicky" {
		t.Errorf("Username = %q, want the nick", got.Username)
	}
	if got.Content != "hello there\n-# whispered through merlin by @realname" {
		t.Errorf("Content = %q: the marker must carry the Discord username", got.Content)
	}
	if am := got.AllowedMentions; am == nil || len(am.Parse) != 0 || len(am.Users) != 0 || len(am.Roles) != 0 {
		t.Errorf("AllowedMentions = %+v, want the zero value so nothing can ping", am)
	}
	if len(audit.entries) != 0 {
		t.Errorf("a successful whisper was audited: %v", audit.entries)
	}
}

func TestWebhookUsernameFallsThroughToTheUsername(t *testing.T) {
	if got := webhookUsername(member("")); got != "Global" {
		t.Errorf("no nick: %q, want the global name", got)
	}
	if got := webhookUsername(member("Clyde Jr")); got != "Global" {
		t.Errorf("a nick Discord refuses: %q, want the global name", got)
	}
	m := member("")
	m.User.GlobalName = "discord staff"
	if got := webhookUsername(m); got != "realname" {
		t.Errorf("both names refused by Discord: %q, want the username", got)
	}
}

func TestPostRefusesAndAuditsWithoutTheText(t *testing.T) {
	ops := newFakeOps()
	p, audit, _ := testPlugin(ops, &fakeScreener{})

	refusal, err := p.post(context.Background(), "g1", "c1", member(""), "visit https://evil.example")
	if err != nil || !strings.Contains(refusal, "links") {
		t.Fatalf("post: refusal=%q err=%v, want a link refusal", refusal, err)
	}
	if len(ops.posted) != 0 {
		t.Error("a refused whisper was posted")
	}
	if len(audit.entries) != 1 || !strings.HasPrefix(audit.entries[0], "whisper.refused ") {
		t.Fatalf("audit = %v, want one whisper.refused entry", audit.entries)
	}
	if strings.Contains(audit.entries[0], "evil.example") {
		t.Errorf("audit entry carries the refused text: %q", audit.entries[0])
	}
}

func TestPostHonoursTheScreener(t *testing.T) {
	ops := newFakeOps()
	scr := &fakeScreener{refusal: "hate speech: no"}
	p, _, _ := testPlugin(ops, scr)

	refusal, _ := p.post(context.Background(), "g1", "c1", member(""), "something")
	if refusal != "hate speech: no" || len(ops.posted) != 0 {
		t.Errorf("refusal=%q posted=%d, want the screener's refusal and nothing posted", refusal, len(ops.posted))
	}
	if scr.calls != 1 {
		t.Errorf("screener called %d times, want 1", scr.calls)
	}
}

func TestPostFallsBackWhenTheModelIsDown(t *testing.T) {
	ops := newFakeOps()
	p, _, _ := testPlugin(ops, &fakeScreener{err: errors.New("503")})

	refusal, err := p.post(context.Background(), "g1", "c1", member(""), "something ordinary")
	if refusal != "" || err != nil || len(ops.posted) != 1 {
		t.Errorf("refusal=%q err=%v posted=%d, want posted on the free rungs alone", refusal, err, len(ops.posted))
	}
}

func TestPostRefusesWithNoScreenerWired(t *testing.T) {
	ops := newFakeOps()
	p, _, _ := testPlugin(ops, nil)

	refusal, _ := p.post(context.Background(), "g1", "c1", member(""), "something")
	if refusal == "" || len(ops.posted) != 0 {
		t.Errorf("refusal=%q posted=%d: nothing may be posted unscreened", refusal, len(ops.posted))
	}
}

func TestPostRefusesOutsideOrdinaryTextChannels(t *testing.T) {
	ops := newFakeOps()
	ops.channels["news"] = &discordgo.Channel{ID: "news", GuildID: "g1", Type: discordgo.ChannelTypeGuildNews}
	ops.channels["thread"] = &discordgo.Channel{ID: "thread", GuildID: "g1", Type: discordgo.ChannelTypeGuildPublicThread}
	p, _, now := testPlugin(ops, &fakeScreener{})

	for _, id := range []string{"news", "thread"} {
		*now = now.Add(time.Minute)
		refusal, err := p.post(context.Background(), "g1", id, member(""), "hi")
		if err != nil || refusal == "" {
			t.Errorf("%s: refusal=%q err=%v, want refused", id, refusal, err)
		}
	}
	if len(ops.posted) != 0 {
		t.Errorf("posted %d into channels that must not take whispers", len(ops.posted))
	}
}

// Being able to see the channel is the requirement; Send Messages is not,
// since the people this exists for do not have it.
func TestPostNeedsViewChannelAndNothingMore(t *testing.T) {
	ops := newFakeOps()
	p, audit, now := testPlugin(ops, &fakeScreener{})

	m := member("")
	m.Permissions = discordgo.PermissionViewChannel
	if refusal, err := p.post(context.Background(), "g1", "c1", m, "hi"); refusal != "" || err != nil {
		t.Errorf("view-only member: refusal=%q err=%v, want posted", refusal, err)
	}

	*now = now.Add(userGap)
	m.Permissions = discordgo.PermissionSendMessages
	refusal, err := p.post(context.Background(), "g1", "c1", m, "hi")
	if err != nil || !strings.Contains(refusal, "cannot see") {
		t.Errorf("member without view: refusal=%q err=%v, want refused", refusal, err)
	}
	if len(ops.posted) != 1 || len(audit.entries) != 1 {
		t.Errorf("posted=%d audited=%d, want 1 and 1", len(ops.posted), len(audit.entries))
	}
}

func TestPostAppliesTheGapAndChargesRefusals(t *testing.T) {
	ops := newFakeOps()
	p, _, now := testPlugin(ops, &fakeScreener{})
	ctx := context.Background()

	if r, _ := p.post(ctx, "g1", "c1", member(""), "one"); r != "" {
		t.Fatalf("first whisper refused: %q", r)
	}
	if r, _ := p.post(ctx, "g1", "c1", member(""), "two"); !strings.Contains(r, "slow down") {
		t.Errorf("second whisper inside the gap: %q, want slow down", r)
	}
	*now = now.Add(userGap)
	if r, _ := p.post(ctx, "g1", "c1", member(""), "@everyone"); !strings.Contains(r, "mentions") {
		t.Fatalf("expected a mention refusal, got %q", r)
	}
	// The refusal above consumed the slot, so this one is inside the gap.
	if r, _ := p.post(ctx, "g1", "c1", member(""), "three"); !strings.Contains(r, "slow down") {
		t.Errorf("a refused whisper was a free retry: %q", r)
	}
}

func TestPostReusesOneWebhookPerChannel(t *testing.T) {
	ops := newFakeOps()
	// A webhook this application did not create carries no token and must
	// not be picked up.
	ops.hooks["c1"] = []*discordgo.Webhook{{ID: "foreign", Name: webhookName}}
	p, _, now := testPlugin(ops, &fakeScreener{})
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		*now = now.Add(userGap)
		if r, err := p.post(ctx, "g1", "c1", member(""), "again"); r != "" || err != nil {
			t.Fatalf("post %d: refusal=%q err=%v", i, r, err)
		}
	}
	if ops.created != 1 {
		t.Errorf("created %d webhooks across three whispers, want 1", ops.created)
	}
}

func TestPostReportsPauseAndForgetsABrokenWebhook(t *testing.T) {
	ops := newFakeOps()
	ops.execErr = discordguard.ErrPaused
	p, _, now := testPlugin(ops, &fakeScreener{})

	_, err := p.post(context.Background(), "g1", "c1", member(""), "hi")
	if !discordguard.Skipped(err) {
		t.Errorf("err = %v, want the guard's pause surfaced as-is", err)
	}
	if _, cached := p.webhooks["c1"]; cached {
		t.Error("webhook still cached after a failed execute")
	}
	ops.execErr = nil
	*now = now.Add(userGap)
	if r, err := p.post(context.Background(), "g1", "c1", member(""), "hi"); r != "" || err != nil {
		t.Errorf("post after recovery: refusal=%q err=%v", r, err)
	}
}

// The command is one public leaf with a deniable action, and it finalizes.
func TestInitRegistersOnePublicLeafWithAnAction(t *testing.T) {
	router := core.NewCommandRouter(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p, _, _ := testPlugin(newFakeOps(), &fakeScreener{})
	if err := p.Init(core.Deps{Commands: router}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := router.Finalize(); err != nil {
		t.Fatalf("the command tree does not finalize, so the bot would not start: %v", err)
	}
	if got := router.Actions(); len(got) != 1 || got[0] != "whisper.say" {
		t.Errorf("actions = %v, want [whisper.say] so a guild can deny it per member", got)
	}
	if got := router.Plugins(); len(got) != 1 || got[0] != "whisper" {
		t.Errorf("plugins = %v, want [whisper]", got)
	}
}

// --- runs of whispers ---

func TestRunWearsOneMarkerAtItsEnd(t *testing.T) {
	ops := newFakeOps()
	p, _, now := testPlugin(ops, &fakeScreener{})
	ctx := context.Background()
	mark := marker("realname")
	for _, text := range []string{"one", "two", "three"} {
		if r, err := p.post(ctx, "g1", "c1", member(""), text); r != "" || err != nil {
			t.Fatalf("post %q: refusal=%q err=%v", text, r, err)
		}
		*now = now.Add(userGap)
	}
	want := []string{"one", "two", "three" + mark}
	if got := ops.contents(); !slices.Equal(got, want) {
		t.Errorf("channel = %q, want %q", got, want)
	}

	// Somebody else talks in between: the next whisper stands alone.
	ops.mu.Lock()
	ops.appendMessage("a real message")
	ops.mu.Unlock()
	if r, err := p.post(ctx, "g1", "c1", member(""), "four"); r != "" || err != nil {
		t.Fatalf("post four: refusal=%q err=%v", r, err)
	}
	want = append(want, "a real message", "four"+mark)
	if got := ops.contents(); !slices.Equal(got, want) {
		t.Errorf("after an interruption, channel = %q, want %q", got, want)
	}

	// Past Discord's own grouping window a new header is drawn, so the run
	// ends there too.
	*now = now.Add(blockGap + time.Second)
	if r, err := p.post(ctx, "g1", "c1", member(""), "five"); r != "" || err != nil {
		t.Fatalf("post five: refusal=%q err=%v", r, err)
	}
	want = append(want, "five"+mark)
	if got := ops.contents(); !slices.Equal(got, want) {
		t.Errorf("after the gap, channel = %q, want %q", got, want)
	}

	// A different member's whisper is its own run.
	*now = now.Add(userGap)
	other := member("")
	other.User.ID, other.User.Username = "u2", "someone"
	if r, err := p.post(ctx, "g1", "c1", other, "six"); r != "" || err != nil {
		t.Fatalf("post six: refusal=%q err=%v", r, err)
	}
	want = append(want, "six"+marker("someone"))
	if got := ops.contents(); !slices.Equal(got, want) {
		t.Errorf("another member, channel = %q, want %q", got, want)
	}
}

func TestDeletingTheEndOfARunHandsTheMarkerBack(t *testing.T) {
	ops := newFakeOps()
	p, _, now := testPlugin(ops, &fakeScreener{})
	ctx := context.Background()
	mark := marker("realname")
	for _, text := range []string{"one", "two", "three"} {
		if r, err := p.post(ctx, "g1", "c1", member(""), text); r != "" || err != nil {
			t.Fatalf("post %q: refusal=%q err=%v", text, r, err)
		}
		*now = now.Add(userGap)
	}
	// A middle line going changes nothing about who wears the marker.
	ops.remove("m2")
	p.HandleMessageDelete("c1", "m2")
	if got, want := ops.contents(), []string{"one", "three" + mark}; !slices.Equal(got, want) {
		t.Errorf("after a middle deletion, channel = %q, want %q", got, want)
	}
	// The end going would leave the run unmarked, so the marker moves back.
	ops.remove("m3")
	p.HandleMessageDelete("c1", "m3")
	if got, want := ops.contents(), []string{"one" + mark}; !slices.Equal(got, want) {
		t.Errorf("after the tail was deleted, channel = %q, want %q", got, want)
	}
	// And the next whisper continues under it as before.
	if r, err := p.post(ctx, "g1", "c1", member(""), "four"); r != "" || err != nil {
		t.Fatalf("post four: refusal=%q err=%v", r, err)
	}
	if got, want := ops.contents(), []string{"one", "four" + mark}; !slices.Equal(got, want) {
		t.Errorf("channel = %q, want %q", got, want)
	}
	// Unknown messages and channels are ignored, not a panic.
	p.HandleMessageDelete("c1", "nope")
	p.HandleMessageDelete("c9", "m1")
}

package activity

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

var windowStart = time.Date(2026, 9, 1, 14, 0, 0, 0, time.UTC)

// fakeSource is the whole Discord side of the scan, so the paging, the window
// bound and the exclusions can be exercised without a network.
type fakeSource struct {
	channels    []*discordgo.Channel
	threads     []*discordgo.Channel
	msgs        map[string][]*discordgo.Message // newest first, as Discord returns them
	unreadable  map[string]bool
	channelsErr error

	mu    sync.Mutex
	calls int
}

func (f *fakeSource) GuildChannels(string, ...discordgo.RequestOption) ([]*discordgo.Channel, error) {
	return f.channels, f.channelsErr
}

func (f *fakeSource) ThreadsActive(string, ...discordgo.RequestOption) (*discordgo.ThreadsList, error) {
	return &discordgo.ThreadsList{Threads: f.threads}, nil
}

func (f *fakeSource) ChannelMessages(channelID string, limit int, beforeID, _, _ string, _ ...discordgo.RequestOption) ([]*discordgo.Message, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.unreadable[channelID] {
		return nil, &discordgo.RESTError{Message: &discordgo.APIErrorMessage{Code: 50001, Message: "Missing Access"}}
	}
	before, _ := strconv.ParseInt(beforeID, 10, 64)
	var out []*discordgo.Message
	for _, m := range f.msgs[channelID] {
		id, _ := strconv.ParseInt(m.ID, 10, 64)
		if id >= before {
			continue
		}
		out = append(out, m)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func textChannel(id, name string) *discordgo.Channel {
	return &discordgo.Channel{ID: id, Name: name, Type: discordgo.ChannelTypeGuildText}
}

// msgAt mints a message with a real snowflake for at, so the window bound is
// tested against the same arithmetic the scan uses.
func msgAt(at time.Time, seq int, author, name string) *discordgo.Message {
	return &discordgo.Message{
		ID:        strconv.FormatInt(snowflake(at)+int64(seq), 10),
		Author:    &discordgo.User{ID: author, Username: name},
		Timestamp: at,
	}
}

// TestSnowflakeBoundsTheWindow is the arithmetic the whole report rests on:
// get the epoch or the shift wrong and every window silently scans the wrong
// span, with a perfectly plausible looking list to show for it. discordgo's
// own reverse function is the check.
func TestSnowflakeBoundsTheWindow(t *testing.T) {
	got, err := discordgo.SnowflakeTimestamp(strconv.FormatInt(snowflake(windowStart), 10))
	if err != nil {
		t.Fatal(err)
	}
	if !got.UTC().Equal(windowStart) {
		t.Fatalf("snowflake round trip: want %s, got %s", windowStart, got.UTC())
	}
	if snowflake(windowStart) >= snowflake(windowStart.Add(time.Minute)) {
		t.Fatal("snowflakes have to increase with time or paging never terminates")
	}
}

func TestParseWhen(t *testing.T) {
	for _, in := range []string{"2026-09-01T14:00:00Z", "2026-09-01 14:00:00", "2026-09-01 14:00", " 2026-09-01 14:00 "} {
		got, err := parseWhen(in)
		if err != nil || !got.Equal(windowStart) {
			t.Fatalf("parseWhen(%q) = %v, %v", in, got, err)
		}
	}
	if got, err := parseWhen("2026-09-01"); err != nil || !got.Equal(windowStart.Add(-14*time.Hour)) {
		t.Fatalf("bare date: %v, %v", got, err)
	}
	if _, err := parseWhen("last tuesday"); err == nil {
		t.Fatal("expected a refusal on an unparseable date")
	}
}

// TestScanCountsAndExcludes walks two channels and a thread, and checks the
// three things a wrong answer here would look plausible about: the window
// bound, who is excluded, and a channel the bot cannot read being reported
// rather than quietly dropped.
func TestScanCountsAndExcludes(t *testing.T) {
	end := windowStart.Add(4 * time.Hour)
	inside := windowStart.Add(time.Hour)
	src := &fakeSource{
		channels: []*discordgo.Channel{
			textChannel("c1", "general"), textChannel("c2", "media"),
			textChannel("c3", "locked"), {ID: "c4", Name: "voice", Type: discordgo.ChannelTypeGuildVoice},
		},
		threads:    []*discordgo.Channel{{ID: "t1", Name: "thread", Type: discordgo.ChannelTypeGuildPublicThread}},
		unreadable: map[string]bool{"c3": true},
		msgs: map[string][]*discordgo.Message{
			"c1": {
				msgAt(inside, 3, "u1", "zoe"),
				msgAt(inside, 2, "u2", "abe"),
				func() *discordgo.Message { m := msgAt(inside, 1, "u3", "beep"); m.Author.Bot = true; return m }(),
				func() *discordgo.Message { m := msgAt(inside, 0, "u4", "hook"); m.WebhookID = "w1"; return m }(),
				msgAt(windowStart.Add(-time.Hour), 0, "u5", "before"), // outside the window
			},
			"c2": {msgAt(inside, 5, "u1", "zoe"), msgAt(inside, 4, "u1", "zoe")},
			"t1": {msgAt(inside, 6, "u2", "abe")},
		},
	}

	rep, err := scan(context.Background(), src, "g1", "", windowStart, end)
	if err != nil {
		t.Fatal(err)
	}
	if rep.messages != 5 {
		t.Fatalf("want 5 counted messages, got %d", rep.messages)
	}
	if rep.busy != 3 || rep.looked != 3 || rep.skipped != 1 {
		t.Fatalf("channel counts: busy %d looked %d skipped %d", rep.busy, rep.looked, rep.skipped)
	}
	if rep.truncated() {
		t.Fatal("a scan that finished must not report itself as stopped early")
	}
	if len(rep.people) != 2 {
		t.Fatalf("want 2 people, got %d", len(rep.people))
	}
	// zoe leads on count; both of her channels survived the merge across
	// workers, which is the part concurrency could silently lose.
	if rep.people[0].name != "zoe" || rep.people[0].count != 3 || len(rep.people[0].channels) != 2 {
		t.Fatalf("top person: %+v", rep.people[0])
	}
}

// TestScanOneChannelOnly: the channel option narrows the walk rather than
// filtering the result afterwards, so an unrelated channel is never read.
func TestScanOneChannelOnly(t *testing.T) {
	inside := windowStart.Add(time.Hour)
	src := &fakeSource{
		channels: []*discordgo.Channel{textChannel("c1", "general"), textChannel("c2", "media")},
		msgs: map[string][]*discordgo.Message{
			"c1": {msgAt(inside, 1, "u1", "zoe")},
			"c2": {msgAt(inside, 2, "u2", "abe")},
		},
	}
	rep, err := scan(context.Background(), src, "g1", "c2", windowStart, windowStart.Add(4*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.people) != 1 || rep.people[0].name != "abe" || rep.looked != 1 {
		t.Fatalf("scoped scan read the wrong rooms: %+v", rep)
	}
}

// TestScanPagesUntilTheWindowEnds: more messages than one page, all inside
// the window, must not stop at the first hundred.
func TestScanPages(t *testing.T) {
	inside := windowStart.Add(time.Hour)
	var msgs []*discordgo.Message
	for n := range 250 {
		msgs = append(msgs, msgAt(inside, 250-n, "u1", "zoe"))
	}
	src := &fakeSource{
		channels: []*discordgo.Channel{textChannel("c1", "general")},
		msgs:     map[string][]*discordgo.Message{"c1": msgs},
	}
	rep, err := scan(context.Background(), src, "g1", "", windowStart, windowStart.Add(4*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if rep.messages != 250 {
		t.Fatalf("want all 250 messages, got %d", rep.messages)
	}
	if src.calls < 3 {
		t.Fatalf("want at least 3 pages, got %d calls", src.calls)
	}
}

// TestScanReportsAnUnreadableGuild: listing channels failing is the one error
// that has no partial answer, so it comes back as an error rather than an
// empty report that reads as a quiet server.
func TestScanReportsAnUnreadableGuild(t *testing.T) {
	src := &fakeSource{channelsErr: context.DeadlineExceeded}
	if _, err := scan(context.Background(), src, "g1", "", windowStart, windowStart.Add(time.Hour)); err == nil {
		t.Fatal("expected an error when the channel list cannot be read")
	}
}

// TestScanIgnoresTheHandlerDeadline is the regression test for a report that
// came back "stopped early" on an ordinary window.
//
// core.CommandRouter gives every handler a 30 second context, which is right
// for a command making a REST call or two and far too short for one walking a
// guild's history, and the scan inherited it. Every scan longer than half a
// minute therefore announced itself as a floor. The scan runs detached with
// its own deadline now, so a parent that is already dead changes nothing.
func TestScanIgnoresTheHandlerDeadline(t *testing.T) {
	inside := windowStart.Add(time.Hour)
	src := &fakeSource{
		channels: []*discordgo.Channel{textChannel("c1", "general")},
		msgs:     map[string][]*discordgo.Message{"c1": {msgAt(inside, 1, "u1", "zoe")}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep, err := scan(ctx, src, "g1", "", windowStart, windowStart.Add(4*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if rep.truncated() {
		t.Fatal("the handler's own deadline must not cut the scan short")
	}
	if rep.messages != 1 {
		t.Fatalf("want the one message, got %d", rep.messages)
	}
}

// TestScanNamesTheBoundThatStoppedIt: "stopped early" on its own leaves the
// reader choosing between narrowing the window and simply running it again,
// which are opposite reactions, so each bound has to name itself.
func TestScanNamesTheBoundThatStoppedIt(t *testing.T) {
	inside := windowStart.Add(time.Hour)
	var msgs []*discordgo.Message
	for n := range 250 {
		msgs = append(msgs, msgAt(inside, 250-n, "u1", "zoe"))
	}
	src := func() *fakeSource {
		return &fakeSource{
			channels: []*discordgo.Channel{textChannel("c1", "general")},
			msgs:     map[string][]*discordgo.Message{"c1": msgs},
		}
	}

	pageCeiling, budget := maxPages, scanBudget
	t.Cleanup(func() { maxPages, scanBudget = pageCeiling, budget })

	maxPages = 1
	rep, err := scan(context.Background(), src(), "g1", "", windowStart, windowStart.Add(4*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if rep.stoppedBy != stopPages {
		t.Fatalf("want the page ceiling reported, got %v", rep.stoppedBy)
	}
	if md := markdown(rep, "birdland", windowStart, windowStart.Add(time.Hour), 0); !strings.Contains(md, "ceiling of 1 pages") {
		t.Fatalf("the ceiling has to name itself:\n%s", md)
	}

	// Negative rather than tiny: a deadline already in the past cancels at
	// construction, where a nanosecond races the timer goroutine.
	maxPages, scanBudget = pageCeiling, -time.Second
	if rep, err = scan(context.Background(), src(), "g1", "", windowStart, windowStart.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if rep.stoppedBy != stopTime {
		t.Fatalf("want the clock reported, got %v", rep.stoppedBy)
	}
	if md := markdown(rep, "birdland", windowStart, windowStart.Add(time.Hour), 0); !strings.Contains(md, "ran out of time") {
		t.Fatalf("the clock has to name itself:\n%s", md)
	}
}

func TestTallyAndRank(t *testing.T) {
	people := map[string]*person{}
	at := windowStart
	tally(people, msgAt(at, 1, "1", "zoe"), "general")
	tally(people, msgAt(at.Add(time.Minute), 2, "1", "zoe"), "media")
	tally(people, msgAt(at, 3, "2", "abe"), "general")
	tally(people, msgAt(at, 4, "2", "abe"), "general")

	ranked := rank(people)
	if len(ranked) != 2 {
		t.Fatalf("want 2 people, got %d", len(ranked))
	}
	// Equal counts break on name, so two runs over one window agree.
	if ranked[0].name != "abe" || ranked[1].name != "zoe" {
		t.Fatalf("tie broken wrong: %s then %s", ranked[0].name, ranked[1].name)
	}
	if got := channelList(people["1"].channels); got != "#general #media" {
		t.Fatalf("channel list: %q", got)
	}
	if !people["1"].last.Equal(at.Add(time.Minute)) {
		t.Fatalf("last seen should be the newest message: %s", people["1"].last)
	}
}

func TestTallyPrefersTheGlobalName(t *testing.T) {
	people := map[string]*person{}
	m := msgAt(windowStart, 1, "1", "zoe_underscore")
	m.Author.GlobalName = "Zoe"
	tally(people, m, "general")
	if people["1"].name != "Zoe" {
		t.Fatalf("want the display name, got %q", people["1"].name)
	}
}

func TestMergePeople(t *testing.T) {
	dst := map[string]*person{"1": {id: "1", name: "zoe", count: 2, channels: map[string]bool{"general": true}, last: windowStart}}
	src := map[string]*person{
		"1": {id: "1", name: "zoe", count: 3, channels: map[string]bool{"media": true}, last: windowStart.Add(time.Hour)},
		"2": {id: "2", name: "abe", count: 1, channels: map[string]bool{"art": true}},
	}
	mergePeople(dst, src)
	if dst["1"].count != 5 || len(dst["1"].channels) != 2 || !dst["1"].last.Equal(windowStart.Add(time.Hour)) {
		t.Fatalf("merge lost something: %+v", dst["1"])
	}
	if dst["2"] == nil {
		t.Fatal("merge dropped a person only one worker saw")
	}
}

func TestChannelListCaps(t *testing.T) {
	set := map[string]bool{"a": true, "b": true, "c": true, "d": true, "e": true}
	if got := channelList(set); got != "#a #b #c +2" {
		t.Fatalf("channel list: %q", got)
	}
}

func TestEscapeDefusesADisplayName(t *testing.T) {
	if got := escape("**zoe**_#"); got != "\\*\\*zoe\\*\\*\\_\\#" {
		t.Fatalf("escape: %q", got)
	}
}

func TestHumanSpan(t *testing.T) {
	for in, want := range map[time.Duration]string{
		30 * time.Second:   "a minute",
		45 * time.Minute:   "45 minutes",
		4 * time.Hour:      "4 hours",
		72 * time.Hour:     "3 days",
		90 * time.Minute:   "90 minutes",
		2 * 24 * time.Hour: "2 days",
	} {
		if got := humanSpan(in); got != want {
			t.Fatalf("humanSpan(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestMarkdownShape(t *testing.T) {
	rep := report{
		people: []*person{
			{id: "1", name: "zoe", count: 42, channels: map[string]bool{"general": true, "media": true}},
			{id: "2", name: "abe", count: 7, channels: map[string]bool{"general": true}},
		},
		messages: 49, busy: 2, looked: 5, skipped: 1,
	}
	md := markdown(rep, "birdland", windowStart, windowStart.Add(4*time.Hour), 0)

	for _, want := range []string{
		"## who was active in birdland",
		"`2026-09-01 14:00` to `2026-09-01 18:00` utc, over `4 hours`",
		"`2` people, `49` messages, `2` of `6` channels",
		"`1` channel could not be read",
		"` 1.` **zoe** `42` in #general #media",
		"` 2.` **abe** `7` in #general",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown missing %q:\n%s", want, md)
		}
	}

	// Kept for eyeballing, the same hook TestRenderPNG has: markdown is read
	// rendered, not as source, so the only real check is pasting it.
	if dir := os.Getenv("ACTIVITY_SAMPLE_DIR"); dir != "" {
		if err := os.WriteFile(filepath.Join(dir, listAttachmentName), []byte(md), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// A capped list has to say what it is not showing, and point at the file
	// that does, or the report silently drops the tail of the thing it is for.
	capped := markdown(rep, "birdland", windowStart, windowStart.Add(time.Hour), 1)
	if strings.Contains(capped, "**abe**") {
		t.Fatal("the cap did not apply")
	}
	if !strings.Contains(capped, "showing the top `1` of `2`") || !strings.Contains(capped, listAttachmentName) {
		t.Fatalf("capped list does not say what is missing:\n%s", capped)
	}
}

func TestMarkdownEmptyAndTruncated(t *testing.T) {
	md := markdown(report{looked: 3, stoppedBy: stopPages}, "birdland", windowStart, windowStart.Add(time.Hour), 0)
	if !strings.Contains(md, "nobody chatted in that window.") {
		t.Fatalf("empty report should say so:\n%s", md)
	}
	if !strings.Contains(md, "hit its ceiling") {
		t.Fatalf("a truncated scan has to admit it:\n%s", md)
	}
}

// fakePrivilege stands in for *core.Permissions.
type fakePrivilege struct{ operator string }

func (f fakePrivilege) IsBootstrapAdmin(userID string) bool {
	return f.operator != "" && userID == f.operator
}

// TestOperatorOnly is the gate this plugin exists behind. TierAdmin is the
// floor on the leaf; every one of these is somebody who clears that floor and
// still must not be able to profile the server.
func TestOperatorOnly(t *testing.T) {
	p := New()
	p.privilege = fakePrivilege{operator: "op"}

	if !p.operator("op") {
		t.Fatal("the bootstrap operator has to be allowed")
	}
	for _, who := range []string{"guild-owner", "some-admin", ""} {
		if p.operator(who) {
			t.Fatalf("%q must be refused", who)
		}
	}

	// A missing checker loses the escape hatch rather than granting it to
	// everybody: the one direction this must never fail in.
	open := New()
	if open.operator("op") {
		t.Fatal("a nil privilege checker must refuse, not open up")
	}
}

// TestCommandIsNotListedToTheServer pins the one place this bot sets
// default_member_permissions at all.
//
// Every registered command appears in every member's picker regardless of who
// may run it, so leaving this unset publishes the fact that somebody can ask
// merlin who was talking and when, to the people it is about, for a command
// none of them can run. Zero is "nobody without Discord's Administrator bit",
// which sits under the operator check rather than replacing it, so removing
// this line widens nothing and changes only who sees the command exists. It is
// asserted here because that is exactly the kind of line a later reader
// deletes for matching the §4a rule rather than the reasoning behind it.
func TestCommandIsNotListedToTheServer(t *testing.T) {
	cmd := command()
	if cmd.DefaultMemberPermissions == nil {
		t.Fatal("/activity must not be listed to every member of the server")
	}
	if *cmd.DefaultMemberPermissions != 0 {
		t.Fatalf("want 0 (administrators only), got %d", *cmd.DefaultMemberPermissions)
	}
	// The picker is cosmetic; the gate is not. If this ever stops being
	// TierAdmin-floored and operator-checked, the line above is not what
	// should have been relied on.
	if cmd.Name != "activity" || len(cmd.Options) != 5 {
		t.Fatalf("command shape changed: %s with %d options", cmd.Name, len(cmd.Options))
	}
}

func TestParseOptions(t *testing.T) {
	now := windowStart.Add(24 * time.Hour)
	str := func(name, v string) *discordgo.ApplicationCommandInteractionDataOption {
		return &discordgo.ApplicationCommandInteractionDataOption{
			Name: name, Type: discordgo.ApplicationCommandOptionString, Value: v,
		}
	}
	args := map[string]*discordgo.ApplicationCommandInteractionDataOption{
		"from":  str("from", "2026-09-01 14:00"),
		"to":    str("to", "2026-09-01 18:00"),
		"top":   {Name: "top", Type: discordgo.ApplicationCommandOptionInteger, Value: float64(500)},
		"share": {Name: "share", Type: discordgo.ApplicationCommandOptionBoolean, Value: true},
	}
	opts, err := parseOptions(args, now)
	if err != nil {
		t.Fatal(err)
	}
	if !opts.from.Equal(windowStart) || !opts.to.Equal(windowStart.Add(4*time.Hour)) {
		t.Fatalf("window: %s to %s", opts.from, opts.to)
	}
	if opts.top != maxTop {
		t.Fatalf("top should clamp to %d, got %d", maxTop, opts.top)
	}
	if !opts.share {
		t.Fatal("share was not read")
	}

	// to defaults to now rather than to nothing.
	only, err := parseOptions(map[string]*discordgo.ApplicationCommandInteractionDataOption{
		"from": str("from", "2026-09-01 14:00"),
	}, now)
	if err != nil || !only.to.Equal(now) || only.top != defaultTop {
		t.Fatalf("defaults: %+v, %v", only, err)
	}

	for name, bad := range map[string]map[string]*discordgo.ApplicationCommandInteractionDataOption{
		"no start":        {},
		"unparseable":     {"from": str("from", "yesterday")},
		"reversed window": {"from": str("from", "2026-09-01 18:00"), "to": str("to", "2026-09-01 14:00")},
		"unparseable end": {"from": str("from", "2026-09-01 14:00"), "to": str("to", "soon")},
		"in the future":   {"from": str("from", "2027-01-01"), "to": str("to", "2027-02-01")},
	} {
		if _, err := parseOptions(bad, now); err == nil {
			t.Fatalf("%s should have been refused", name)
		}
	}
}

package statistics

import (
	"net/http"
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
		return nil, &discordgo.RESTError{
			Response: &http.Response{StatusCode: http.StatusForbidden, Status: "403 Forbidden"},
			Message:  &discordgo.APIErrorMessage{Code: 50001, Message: "Missing Access"},
		}
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
			{id: "1", name: "zoe", count: 42, voice: 90 * time.Minute, channels: map[string]bool{"general": true, "media": true}},
			{id: "2", name: "abe", count: 7, channels: map[string]bool{"general": true}},
			// Voice only: no channels, no message count, still a person.
			{id: "3", name: "kit", voice: 3 * time.Hour, channels: map[string]bool{}},
		},
		messages: 49, voice: 270 * time.Minute, channels: 2,
		days: []DayStat{{Day: windowStart.Truncate(24 * time.Hour), Messages: 49, VoiceSeconds: 270 * 60}},
	}
	md := markdown(rep, "birdland", windowStart, windowStart.Add(4*time.Hour), 0)

	for _, want := range []string{
		"## who was active in birdland",
		"`2026-09-01 14:00` to `2026-09-01 18:00` utc, over `4 hours`",
		"`3` people, `49` messages, `4.5h` in voice, `2` channels",
		"` 1.` **zoe** " + iconMessages + " `42` " + iconVoice + " `1.5h` in #general #media",
		"` 2.` **abe** " + iconMessages + " `7` in #general\n",
		"` 3.` **kit** " + iconVoice + " `3.0h`\n",
		"## day by day\n`2026-09-01` " + iconMessages + " `49` " + iconVoice + " `4.5h`",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown missing %q:\n%s", want, md)
		}
	}
	// The embed's copy carries the heatmap instead of the listing.
	if strings.Contains(markdown(rep, "birdland", windowStart, windowStart.Add(4*time.Hour), 24), "day by day") {
		t.Fatal("the shown report should not carry the day listing")
	}
	// A text-only window says nothing about voice at all.
	if strings.Contains(markdown(report{people: rep.people[1:2], messages: 7}, "b", windowStart, windowStart.Add(time.Hour), 0), "in voice") {
		t.Fatal("no voice, no voice line")
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
	if !strings.Contains(capped, "showing the top `1` of `3`") || !strings.Contains(capped, listAttachmentName) {
		t.Fatalf("capped list does not say what is missing:\n%s", capped)
	}
}

// TestMarkdownEmptyAndPartial: a window that starts before counting began
// has to say so, or a quiet week is indistinguishable from an uncounted one.
func TestMarkdownEmptyAndPartial(t *testing.T) {
	rep := report{from: windowStart, coveredFrom: windowStart.Add(time.Hour)}
	md := markdown(rep, "birdland", windowStart, windowStart.Add(2*time.Hour), 0)
	if !strings.Contains(md, "nobody chatted in that window.") {
		t.Fatalf("empty report should say so:\n%s", md)
	}
	if !strings.Contains(md, "counting began `2026-09-01 15:00`") {
		t.Fatalf("a partly covered window has to admit it:\n%s", md)
	}
	whole := report{from: windowStart, coveredFrom: windowStart}
	if whole.partial() || strings.Contains(markdown(whole, "b", windowStart, windowStart.Add(time.Hour), 0), "counting began") {
		t.Fatal("a fully covered window carries no caveat")
	}
	if !strings.Contains(totalsLine(rep), "counted from 2026-09-01") {
		t.Fatalf("the card's totals line should carry the caveat too: %q", totalsLine(rep))
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
	p := New(newFakeStore(), nil, nil)
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
	open := New(newFakeStore(), nil, nil)
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
		t.Fatal("/statistics must not be listed to every member of the server")
	}
	if *cmd.DefaultMemberPermissions != 0 {
		t.Fatalf("want 0 (administrators only), got %d", *cmd.DefaultMemberPermissions)
	}
	// The picker is cosmetic; the gate is not. If this ever stops being
	// TierAdmin-floored and operator-checked, the line above is not what
	// should have been relied on.
	if cmd.Name != "statistics" || len(cmd.Options) != 6 {
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

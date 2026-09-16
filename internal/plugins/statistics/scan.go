package statistics

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// discordEpoch is the millisecond epoch Discord snowflakes count from. A
// timestamp converts straight into a snowflake, which is what lets a window
// be paged with before/after instead of walking every channel back to its
// first message.
const discordEpoch = 1420070400000

const (
	// pageSize is Discord's own maximum for one ChannelMessages call.
	pageSize = 100
	// requestGap is the backfill's own throttle: one page request per gap,
	// so 40 a second, under Discord's 50 a second global ceiling with room
	// for everything else the bot is doing. Per channel, discordgo already
	// tracks the bucket and sleeps out its reset, which for message history
	// is about a page a second and is what actually bounds a backfill; this
	// is the global half, which discordgo only learns about from a 429
	// (spec.MD §4: self-throttle, do not rely on Discord's).
	requestGap = 25 * time.Millisecond
	// pageRetries is how many times one page is re-asked for after a
	// transient failure (a 5xx, a dropped connection) before the channel is
	// given up on. discordgo retries 429s itself; this is for the rest. A
	// 4xx is not retried: Missing Access does not get better by asking.
	pageRetries = 3
	retryPause  = 2 * time.Second
)

// messageSource is the slice of *discordgo.Session the backfill uses, so it
// can be driven by a fake in tests. The narrow-interface seam every other
// consumer in this codebase uses, rather than depending on the concrete
// session.
type messageSource interface {
	GuildChannels(guildID string, options ...discordgo.RequestOption) ([]*discordgo.Channel, error)
	ThreadsActive(guildID string, options ...discordgo.RequestOption) (*discordgo.ThreadsList, error)
	ChannelMessages(channelID string, limit int, beforeID, afterID, aroundID string, options ...discordgo.RequestOption) ([]*discordgo.Message, error)
}

// person is one member's total over a window, as the renderer wants it.
type person struct {
	id       string
	name     string
	avatar   string // avatar hash, empty for a member on a default avatar
	count    int
	voice    time.Duration
	channels map[string]bool
	last     time.Time
}

// hourlyHeatMax is the longest window that gets the hour-of-day grid;
// anything longer gets the daily one.
const hourlyHeatMax = 7 * 24 * time.Hour

// report is everything a rendered answer needs.
type report struct {
	people   []*person
	messages int
	voice    time.Duration
	channels int // channels that carried at least one message
	// days is the server's own day by day, oldest first, for the heatmap
	// and the full listing. Only days with something in them. For a window
	// of hourlyHeatMax (a week) or shorter it is hour by hour instead, and hourly
	// says so: a grid of one or two day cells says nothing a totals line
	// does not, where twenty four hour cells show when the server is awake.
	days   []DayStat
	hourly bool
	// coveredFrom is the earliest instant the buckets can speak for. A
	// window starting before it is answered from what exists, and says so:
	// silently reporting a quiet server for the days before counting began
	// would be the wrong kind of wrong.
	coveredFrom time.Time
	from        time.Time
}

func (r report) partial() bool { return !r.coveredFrom.IsZero() && r.from.Before(r.coveredFrom) }

// snowflake is the smallest id Discord could have minted at t.
func snowflake(t time.Time) int64 {
	return (t.UnixMilli() - discordEpoch) << 22
}

// parseWhen reads the formats a person actually types. Everything is utc: a
// report whose numbers get compared across people in different places has no
// business guessing a local zone.
func parseWhen(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is not a date i understand. try 2026-09-01, 2026-09-01 14:00 or an rfc3339 timestamp, all utc", s)
}

func readable(ch *discordgo.Channel) bool {
	if ch == nil {
		return false
	}
	switch ch.Type {
	case discordgo.ChannelTypeGuildText, discordgo.ChannelTypeGuildNews,
		discordgo.ChannelTypeGuildPublicThread, discordgo.ChannelTypeGuildPrivateThread,
		discordgo.ChannelTypeGuildNewsThread:
		return true
	}
	return false
}

// errScanStopped is the context ending, as distinct from a channel that
// could not be read.
var errScanStopped = errors.New("scan stopped early")

// page fetches one page, waiting for a request slot first and re-asking
// after a transient failure. The context is checked at every wait, so a
// deadline is felt within one gap rather than one retry pause.
func page(ctx context.Context, src messageSource, channelID, before string, tick <-chan time.Time) ([]*discordgo.Message, error) {
	for attempt := 0; ; attempt++ {
		select {
		case <-ctx.Done():
			return nil, errScanStopped
		case <-tick:
		}
		msgs, err := src.ChannelMessages(channelID, pageSize, before, "", "")
		if err == nil || attempt >= pageRetries || !transient(err) {
			return msgs, err
		}
		select {
		case <-ctx.Done():
			return nil, errScanStopped
		case <-time.After(retryPause * time.Duration(attempt+1)):
		}
	}
}

// transient is an error worth asking again about: anything but a Discord
// 4xx, which is an answer rather than a fault.
func transient(err error) bool {
	var rerr *discordgo.RESTError
	if errors.As(err, &rerr) && rerr.Response != nil {
		return rerr.Response.StatusCode >= 500
	}
	return true
}

// messageID parses a snowflake, or 0 for anything that is not one.
func messageID(s string) int64 {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return id
}

// rank orders by message count, then voice time, then by name so two runs
// over one window produce the same list rather than swapping people on
// every tie.
func rank(people map[string]*person) []*person {
	out := make([]*person, 0, len(people))
	for _, p := range people {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].count != out[j].count {
			return out[i].count > out[j].count
		}
		if out[i].voice != out[j].voice {
			return out[i].voice > out[j].voice
		}
		return out[i].name < out[j].name
	})
	return out
}

// chatters and voicers are the two listings a report carries, since the
// people in voice are mostly not the people typing and one ranking buries
// whichever it is not sorted by. A member doing both appears in both.
func chatters(rep report) []*person {
	var out []*person
	for _, p := range rep.people {
		if p.count > 0 {
			out = append(out, p)
		}
	}
	return out
}

func voicers(rep report) []*person {
	var out []*person
	for _, p := range rep.people {
		if p.voice > 0 {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].voice != out[j].voice {
			return out[i].voice > out[j].voice
		}
		return out[i].name < out[j].name
	})
	return out
}

// capped is the first limit of a list, or all of it for limit 0.
func capped(people []*person, limit int) []*person {
	if limit > 0 && len(people) > limit {
		return people[:limit]
	}
	return people
}

// Icons for the two kinds of activity, the same in the markdown and on the
// card so a reader learns them once.
const (
	iconMessages = "\U0001F4AC"       // speech balloon
	iconVoice    = "\U0001F399\uFE0F" // studio microphone
)

// stats renders a member's or a day's two counts, leaving out whichever
// is zero: "🎙️ 0.0h" on somebody who never joined voice is noise on every
// row of a text-only server. voiceFirst leads with the microphone, for the
// voice listing, where that is the number the row is ranked by.
func stats(messages int, voice time.Duration, voiceFirst bool) string {
	var parts []string
	if messages > 0 {
		parts = append(parts, fmt.Sprintf("%s `%d`", iconMessages, messages))
	}
	if voice > 0 {
		parts = append(parts, fmt.Sprintf("%s `%s`", iconVoice, hours(voice)))
	}
	if voiceFirst && len(parts) == 2 {
		parts[0], parts[1] = parts[1], parts[0]
	}
	return strings.Join(parts, " ")
}

// hours renders voice time to a tenth of an hour, the unit the report
// promises. Anything under six minutes still shows as something rather
// than rounding to "0.0h" and reading as nothing.
func hours(d time.Duration) string {
	if d > 0 && d < 6*time.Minute {
		return "<0.1h"
	}
	return fmt.Sprintf("%.1fh", d.Hours())
}

// markdown renders the report for Discord. limit 0 means everyone, which is
// what the attached .md file gets.
func markdown(rep report, guild string, start, end time.Time, limit int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## who was active in %s\n", guild)
	fmt.Fprintf(&b, "`%s` to `%s` utc, over `%s`\n",
		start.Format("2006-01-02 15:04"), end.Format("2006-01-02 15:04"), humanSpan(end.Sub(start)))
	fmt.Fprintf(&b, "`%d` people, `%d` messages, ", len(rep.people), rep.messages)
	if rep.voice > 0 {
		fmt.Fprintf(&b, "`%s` in voice, ", hours(rep.voice))
	}
	fmt.Fprintf(&b, "`%d` channels\n", rep.channels)
	if rep.partial() {
		fmt.Fprintf(&b, "-# counting began `%s` utc, so this window is only counted from there. `/statistics backfill` fills in what came before\n",
			rep.coveredFrom.Format("2006-01-02 15:04"))
	}
	b.WriteString("\n")

	if len(rep.people) == 0 {
		b.WriteString("nobody chatted in that window.\n")
		return b.String()
	}

	chat, voice := chatters(rep), voicers(rep)
	shown := capped(chat, limit)
	for i, p := range shown {
		fmt.Fprintf(&b, "`%2d.` **%s** %s", i+1, escape(p.name), stats(p.count, p.voice, false))
		if len(p.channels) > 0 {
			fmt.Fprintf(&b, " in %s", channelList(p.channels))
		}
		b.WriteString("\n")
	}
	if len(shown) < len(chat) {
		fmt.Fprintf(&b, "\nshowing the top `%d` of `%d`, the rest is in %s\n", len(shown), len(chat), listAttachmentName)
	}
	if len(voice) > 0 {
		b.WriteString("\n## in voice\n")
		shown := capped(voice, limit)
		for i, p := range shown {
			fmt.Fprintf(&b, "`%2d.` **%s** %s\n", i+1, escape(p.name), stats(p.count, p.voice, true))
		}
		if len(shown) < len(voice) {
			fmt.Fprintf(&b, "\nshowing the top `%d` of `%d`, the rest is in %s\n", len(shown), len(voice), listAttachmentName)
		}
	}
	// The day by day rides only in the full file: the embed carries the
	// same thing as the heatmap, and a listing under it would be the one
	// section nobody scrolls to.
	if limit == 0 && len(rep.days) > 0 {
		heading, layout := "day by day", "2006-01-02"
		if rep.hourly {
			heading, layout = "hour by hour", "2006-01-02 15:04"
		}
		fmt.Fprintf(&b, "\n## %s\n", heading)
		for _, d := range rep.days {
			fmt.Fprintf(&b, "`%s` %s\n", d.Day.Format(layout), stats(d.Messages, time.Duration(d.VoiceSeconds)*time.Second, false))
		}
	}
	return b.String()
}

// channelList names up to three channels so a row stays one line.
func channelList(set map[string]bool) string {
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, "#"+n)
	}
	sort.Strings(names)
	if len(names) > 3 {
		return strings.Join(names[:3], " ") + fmt.Sprintf(" +%d", len(names)-3)
	}
	return strings.Join(names, " ")
}

// escape defuses the markdown in a display name. A member picks their own,
// and an unescaped `**__` in a report is how a reader misreads a row.
func escape(s string) string {
	return strings.NewReplacer(
		"*", "\\*", "_", "\\_", "`", "\\`", "~", "\\~", "|", "\\|", ">", "\\>", "#", "\\#",
	).Replace(s)
}

// humanSpan is prose, matching the rest of the member-facing durations in
// this bot rather than core.FormatDuration's compact admin form.
func humanSpan(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	case d >= 2*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	case d >= 2*time.Minute:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	default:
		return "a minute"
	}
}

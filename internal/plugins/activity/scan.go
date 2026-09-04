package activity

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	// maxPages bounds the whole scan. A wide window over a busy guild would
	// otherwise walk for longer than the 15 minutes an interaction token
	// lives, and a report that never lands is worse than a partial one that
	// says it is partial.
	maxPages = 600
	// scanBudget is the wall-clock half of the same bound, for the case
	// where the pages are few but slow (rate limits, a struggling API).
	scanBudget = 3 * time.Minute
	// scanWorkers is how many channels are walked at once.
	//
	// Concurrency here is safe rather than cheeky: Discord buckets
	// GET /channels/{id}/messages per channel, discordgo holds a lock per
	// bucket and sleeps out a 429 on its own, and one worker per channel
	// never queues two calls against the same bucket anyway. Six keeps the
	// burst well under the 50 requests a second global ceiling while a wide
	// window over a large guild finishes in a fraction of the time. Each
	// worker tallies into its own map and merges once, so the concurrency
	// costs no lock on the hot path.
	scanWorkers = 6
)

// messageSource is the slice of *discordgo.Session this plugin uses, so the
// scan can be driven by a fake in tests. The narrow-interface seam every
// other consumer in this codebase uses, rather than depending on the
// concrete session.
type messageSource interface {
	GuildChannels(guildID string, options ...discordgo.RequestOption) ([]*discordgo.Channel, error)
	ThreadsActive(guildID string, options ...discordgo.RequestOption) (*discordgo.ThreadsList, error)
	ChannelMessages(channelID string, limit int, beforeID, afterID, aroundID string, options ...discordgo.RequestOption) ([]*discordgo.Message, error)
}

// person is one member's tally over the window.
type person struct {
	id       string
	name     string
	avatar   string // avatar hash, empty for a member on a default avatar
	count    int
	channels map[string]bool
	last     time.Time
}

// report is everything a rendered answer needs. The counts of channels are
// kept apart on purpose: "6 of 31" is how an operator sees that half the
// server was invisible to the bot, which a single number would hide.
type report struct {
	people    []*person
	messages  int
	busy      int  // channels that carried at least one message
	looked    int  // channels the scan could read
	skipped   int  // channels the bot could not read
	truncated bool // the page ceiling or the deadline stopped the walk
}

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

// scan walks every readable channel and active thread in the guild, newest
// first, stopping at the window start. onlyChannel narrows it to one room
// when set.
func scan(ctx context.Context, src messageSource, guildID, onlyChannel string, start, end time.Time) (report, error) {
	channels, err := src.GuildChannels(guildID)
	if err != nil {
		return report{}, fmt.Errorf("could not list this server's channels: %w", err)
	}
	// Threads are where a good half of a busy guild talks, and a missing
	// thread list is not worth failing the whole report over.
	if threads, terr := src.ThreadsActive(guildID); terr == nil && threads != nil {
		channels = append(channels, threads.Threads...)
	}

	deadline, cancel := context.WithTimeout(ctx, scanBudget)
	defer cancel()

	after, before := snowflake(start), strconv.FormatInt(snowflake(end), 10)
	wanted := make([]*discordgo.Channel, 0, len(channels))
	for _, ch := range channels {
		if readable(ch) && (onlyChannel == "" || ch.ID == onlyChannel) {
			wanted = append(wanted, ch)
		}
	}

	var (
		pages atomic.Int64
		mu    sync.Mutex
		wg    sync.WaitGroup
	)
	people := map[string]*person{}
	rep := report{}
	work := make(chan *discordgo.Channel)

	for range min(scanWorkers, max(1, len(wanted))) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := map[string]*person{}
			var seen, busy, looked, skipped int
			stopped := false
			for ch := range work {
				n, err := scanChannel(deadline, src, ch, before, after, local, &pages)
				switch {
				case errors.Is(err, errScanStopped):
					stopped = true
				case err != nil:
					// A channel the bot cannot see is the ordinary case in
					// a real guild, not a reason to abandon the report. It
					// is counted, so the answer can say how much of the
					// server it could not read.
					skipped++
					continue
				}
				looked++
				seen += n
				if n > 0 {
					busy++
				}
			}
			mu.Lock()
			defer mu.Unlock()
			mergePeople(people, local)
			rep.messages += seen
			rep.busy += busy
			rep.looked += looked
			rep.skipped += skipped
			rep.truncated = rep.truncated || stopped
		}()
	}
	for _, ch := range wanted {
		work <- ch
	}
	close(work)
	wg.Wait()

	rep.people = rank(people)
	return rep, nil
}

// mergePeople folds one worker's tally into the shared one. Counts add,
// channels union, and the newest last-seen wins.
func mergePeople(dst, src map[string]*person) {
	for id, s := range src {
		d := dst[id]
		if d == nil {
			dst[id] = s
			continue
		}
		d.count += s.count
		for ch := range s.channels {
			d.channels[ch] = true
		}
		if s.last.After(d.last) {
			d.last = s.last
		}
	}
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

// errScanStopped is the page ceiling or the deadline, as distinct from a
// channel that could not be read. One means the report is a floor and has to
// say so; the other means one room is missing from an otherwise whole answer.
var errScanStopped = errors.New("scan stopped early")

// scanChannel pages one channel backwards from before, stopping at the first
// message older than after. Only before is passed to Discord: the docs say to
// use one of before/after/around per call, and paging on before is the half
// that terminates. It returns the messages counted, plus errScanStopped if it
// ran out of budget rather than out of window.
func scanChannel(ctx context.Context, src messageSource, ch *discordgo.Channel, before string, after int64, people map[string]*person, pages *atomic.Int64) (int, error) {
	seen := 0
	for {
		if ctx.Err() != nil || pages.Add(1) > maxPages {
			return seen, errScanStopped
		}
		msgs, err := src.ChannelMessages(ch.ID, pageSize, before, "", "")
		if err != nil {
			return seen, err
		}
		if len(msgs) == 0 {
			return seen, nil
		}
		for _, m := range msgs {
			id, err := strconv.ParseInt(m.ID, 10, 64)
			if err != nil {
				continue
			}
			if id < after {
				return seen, nil
			}
			if m.Author == nil || m.Author.Bot || m.WebhookID != "" {
				continue
			}
			seen++
			tally(people, m, ch.Name)
		}
		before = msgs[len(msgs)-1].ID
	}
}

func tally(people map[string]*person, m *discordgo.Message, channel string) {
	p := people[m.Author.ID]
	if p == nil {
		name := m.Author.GlobalName
		if name == "" {
			name = m.Author.Username
		}
		p = &person{id: m.Author.ID, name: name, avatar: m.Author.Avatar, channels: map[string]bool{}}
		people[m.Author.ID] = p
	}
	p.count++
	p.channels[channel] = true
	if m.Timestamp.After(p.last) {
		p.last = m.Timestamp.UTC()
	}
}

// rank orders by message count, then by name so two runs over one window
// produce the same list rather than swapping people on every tie.
func rank(people map[string]*person) []*person {
	out := make([]*person, 0, len(people))
	for _, p := range people {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].count != out[j].count {
			return out[i].count > out[j].count
		}
		return out[i].name < out[j].name
	})
	return out
}

// markdown renders the report for Discord. limit 0 means everyone, which is
// what the attached .md file gets.
func markdown(rep report, guild string, start, end time.Time, limit int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## who was active in %s\n", guild)
	fmt.Fprintf(&b, "`%s` to `%s` utc, over `%s`\n",
		start.Format("2006-01-02 15:04"), end.Format("2006-01-02 15:04"), humanSpan(end.Sub(start)))
	fmt.Fprintf(&b, "`%d` people, `%d` messages, `%d` of `%d` channels\n",
		len(rep.people), rep.messages, rep.busy, rep.looked+rep.skipped)
	if rep.skipped > 0 {
		noun, them := "channels", "them"
		if rep.skipped == 1 {
			noun, them = "channel", "it"
		}
		fmt.Fprintf(&b, "-# `%d` %s could not be read, so nothing said in %s is counted\n", rep.skipped, noun, them)
	}
	if rep.truncated {
		b.WriteString("-# the scan hit its ceiling and stopped early, so this is a floor and not the whole window\n")
	}
	b.WriteString("\n")

	if len(rep.people) == 0 {
		b.WriteString("nobody chatted in that window.\n")
		return b.String()
	}

	shown := rep.people
	if limit > 0 && len(shown) > limit {
		shown = shown[:limit]
	}
	for i, p := range shown {
		fmt.Fprintf(&b, "`%2d.` **%s** `%d` in %s\n", i+1, escape(p.name), p.count, channelList(p.channels))
	}
	if len(shown) < len(rep.people) {
		fmt.Fprintf(&b, "\nshowing the top `%d` of `%d`, the rest is in %s\n", len(shown), len(rep.people), listAttachmentName)
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

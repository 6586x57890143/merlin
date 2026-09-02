package contest

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// postingPerms is what a member needs to start a forum post. Denying it is
// how a contest forum is opened and closed, rather than archiving or locking
// the channel: a locked channel also stops people talking in the posts that
// already exist, and the discussion under an entry is half the point of
// running the contest in a forum at all.
const postingPerms = discordgo.PermissionCreatePublicThreads

// ErrForumFull reports that the guild is too close to Discord's 500-channel
// cap to safely create another channel.
var ErrForumFull = fmt.Errorf("contest: this server is too close to Discord's channel limit to add a contest forum")

// ErrNoMessageContent reports that merlin fetched a forum post and got back
// no attachments and no text, which in practice means one thing: the Message
// Content toggle is off in the Developer Portal.
//
// This is the one dependency in the plugin that fails invisibly if it is not
// checked for, because an empty message and a message merlin cannot read
// look identical. So it is checked for, said out loud in the thread, and
// reported by /contest status. Somebody's entry is never quietly dropped.
var ErrNoMessageContent = fmt.Errorf("contest: merlin cannot read message content, so forum posts arrive empty")

// createForum makes the contest's forum channel. Posting starts denied,
// because a contest in its announce phase is a thing people are being told
// about, not a thing they can enter yet.
func (p *Plugin) createForum(ctx context.Context, c Contest, categoryID string) (string, error) {
	ops := p.opsFor(c.GuildID)

	// Self-throttle well clear of Discord's guild channel cap, the same rule
	// rotation applies before creating a replacement channel. The bot must
	// never be the thing that walks a guild into a hard platform limit.
	channels, err := ops.GuildChannels(c.GuildID)
	if err != nil {
		return "", fmt.Errorf("contest: list channels: %w", err)
	}
	if len(channels) >= 500-channelCapHeadroom {
		return "", ErrForumFull
	}

	ch, err := ops.GuildChannelCreateComplex(c.GuildID, discordgo.GuildChannelCreateData{
		Name:     forumName(c.Title),
		Type:     discordgo.ChannelTypeGuildForum,
		Topic:    truncate(strings.TrimSuffix(c.Title+". "+c.Theme, ". "), 1000),
		ParentID: categoryID,
		PermissionOverwrites: []*discordgo.PermissionOverwrite{{
			ID:   c.GuildID, // @everyone's role id is the guild id
			Type: discordgo.PermissionOverwriteTypeRole,
			Deny: postingPerms,
		}},
	})
	if err != nil {
		return "", fmt.Errorf("contest: create forum: %w", err)
	}
	return ch.ID, nil
}

// setForumOpen flips whether members may start new posts.
func (p *Plugin) setForumOpen(ctx context.Context, c Contest, open bool) error {
	if c.ForumChannelID == "" {
		return nil
	}
	var deny int64
	if !open {
		deny = postingPerms
	}
	return p.opsFor(c.GuildID).ChannelPermissionSet(
		c.ForumChannelID, c.GuildID, discordgo.PermissionOverwriteTypeRole, 0, deny)
}

// truncate caps a string at n runes. Discord rejects an over-long channel
// topic outright rather than trimming it, which would fail the whole create
// over a field nobody reads carefully.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "."
}

// forumName turns a contest title into a channel name Discord will accept
// unchanged. Discord lowercases and hyphenates on its own, but doing it here
// means the name merlin logs and the name in the sidebar are the same
// string.
func forumName(title string) string {
	var b strings.Builder
	b.WriteString("contest-")
	last := '-'
	for _, r := range strings.ToLower(title) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			last = r
		default:
			if last != '-' {
				b.WriteRune('-')
				last = '-'
			}
		}
		if b.Len() >= 90 {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

// syncSubmissions re-derives the entry list from the forum's live threads.
//
// This is the same re-derive-from-live-state rule rotation and roles follow,
// and it is why there is no /contest withdraw command: deleting your post is
// how you withdraw, and a command that had to stay in sync with that would
// be a second source of truth. It also means an edited title is picked up,
// and that re-reading a post refreshes its Discord CDN link, which expires
// after about a day.
//
// **Withdrawal only happens while submissions are open.** Once voting
// starts, people are casting votes against a specific list, and letting a
// deleted post retroactively remove an entry would silently discard every
// vote already cast for it: somebody could lose, delete their post, and
// take their voters' ballots with them. So during vote the sync still runs,
// because the CDN links need refreshing and that is the only way to get a
// fresh one, but the entry list itself is frozen.
func (p *Plugin) syncSubmissions(ctx context.Context, c Contest) error {
	if c.ForumChannelID == "" {
		return nil
	}
	threads, err := p.forumThreads(c)
	if err != nil {
		return err
	}

	// One live entry per member. The keeper is the earliest post rather than
	// whichever the list happened to return first: thread IDs are
	// snowflakes, so comparing them is comparing creation time.
	keep := make(map[string]string, len(threads)) // user -> thread
	for _, th := range threads {
		if prev, dup := keep[th.OwnerID]; !dup || th.ID < prev {
			keep[th.OwnerID] = th.ID
		}
	}

	// The cap is the first maxEntries to post, decided over the whole set.
	// It used to be a break inside the dedupe loop testing a slice that was
	// only appended to in the loop after it, so it read 0 >= maxEntries on
	// every pass and bounded nothing at all.
	live := make([]string, 0, len(keep))
	for _, id := range keep {
		live = append(live, id)
	}
	slices.Sort(live)
	if len(live) > maxEntries {
		live = live[:maxEntries]
	}

	// Withdraw before upserting, not after, and from what Discord says
	// exists rather than from what merlin managed to process.
	//
	// Both orderings matter. `live` used to be built out of the threads
	// whose read and write below both succeeded, so a single 429 on one
	// ChannelMessages call withdrew a perfectly live entry: the "only
	// untrack on gone, never on failed" rule (spec.MD §4) pointed at the
	// entry list. And upserting first put the replacement post of a member
	// who had deleted and reposted straight into
	// contest_submissions_one_live_idx, against the old row that this call
	// is about to retire, losing the new entry for a whole tick.
	if c.Phase == PhaseSubmit {
		if err := p.store.WithdrawMissing(ctx, c.ID, live, p.now()); err != nil {
			return err
		}
	}

	wanted := make(map[string]bool, len(live))
	for _, id := range live {
		wanted[id] = true
	}
	for _, th := range threads {
		if !wanted[th.ID] {
			continue
		}
		sub, err := p.readThread(ctx, c, th)
		if err != nil {
			p.log.Error("contest: read thread", "thread", th.ID, "err", err)
			continue
		}
		if err := p.store.UpsertSubmission(ctx, sub); err != nil {
			p.log.Error("contest: record submission", "thread", th.ID, "err", err)
			continue
		}
	}
	return nil
}

// forumThreads is every post under the contest forum, archived ones
// included.
//
// The active list alone is not the entry list. A forum post archives itself
// after its parent's inactivity window, which merlin never sets and so takes
// Discord's default of a few days, and an archived post is still sitting
// there in the forum looking exactly like an entry to the person who wrote
// it. Reading only the active list made a quiet entry indistinguishable from
// a deleted one, so it got withdrawn: dropped from the gallery, the vote and
// the tally, with nothing said to the member.
func (p *Plugin) forumThreads(c Contest) ([]*discordgo.Channel, error) {
	ops := p.opsFor(c.GuildID)
	active, err := ops.GuildThreadsActive(c.GuildID)
	if err != nil {
		return nil, fmt.Errorf("contest: list threads: %w", err)
	}
	out := make([]*discordgo.Channel, 0, len(active.Threads))
	for _, th := range active.Threads {
		if th.ParentID == c.ForumChannelID {
			out = append(out, th)
		}
	}

	// Channel-scoped, so no ParentID filter is needed here. Paged from
	// newest archive time backwards, which is the order Discord returns and
	// the cursor it takes.
	var before *time.Time
	for range maxArchivedPages {
		list, err := ops.ThreadsArchived(c.ForumChannelID, before, archivedPageSize)
		if err != nil {
			return nil, fmt.Errorf("contest: list archived threads: %w", err)
		}
		out = append(out, list.Threads...)
		last := len(list.Threads) - 1
		if !list.HasMore || last < 0 || list.Threads[last].ThreadMetadata == nil {
			break
		}
		ts := list.Threads[last].ThreadMetadata.ArchiveTimestamp
		before = &ts
	}
	return out, nil
}

// readThread turns one forum post into a submission.
//
// The starter message of a forum post shares the thread's own id, so asking
// for one message around that id returns exactly it. This is the only place
// this plugin reads message content, it is one message at a time and on
// demand, and it happens over REST rather than the gateway: contest never
// joins aimod in receiving every message in every guild.
func (p *Plugin) readThread(ctx context.Context, c Contest, th *discordgo.Channel) (Submission, error) {
	ops := p.opsFor(c.GuildID)
	msgs, err := ops.ChannelMessages(th.ID, 1, "", "", th.ID)
	if err != nil {
		return Submission{}, fmt.Errorf("contest: read starter message: %w", err)
	}
	if len(msgs) == 0 {
		return Submission{}, fmt.Errorf("contest: thread %s has no starter message", th.ID)
	}
	msg := msgs[0]

	sub := Submission{
		ID:        newID(),
		ContestID: c.ID,
		UserID:    th.OwnerID,
		ThreadID:  th.ID,
		Title:     th.Name,
		Body:      strings.TrimSpace(msg.Content),
		Kind:      "text",
	}
	if msg.Author != nil {
		sub.UserID = msg.Author.ID
		sub.Author = displayName(msg)
	}
	if len(msg.Attachments) > 0 {
		// Every attachment, not just the first. Somebody posting four
		// drawings had three of them silently dropped, while the forum
		// thread they came from showed all four. Capped because the card
		// has to stay a card: past maxEntryMedia the gallery is a scroll.
		for _, a := range msg.Attachments {
			if len(sub.MediaURLs) >= maxEntryMedia {
				break
			}
			sub.MediaURLs = append(sub.MediaURLs, a.URL)
		}
		first := msg.Attachments[0]
		sub.MediaURL = first.URL
		sub.Kind = kindOf(first.ContentType, first.Filename)
	} else if link := firstLink(sub.Body); link != "" {
		sub.Link = link
		sub.Kind = "link"
	}

	if sub.MediaURL == "" && sub.Body == "" && sub.Link == "" {
		// Genuinely empty and unreadable look the same from here, and the
		// unreadable case is the likely one, so say the actionable thing.
		return sub, ErrNoMessageContent
	}
	return sub, nil
}

// displayName prefers the member's per-server nickname, since that is the
// name everybody in the channel already associates with them.
func displayName(msg *discordgo.Message) string {
	if msg.Member != nil && msg.Member.Nick != "" {
		return msg.Member.Nick
	}
	if msg.Author.GlobalName != "" {
		return msg.Author.GlobalName
	}
	return msg.Author.Username
}

// kindOf decides how the gallery renders an entry. Content type first,
// because Discord sets it and it is right; the extension is the fallback for
// the handful of formats it does not label.
func kindOf(contentType, filename string) string {
	mt, _, _ := strings.Cut(contentType, "/")
	switch mt {
	case "image", "audio", "video":
		return mt
	}
	ext := filename
	if i := strings.LastIndex(filename, "."); i >= 0 {
		ext = strings.ToLower(filename[i+1:])
	}
	switch ext {
	case "png", "jpg", "jpeg", "gif", "webp", "avif":
		return "image"
	case "mp3", "wav", "ogg", "flac", "m4a", "opus":
		return "audio"
	case "mp4", "webm", "mov":
		return "video"
	case "txt", "md":
		return "text"
	}
	return "other"
}

// firstLink pulls a bare URL out of a post body, so an entry that is a
// SoundCloud or YouTube link renders as a link card rather than as a wall of
// text with a URL in it. Deliberately not a parser: anything more than "the
// first http token" is guessing at intent.
func firstLink(body string) string {
	for _, field := range strings.Fields(body) {
		if strings.HasPrefix(field, "https://") || strings.HasPrefix(field, "http://") {
			return strings.TrimRight(field, ".,)>\"'")
		}
	}
	return ""
}

// HandleThreadCreate is wired to the gateway in cmd/bot/main.go. It exists
// only to make a new entry appear within a second instead of within a tick;
// the tick re-derives everything anyway, so this handler failing costs at
// most a minute of latency and never a lost entry.
//
// ThreadCreate carries the thread's id, name and owner, none of which are
// message-content fields, so this arrives with no privileged intent.
func (p *Plugin) HandleThreadCreate(s *discordgo.Session, tc *discordgo.ThreadCreate) {
	if tc.GuildID == "" || tc.ParentID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	c, err := p.store.LiveContest(ctx, tc.GuildID)
	if err != nil || c.ForumChannelID != tc.ParentID {
		return
	}
	if c.Phase != PhaseSubmit {
		return
	}

	sub, err := p.readThread(ctx, c, tc.Channel)
	if err == ErrNoMessageContent {
		p.sayInThread(ctx, c, tc.ID, "i can't see attachments or text on this post, so it isn't counted yet. an admin needs to switch on Message Content in the developer portal.")
		return
	}
	if err != nil {
		p.log.Error("contest: read new thread", "thread", tc.ID, "err", err)
		return
	}

	// A second post by the same member is refused rather than replacing the
	// first: one entry each, and which one counts should not depend on the
	// order two REST calls happened to land in.
	existing, err := p.store.Submissions(ctx, c.ID)
	if err != nil {
		p.log.Error("contest: check existing entries", "contest", c.ID, "err", err)
		return
	}
	for _, e := range existing {
		if e.UserID == sub.UserID && e.ThreadID != sub.ThreadID {
			p.sayInThread(ctx, c, tc.ID, "you already have an entry in this one, so this post doesn't count. delete the other post if you'd rather this be it.")
			return
		}
	}
	if len(existing) >= maxEntries {
		p.sayInThread(ctx, c, tc.ID, "this contest is full, sorry. nothing past "+fmt.Sprint(maxEntries)+" entries fits on the gallery.")
		return
	}

	if err := p.store.UpsertSubmission(ctx, sub); err != nil {
		p.log.Error("contest: record new submission", "thread", tc.ID, "err", err)
		return
	}
	p.pushBestEffort(ctx, c)
}

// sayInThread posts a short plain reply into a forum post. Plain content
// rather than an embed on purpose: these are corrections aimed at one
// person mid-post, and an embed with a thumbnail would read as an
// announcement.
func (p *Plugin) sayInThread(ctx context.Context, c Contest, threadID, msg string) {
	if _, err := p.opsFor(c.GuildID).ChannelMessageSendComplex(threadID, &discordgo.MessageSend{
		Content: msg,
	}); err != nil {
		p.log.Error("contest: reply in thread", "thread", threadID, "err", err)
	}
}

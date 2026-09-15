package statistics

import (
	"context"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// flushEvery is how often the in-memory tallies are written out. discordgo
// dispatches gateway events serially, so the handlers below only touch a
// map under a lock; the database sees one statement per table per flush
// rather than one per message.
const flushEvery = 10 * time.Second

// flushTimeout bounds one flush; a wedged database must not hold the
// counters' lock, since every message handler waits on it.
const flushTimeout = 30 * time.Second

// pendingCap is how many distinct cells a failed flush may carry over
// before the oldest are dropped. A database that stays down should cost
// counts, not memory.
// ponytail: flat cap and drop-all; per-guild caps if one guild ever floods.
const pendingCap = 50000

type bucketKey struct {
	guildID, channelID, userID string
	hour                       time.Time
}

type memberKey struct {
	guildID string
	hour    time.Time
}

// counter is the live tally between flushes.
type counter struct {
	mu       sync.Mutex
	messages map[bucketKey]int
	members  map[memberKey]*MemberBucket
	users    map[string]UserSeen // guild:user
	channels map[string]ChannelSeen
}

func newCounter() *counter {
	return &counter{
		messages: map[bucketKey]int{},
		members:  map[memberKey]*MemberBucket{},
		users:    map[string]UserSeen{},
		channels: map[string]ChannelSeen{},
	}
}

// HandleMessage counts one gateway message. Bots and webhooks are not
// people; DMs have no guild. Content is never read, and the event carries
// none without the MESSAGE_CONTENT intent anyway.
func (p *Plugin) HandleMessage(m *discordgo.Message) {
	if m == nil || m.GuildID == "" || m.Author == nil || m.Author.Bot || m.WebhookID != "" || !p.enabled(m.GuildID) {
		return
	}
	at := m.Timestamp.UTC()
	if at.IsZero() {
		at = p.now()
	}
	name := m.Author.GlobalName
	if name == "" {
		name = m.Author.Username
	}
	if m.Member != nil && m.Member.Nick != "" {
		name = m.Member.Nick
	}
	chName := ""
	if p.channelName != nil {
		chName = p.channelName(m.GuildID, m.ChannelID)
	}

	c := p.count
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages[bucketKey{m.GuildID, m.ChannelID, m.Author.ID, at.Truncate(time.Hour)}]++
	c.users[m.GuildID+":"+m.Author.ID] = UserSeen{GuildID: m.GuildID, UserID: m.Author.ID, Name: name, Avatar: m.Author.Avatar, SeenAt: at}
	if chName != "" {
		c.channels[m.GuildID+":"+m.ChannelID] = ChannelSeen{GuildID: m.GuildID, ChannelID: m.ChannelID, Name: chName}
	}
}

// HandleMemberJoin and HandleMemberLeave count arrivals and departures.
// Nothing about who: see the migration.
func (p *Plugin) HandleMemberJoin(guildID string)  { p.member(guildID, 1, 0) }
func (p *Plugin) HandleMemberLeave(guildID string) { p.member(guildID, 0, 1) }

func (p *Plugin) member(guildID string, joined, departed int) {
	if guildID == "" || !p.enabled(guildID) {
		return
	}
	k := memberKey{guildID, p.now().Truncate(time.Hour)}
	c := p.count
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.members[k]
	if b == nil {
		b = &MemberBucket{GuildID: guildID, Hour: k.hour}
		c.members[k] = b
	}
	b.Joined += joined
	b.Departed += departed
}

// flush writes everything counted since the last one. On a failed write the
// cells go back into the tally so the next flush carries them, up to
// pendingCap; a message is never double counted because the maps are
// swapped out under the lock before anything is written.
func (p *Plugin) flush(ctx context.Context) {
	c := p.count
	c.mu.Lock()
	messages, members, users, channels := c.messages, c.members, c.users, c.channels
	c.messages, c.members, c.users, c.channels = map[bucketKey]int{}, map[memberKey]*MemberBucket{}, map[string]UserSeen{}, map[string]ChannelSeen{}
	c.mu.Unlock()
	if len(messages) == 0 && len(members) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, flushTimeout)
	defer cancel()

	rows := make([]Bucket, 0, len(messages))
	for k, n := range messages {
		rows = append(rows, Bucket{GuildID: k.guildID, ChannelID: k.channelID, UserID: k.userID, Hour: k.hour, Messages: n})
	}
	if err := p.store.AddMessages(ctx, rows); err != nil {
		p.log.Error("statistics: flush messages", "cells", len(rows), "err", err)
		p.restore(messages, nil)
	}

	mrows := make([]MemberBucket, 0, len(members))
	for _, b := range members {
		mrows = append(mrows, *b)
	}
	if err := p.store.AddMembers(ctx, mrows); err != nil {
		p.log.Error("statistics: flush members", "cells", len(mrows), "err", err)
		p.restore(nil, members)
	}

	// Names are best effort: a lost sighting is replaced by the next
	// message from the same person.
	seen := make([]UserSeen, 0, len(users))
	for _, u := range users {
		seen = append(seen, u)
	}
	if err := p.store.UpsertUsers(ctx, seen); err != nil {
		p.log.Warn("statistics: flush users", "err", err)
	}
	chs := make([]ChannelSeen, 0, len(channels))
	for _, ch := range channels {
		chs = append(chs, ch)
	}
	if err := p.store.UpsertChannels(ctx, chs); err != nil {
		p.log.Warn("statistics: flush channels", "err", err)
	}
}

func (p *Plugin) restore(messages map[bucketKey]int, members map[memberKey]*MemberBucket) {
	c := p.count
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.messages)+len(messages) > pendingCap {
		p.log.Error("statistics: dropping counts, database has been unreachable too long", "cells", len(messages))
		return
	}
	for k, n := range messages {
		c.messages[k] += n
	}
	for k, b := range members {
		if cur := c.members[k]; cur != nil {
			cur.Joined += b.Joined
			cur.Departed += b.Departed
		} else {
			c.members[k] = b
		}
	}
}

// flushLoop runs until ctx ends, then flushes once more so a shutdown
// loses at most nothing.
func (p *Plugin) flushLoop(ctx context.Context) {
	t := time.NewTicker(flushEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			p.flush(context.WithoutCancel(ctx))
			return
		case <-t.C:
			p.flush(ctx)
		}
	}
}

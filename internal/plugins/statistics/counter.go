package statistics

import (
	"context"
	"strings"
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

// voiceOpen is a member currently in a voice channel and the instant their
// time there was last attributed. Nothing here is written anywhere: the
// seconds are booked into hourly buckets as they pass, and this is only
// the cursor.
type voiceOpen struct {
	channelID string
	since     time.Time
}

// counter is the live tally between flushes.
type counter struct {
	mu       sync.Mutex
	messages map[bucketKey]int
	voice    map[bucketKey]int    // seconds
	open     map[string]voiceOpen // guild:user, members in voice right now
	members  map[memberKey]*MemberBucket
	users    map[string]UserSeen // guild:user
	channels map[string]ChannelSeen
}

func newCounter() *counter {
	return &counter{
		messages: map[bucketKey]int{},
		voice:    map[bucketKey]int{},
		open:     map[string]voiceOpen{},
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

// HandleVoiceState books a member's voice time as their channel changes.
// A mute or deafen toggle arrives as the same event with the channel
// unchanged and is ignored, so one sitting is one run of seconds. The
// seconds up to now go into the hourly buckets and the cursor moves,
// whether they left, switched rooms or joined.
func (p *Plugin) HandleVoiceState(vs *discordgo.VoiceStateUpdate) {
	if vs == nil || vs.VoiceState == nil || vs.GuildID == "" || vs.UserID == "" || !p.enabled(vs.GuildID) {
		return
	}
	if vs.BeforeUpdate != nil && vs.BeforeUpdate.ChannelID == vs.ChannelID {
		return
	}
	if vs.Member != nil && vs.Member.User != nil && vs.Member.User.Bot {
		return
	}
	now := p.now()
	c := p.count
	c.mu.Lock()
	defer c.mu.Unlock()
	c.settleVoice(vs.GuildID, vs.UserID, now)
	if vs.ChannelID == "" {
		delete(c.open, vs.GuildID+":"+vs.UserID)
		return
	}
	c.open[vs.GuildID+":"+vs.UserID] = voiceOpen{channelID: vs.ChannelID, since: now}
	if m := vs.Member; m != nil && m.User != nil {
		name := m.User.GlobalName
		if name == "" {
			name = m.User.Username
		}
		if m.Nick != "" {
			name = m.Nick
		}
		c.users[vs.GuildID+":"+vs.UserID] = UserSeen{GuildID: vs.GuildID, UserID: vs.UserID, Name: name, Avatar: m.User.Avatar, SeenAt: now}
	}
}

// SyncVoice seeds the cursors from who is in voice as the guild arrives,
// so a restart resumes counting for everybody already sitting there. The
// minutes between the old process going down and this one seeing the
// guild are lost, which is the honest direction: a gap the size of a
// deploy, never time nobody was there for.
func (p *Plugin) SyncVoice(guildID string, states []*discordgo.VoiceState) {
	if guildID == "" || !p.enabled(guildID) {
		return
	}
	now := p.now()
	c := p.count
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, vs := range states {
		if vs == nil || vs.UserID == "" || vs.ChannelID == "" {
			continue
		}
		if _, already := c.open[guildID+":"+vs.UserID]; already {
			continue
		}
		c.open[guildID+":"+vs.UserID] = voiceOpen{channelID: vs.ChannelID, since: now}
	}
}

// settleVoice books one member's open time up to now into the hourly
// buckets, split at hour boundaries, and moves the cursor. Caller holds
// the lock.
func (c *counter) settleVoice(guildID, userID string, now time.Time) {
	k := guildID + ":" + userID
	o, ok := c.open[k]
	if !ok {
		return
	}
	for _, h := range hourSlices(o.since, now) {
		c.voice[bucketKey{guildID, o.channelID, userID, h.hour}] += h.seconds
	}
	o.since = now
	c.open[k] = o
}

type hourSlice struct {
	hour    time.Time
	seconds int
}

// hourSlices splits [from, to) into whole seconds per UTC hour bucket.
func hourSlices(from, to time.Time) []hourSlice {
	var out []hourSlice
	for from.Before(to) {
		hour := from.Truncate(time.Hour)
		end := hour.Add(time.Hour)
		if end.After(to) {
			end = to
		}
		if secs := int(end.Sub(from).Seconds()); secs > 0 {
			out = append(out, hourSlice{hour: hour, seconds: secs})
		}
		from = end
	}
	return out
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
	// Whoever is still in voice gets their time so far booked now, so a
	// crash costs at most one flush interval of anybody's sitting. A guild
	// that turned the plugin off mid-sitting drops its cursors here: its
	// leave events are no longer heard, so nothing else would ever close
	// them.
	now := p.now()
	for k := range c.open {
		guildID, userID, _ := strings.Cut(k, ":")
		if !p.enabled(guildID) {
			delete(c.open, k)
			continue
		}
		c.settleVoice(guildID, userID, now)
	}
	messages, voice, members, users, channels := c.messages, c.voice, c.members, c.users, c.channels
	c.messages, c.voice, c.members, c.users, c.channels = map[bucketKey]int{}, map[bucketKey]int{}, map[memberKey]*MemberBucket{}, map[string]UserSeen{}, map[string]ChannelSeen{}
	c.mu.Unlock()
	if len(messages) == 0 && len(voice) == 0 && len(members) == 0 {
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
		p.restore(messages, nil, nil)
	}

	vrows := make([]VoiceBucket, 0, len(voice))
	for k, secs := range voice {
		vrows = append(vrows, VoiceBucket{GuildID: k.guildID, ChannelID: k.channelID, UserID: k.userID, Hour: k.hour, Seconds: secs})
	}
	if err := p.store.AddVoice(ctx, vrows); err != nil {
		p.log.Error("statistics: flush voice", "cells", len(vrows), "err", err)
		p.restore(nil, voice, nil)
	}

	mrows := make([]MemberBucket, 0, len(members))
	for _, b := range members {
		mrows = append(mrows, *b)
	}
	if err := p.store.AddMembers(ctx, mrows); err != nil {
		p.log.Error("statistics: flush members", "cells", len(mrows), "err", err)
		p.restore(nil, nil, members)
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

func (p *Plugin) restore(messages, voice map[bucketKey]int, members map[memberKey]*MemberBucket) {
	c := p.count
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.messages)+len(messages)+len(c.voice)+len(voice) > pendingCap {
		p.log.Error("statistics: dropping counts, database has been unreachable too long", "cells", len(messages)+len(voice))
		return
	}
	for k, n := range messages {
		c.messages[k] += n
	}
	for k, n := range voice {
		c.voice[k] += n
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

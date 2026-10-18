package statistics

import (
	"context"
	"sync"
	"time"

	"github.com/disgoorg/disgo/discord"
	snow "github.com/disgoorg/snowflake/v2"
)

// flushEvery is how often the in-memory tallies are written out. The
// gateway handlers below only touch a map under a lock; the database sees
// one statement per table per flush rather than one per message.
const flushEvery = 10 * time.Second

// flushTimeout bounds one flush; a wedged database must not hold the
// counters' lock, since every message handler waits on it.
const flushTimeout = 30 * time.Second

// pendingCap is how many distinct cells a failed flush may carry over
// before the oldest are dropped. A database that stays down should cost
// counts, not memory.
// ponytail: flat cap and drop-all; per-guild caps if one guild ever floods.
const pendingCap = 50000

// The tally is keyed on the gateway's snowflakes, so counting a message
// formats nothing and allocates nothing; IDs become text once, at flush.
type cell struct {
	guild, channel, user snow.ID
	hour                 time.Time
}

// seat is a member, or a channel, in one guild.
type seat struct{ guild, id snow.ID }

// sighting is the newest name and avatar seen for a member.
type sighting struct {
	name, avatar string
	at           time.Time
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
	channel snow.ID
	since   time.Time
}

// counter is the live tally between flushes.
type counter struct {
	mu       sync.Mutex
	messages map[cell]int
	voice    map[cell]int       // seconds
	open     map[seat]voiceOpen // members in voice right now
	members  map[memberKey]*MemberBucket
	users    map[seat]sighting
	channels map[seat]string // name
}

func newCounter() *counter {
	return &counter{
		messages: map[cell]int{},
		voice:    map[cell]int{},
		open:     map[seat]voiceOpen{},
		members:  map[memberKey]*MemberBucket{},
		users:    map[seat]sighting{},
		channels: map[seat]string{},
	}
}

// post is what the backfill reads of a message from history.
type post struct {
	// id is the message's own, which the backfill pages by.
	id                  string
	channelID, authorID string
	at                  time.Time
	// name is the member's nickname, else their display name, else their
	// username.
	name, avatar string
	// skip is a bot, a webhook, or a message with no author.
	skip bool
}

// heard is what the live counter reads of a gateway message, on snowflakes.
type heard struct {
	guild, channel, author snow.ID
	at                     time.Time
	name, avatar           string
	skip                   bool
}

// HandleMessage counts one gateway message. Bots and webhooks are not
// people; DMs have no guild. Content is never read, and the event carries
// none without the MESSAGE_CONTENT intent anyway.
func (p *Plugin) HandleMessage(m discord.Message) {
	if m.GuildID == nil {
		return
	}
	in := heard{
		guild: *m.GuildID, channel: m.ChannelID, author: m.Author.ID,
		at: m.CreatedAt, avatar: deref(m.Author.Avatar),
		skip: m.Author.ID == 0 || m.Author.Bot || m.WebhookID != nil,
	}
	in.name = m.Author.Username
	if g := deref(m.Author.GlobalName); g != "" {
		in.name = g
	}
	if m.Member != nil && deref(m.Member.Nick) != "" {
		in.name = *m.Member.Nick
	}
	p.count1(in)
}

func (p *Plugin) count1(m heard) {
	if m.guild == 0 || m.skip || !p.enabledID(m.guild) {
		return
	}
	at := m.at.UTC()
	if at.IsZero() {
		at = p.now()
	}
	chName := ""
	if p.channelName != nil {
		chName = p.channelName(m.guild, m.channel)
	}

	c := p.count
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages[cell{m.guild, m.channel, m.author, at.Truncate(time.Hour)}]++
	c.users[seat{m.guild, m.author}] = sighting{name: m.name, avatar: m.avatar, at: at}
	if chName != "" {
		c.channels[seat{m.guild, m.channel}] = chName
	}
}

// move is what statistics reads of a voice state change.
type move struct {
	// channel is 0 when the member is in no voice channel.
	guild, user, channel snow.ID
	// before is the channel before the change, when the gateway cache knew
	// it; a mute or deafen leaves it equal to channel.
	before *snow.ID
	bot    bool
	// named is set when the event carried the member, and name and avatar
	// are theirs.
	named        bool
	name, avatar string
}

// HandleVoiceState books a member's voice time as their channel changes.
// A mute or deafen toggle arrives as the same event with the channel
// unchanged and is ignored, so one sitting is one run of seconds. The
// seconds up to now go into the hourly buckets and the cursor moves,
// whether they left, switched rooms or joined. old is the state before,
// from the gateway cache, zero when it had none.
func (p *Plugin) HandleVoiceState(vs, old discord.VoiceState, m discord.Member) {
	in := move{guild: vs.GuildID, user: vs.UserID, channel: channelOf(vs.ChannelID), bot: m.User.Bot}
	if old.UserID != 0 {
		before := channelOf(old.ChannelID)
		in.before = &before
	}
	if m.User.ID != 0 {
		in.named, in.avatar = true, deref(m.User.Avatar)
		in.name = m.User.Username
		if g := deref(m.User.GlobalName); g != "" {
			in.name = g
		}
		if n := deref(m.Nick); n != "" {
			in.name = n
		}
	}
	p.voice1(in)
}

func (p *Plugin) voice1(vs move) {
	if vs.guild == 0 || vs.user == 0 || !p.enabledID(vs.guild) {
		return
	}
	if vs.before != nil && *vs.before == vs.channel {
		return
	}
	if vs.bot {
		return
	}
	now := p.now()
	c := p.count
	c.mu.Lock()
	defer c.mu.Unlock()
	who := seat{vs.guild, vs.user}
	c.settleVoice(who, now)
	if vs.channel == 0 {
		delete(c.open, who)
		return
	}
	c.open[who] = voiceOpen{channel: vs.channel, since: now}
	if vs.named {
		c.users[who] = sighting{name: vs.name, avatar: vs.avatar, at: now}
	}
}

// SyncVoice seeds the cursors from who is in voice as the guild arrives,
// so a restart resumes counting for everybody already sitting there. The
// minutes between the old process going down and this one seeing the
// guild are lost, which is the honest direction: a gap the size of a
// deploy, never time nobody was there for.
func (p *Plugin) SyncVoice(guildID string, states []discord.VoiceState) {
	g, err := snow.Parse(guildID)
	if err != nil {
		return
	}
	seats := make([][2]snow.ID, 0, len(states))
	for _, vs := range states {
		seats = append(seats, [2]snow.ID{vs.UserID, channelOf(vs.ChannelID)})
	}
	p.syncVoice(g, seats)
}

// syncVoice is SyncVoice for seats of user and channel.
func (p *Plugin) syncVoice(guildID snow.ID, seats [][2]snow.ID) {
	if guildID == 0 || !p.enabledID(guildID) {
		return
	}
	now := p.now()
	c := p.count
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range seats {
		who, ch := seat{guildID, s[0]}, s[1]
		if who.id == 0 || ch == 0 {
			continue
		}
		if _, already := c.open[who]; already {
			continue
		}
		c.open[who] = voiceOpen{channel: ch, since: now}
	}
}

func channelOf(id *snow.ID) snow.ID {
	if id == nil {
		return 0
	}
	return *id
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// settleVoice books one member's open time up to now into the hourly
// buckets, split at hour boundaries, and moves the cursor. Caller holds
// the lock.
func (c *counter) settleVoice(who seat, now time.Time) {
	o, ok := c.open[who]
	if !ok {
		return
	}
	for _, h := range hourSlices(o.since, now) {
		c.voice[cell{who.guild, o.channel, who.id, h.hour}] += h.seconds
	}
	o.since = now
	c.open[who] = o
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
	for who := range c.open {
		if !p.enabledID(who.guild) {
			delete(c.open, who)
			continue
		}
		c.settleVoice(who, now)
	}
	messages, voice, members, users, channels := c.messages, c.voice, c.members, c.users, c.channels
	c.messages, c.voice, c.members, c.users, c.channels = map[cell]int{}, map[cell]int{}, map[memberKey]*MemberBucket{}, map[seat]sighting{}, map[seat]string{}
	c.mu.Unlock()
	if len(messages) == 0 && len(voice) == 0 && len(members) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, flushTimeout)
	defer cancel()

	rows := make([]Bucket, 0, len(messages))
	for k, n := range messages {
		rows = append(rows, Bucket{GuildID: k.guild.String(), ChannelID: k.channel.String(), UserID: k.user.String(), Hour: k.hour, Messages: n})
	}
	if err := p.store.AddMessages(ctx, rows); err != nil {
		p.log.Error("statistics: flush messages", "cells", len(rows), "err", err)
		p.restore(messages, nil, nil)
	}

	vrows := make([]VoiceBucket, 0, len(voice))
	for k, secs := range voice {
		vrows = append(vrows, VoiceBucket{GuildID: k.guild.String(), ChannelID: k.channel.String(), UserID: k.user.String(), Hour: k.hour, Seconds: secs})
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
	for who, u := range users {
		seen = append(seen, UserSeen{GuildID: who.guild.String(), UserID: who.id.String(), Name: u.name, Avatar: u.avatar, SeenAt: u.at})
	}
	if err := p.store.UpsertUsers(ctx, seen); err != nil {
		p.log.Warn("statistics: flush users", "err", err)
	}
	chs := make([]ChannelSeen, 0, len(channels))
	for where, name := range channels {
		chs = append(chs, ChannelSeen{GuildID: where.guild.String(), ChannelID: where.id.String(), Name: name})
	}
	if err := p.store.UpsertChannels(ctx, chs); err != nil {
		p.log.Warn("statistics: flush channels", "err", err)
	}
}

func (p *Plugin) restore(messages, voice map[cell]int, members map[memberKey]*MemberBucket) {
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

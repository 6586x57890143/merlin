package statistics

import (
	"context"
	"errors"
	"slices"
	"sort"
	"sync"
	"time"
)

// fakeStore is Store in memory, keyed the way the tables are, so the
// counter, the backfill and the report can be exercised against exactly the
// semantics the SQL has: additive live writes, replace-the-hour backfill
// writes, newest-sighting-wins names.
type fakeStore struct {
	mu       sync.Mutex
	hourly   map[bucketKey]int
	voice    map[bucketKey]int // seconds
	members  map[memberKey]MemberBucket
	users    map[string]UserSeen // guild:user
	channels map[string]string   // guild:channel -> name
	config   map[string]Config
	backfill map[string]BackfillRow // guild:channel

	addErr    error // fails AddMessages
	reportErr error // fails Report
	setHours  int   // SetHour calls, for the checkpoint tests
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		hourly:   map[bucketKey]int{},
		voice:    map[bucketKey]int{},
		members:  map[memberKey]MemberBucket{},
		users:    map[string]UserSeen{},
		channels: map[string]string{},
		config:   map[string]Config{},
		backfill: map[string]BackfillRow{},
	}
}

func (f *fakeStore) AddMessages(_ context.Context, rows []Bucket) error {
	if f.addErr != nil {
		return f.addErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range rows {
		f.hourly[bucketKey{r.GuildID, r.ChannelID, r.UserID, r.Hour}] += r.Messages
	}
	return nil
}

func (f *fakeStore) SetHour(_ context.Context, guildID, channelID string, hour time.Time, counts map[string]int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setHours++
	for k := range f.hourly {
		if k.guildID == guildID && k.channelID == channelID && k.hour.Equal(hour) {
			delete(f.hourly, k)
		}
	}
	for u, n := range counts {
		f.hourly[bucketKey{guildID, channelID, u, hour}] = n
	}
	return nil
}

func (f *fakeStore) AddVoice(_ context.Context, rows []VoiceBucket) error {
	if f.addErr != nil {
		return f.addErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range rows {
		f.voice[bucketKey{r.GuildID, r.ChannelID, r.UserID, r.Hour}] += r.Seconds
	}
	return nil
}

func (f *fakeStore) AddMembers(_ context.Context, rows []MemberBucket) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range rows {
		k := memberKey{r.GuildID, r.Hour}
		cur := f.members[k]
		cur.GuildID, cur.Hour = r.GuildID, r.Hour
		cur.Joined += r.Joined
		cur.Departed += r.Departed
		f.members[k] = cur
	}
	return nil
}

func (f *fakeStore) UpsertUsers(_ context.Context, users []UserSeen) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range users {
		k := u.GuildID + ":" + u.UserID
		if cur, ok := f.users[k]; ok && cur.SeenAt.After(u.SeenAt) {
			continue
		}
		f.users[k] = u
	}
	return nil
}

func (f *fakeStore) UpsertChannels(_ context.Context, channels []ChannelSeen) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range channels {
		if c.Name != "" {
			f.channels[c.GuildID+":"+c.ChannelID] = c.Name
		}
	}
	return nil
}

func (f *fakeStore) Config(_ context.Context, guildID string) (Config, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.config[guildID]; ok {
		return c, nil
	}
	return Config{GuildID: guildID, RetentionDays: defaultRetentionDays}, nil
}

func (f *fakeStore) SetRetention(_ context.Context, guildID string, days int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.config[guildID]
	c.GuildID, c.RetentionDays = guildID, days
	f.config[guildID] = c
	return nil
}

func (f *fakeStore) MarkLive(_ context.Context, guildID string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.config[guildID]
	if !ok {
		c = Config{GuildID: guildID, RetentionDays: defaultRetentionDays}
	}
	if c.LiveSince.IsZero() {
		c.LiveSince = at
	}
	f.config[guildID] = c
	return nil
}

func (f *fakeStore) Prune(_ context.Context, now time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for k := range f.hourly {
		days := defaultRetentionDays
		if c, ok := f.config[k.guildID]; ok {
			days = c.RetentionDays
		}
		if k.hour.Before(now.Add(-time.Duration(days) * 24 * time.Hour)) {
			delete(f.hourly, k)
			n++
		}
	}
	return n, nil
}

func (f *fakeStore) Report(_ context.Context, guildID, channelID string, from, to time.Time) ([]Row, error) {
	if f.reportErr != nil {
		return nil, f.reportErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	byUser := map[string]*Row{}
	for k, n := range f.hourly {
		if k.guildID != guildID || k.hour.Before(from.Truncate(time.Hour)) || !k.hour.Before(to) {
			continue
		}
		if channelID != "" && k.channelID != channelID {
			continue
		}
		r := byUser[k.userID]
		if r == nil {
			r = &Row{UserID: k.userID}
			byUser[k.userID] = r
		}
		r.Messages += n
		if !slices.Contains(r.Channels, k.channelID) {
			r.Channels = append(r.Channels, k.channelID)
		}
		if k.hour.After(r.Last) {
			r.Last = k.hour
		}
	}
	for k, secs := range f.voice {
		if k.guildID != guildID || k.hour.Before(from.Truncate(time.Hour)) || !k.hour.Before(to) {
			continue
		}
		if channelID != "" && k.channelID != channelID {
			continue
		}
		r := byUser[k.userID]
		if r == nil {
			r = &Row{UserID: k.userID}
			byUser[k.userID] = r
		}
		r.VoiceSeconds += secs
		if !slices.Contains(r.VoiceChannels, k.channelID) {
			r.VoiceChannels = append(r.VoiceChannels, k.channelID)
		}
	}
	var out []Row
	for _, r := range byUser {
		sort.Strings(r.Channels)
		sort.Strings(r.VoiceChannels)
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UserID < out[j].UserID })
	return out, nil
}

func (f *fakeStore) Days(_ context.Context, guildID, channelID string, from, to time.Time) ([]DayStat, error) {
	return f.activity(guildID, channelID, from, to, 24*time.Hour)
}

func (f *fakeStore) Hours(_ context.Context, guildID, channelID string, from, to time.Time) ([]DayStat, error) {
	return f.activity(guildID, channelID, from, to, time.Hour)
}

func (f *fakeStore) activity(guildID, channelID string, from, to time.Time, unit time.Duration) ([]DayStat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	days := map[time.Time]*DayStat{}
	at := func(hour time.Time) *DayStat {
		day := hour.Truncate(unit)
		d := days[day]
		if d == nil {
			d = &DayStat{Day: day}
			days[day] = d
		}
		return d
	}
	in := func(k bucketKey) bool {
		return k.guildID == guildID && !k.hour.Before(from.Truncate(time.Hour)) && k.hour.Before(to) &&
			(channelID == "" || k.channelID == channelID)
	}
	for k, n := range f.hourly {
		if in(k) {
			at(k.hour).Messages += n
		}
	}
	for k, secs := range f.voice {
		if in(k) {
			at(k.hour).VoiceSeconds += secs
		}
	}
	var out []DayStat
	for _, d := range days {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day.Before(out[j].Day) })
	return out, nil
}

func (f *fakeStore) Users(_ context.Context, guildID string, ids []string) (map[string]UserSeen, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]UserSeen{}
	for _, id := range ids {
		if u, ok := f.users[guildID+":"+id]; ok {
			out[id] = u
		}
	}
	return out, nil
}

func (f *fakeStore) Channels(_ context.Context, guildID string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for k, name := range f.channels {
		if len(k) > len(guildID) && k[:len(guildID)+1] == guildID+":" {
			out[k[len(guildID)+1:]] = name
		}
	}
	return out, nil
}

func (f *fakeStore) ChannelTotals(_ context.Context, guildID string, from, to time.Time) ([]ChannelTotal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	totals := map[string]*ChannelTotal{}
	people := map[string]map[string]bool{}
	for k, n := range f.hourly {
		if k.guildID != guildID || k.hour.Before(from.Truncate(time.Hour)) || !k.hour.Before(to) {
			continue
		}
		t := totals[k.channelID]
		if t == nil {
			t = &ChannelTotal{ChannelID: k.channelID}
			totals[k.channelID] = t
			people[k.channelID] = map[string]bool{}
		}
		t.Messages += n
		people[k.channelID][k.userID] = true
	}
	var out []ChannelTotal
	for id, t := range totals {
		t.People = len(people[id])
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Messages > out[j].Messages })
	return out, nil
}

func (f *fakeStore) MemberDays(_ context.Context, guildID string, from, to time.Time) ([]MemberDay, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	days := map[time.Time]*MemberDay{}
	for k, b := range f.members {
		if k.guildID != guildID || k.hour.Before(from.Truncate(time.Hour)) || !k.hour.Before(to) {
			continue
		}
		day := k.hour.Truncate(24 * time.Hour)
		d := days[day]
		if d == nil {
			d = &MemberDay{Day: day}
			days[day] = d
		}
		d.Joined += b.Joined
		d.Departed += b.Departed
	}
	var out []MemberDay
	for _, d := range days {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day.Before(out[j].Day) })
	return out, nil
}

func (f *fakeStore) OldestHour(_ context.Context, guildID string) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var oldest time.Time
	for k := range f.hourly {
		if k.guildID == guildID && (oldest.IsZero() || k.hour.Before(oldest)) {
			oldest = k.hour
		}
	}
	return oldest, nil
}

func (f *fakeStore) RequestBackfill(_ context.Context, rows []BackfillRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range rows {
		r.Cursor, r.Done, r.Error = "", false, ""
		f.backfill[r.GuildID+":"+r.ChannelID] = r
	}
	return nil
}

func (f *fakeStore) PendingBackfill(_ context.Context, guildID string) ([]BackfillRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []BackfillRow
	for _, r := range f.backfill {
		if r.GuildID == guildID && !r.Done {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ChannelID < out[j].ChannelID })
	return out, nil
}

func (f *fakeStore) SetBackfillCursor(_ context.Context, guildID, channelID, cursor string, done bool, errText string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.backfill[guildID+":"+channelID]
	if !ok {
		return errors.New("fake store: no such backfill row")
	}
	r.Cursor, r.Done, r.Error = cursor, done, errText
	f.backfill[guildID+":"+channelID] = r
	return nil
}

func (f *fakeStore) BackfillStatus(_ context.Context, guildID string) (BackfillSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var s BackfillSummary
	for _, r := range f.backfill {
		if r.GuildID != guildID {
			continue
		}
		switch {
		case !r.Done:
			s.Pending++
		case r.Error != "":
			s.Failed++
		default:
			s.Done++
		}
		if s.From.IsZero() || r.From.Before(s.From) {
			s.From = r.From
		}
	}
	return s, nil
}

// voiceTotal sums a guild's voice seconds, optionally for one user.
func (f *fakeStore) voiceTotal(guildID, userID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for k, c := range f.voice {
		if k.guildID == guildID && (userID == "" || k.userID == userID) {
			n += c
		}
	}
	return n
}

// total sums a guild's buckets, optionally for one user.
func (f *fakeStore) total(guildID, userID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for k, c := range f.hourly {
		if k.guildID == guildID && (userID == "" || k.userID == userID) {
			n += c
		}
	}
	return n
}

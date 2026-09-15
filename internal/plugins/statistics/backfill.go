package statistics

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/scheduler"
)

// A backfill reads a channel's history over REST, newest first, into the
// same buckets the gateway fills, for the time before this bot was
// counting. It runs as a Scheduler job so it survives everything the old
// scan did not: a restart resumes it, an interaction token has nothing to do
// with it, and the operator does not have to keep a Discord window open.
//
// It is idempotent by construction. An hour is written only once every
// message in it has been read (paging is contiguous and newest-first, so
// the first message of an older hour proves the newer one is complete), and
// written by replacing the channel-hour rather than adding to it, so
// re-reading after a restart lands on the same numbers. The cursor advances
// with each hour written, so at most one partial hour is read twice and
// nothing is counted twice.

const (
	// backfillSlice is how long one job run works before returning, well
	// inside the Scheduler's jobTimeout so a run ends by choice with its
	// cursor written rather than being killed and recorded as a failure.
	backfillSlice = 5 * time.Minute
	backfillEvery = time.Minute
)

var errSliceOver = errors.New("backfill slice over")

func backfillJobKey(guildID string) string { return scheduler.JobKey(guildID, "stats-backfill") }

// reconcileBackfillJob registers the job where a guild has channels
// pending and unregisters it where it does not, the same "a job exists only
// where it has work" rule as rotation's sweep.
func (p *Plugin) reconcileBackfillJob(ctx context.Context, guildID string) {
	if p.sched == nil {
		return
	}
	pending, err := p.store.PendingBackfill(ctx, guildID)
	if err != nil {
		p.log.Error("statistics: read backfill queue", "guild", guildID, "err", err)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	key := backfillJobKey(guildID)
	switch {
	case len(pending) > 0 && !p.backfillRegistered[guildID]:
		if err := p.sched.Register(key, core.CronSpec{Schedule: core.IntervalSchedule{Interval: backfillEvery}}, p.makeBackfillJob(guildID)); err != nil {
			p.log.Error("statistics: register backfill job", "guild", guildID, "err", err)
			return
		}
		p.backfillRegistered[guildID] = true
	case len(pending) == 0 && p.backfillRegistered[guildID]:
		if err := p.sched.Unregister(key); err != nil {
			p.log.Error("statistics: unregister backfill job", "guild", guildID, "err", err)
			return
		}
		delete(p.backfillRegistered, guildID)
	}
}

func (p *Plugin) makeBackfillJob(guildID string) func(context.Context) error {
	return func(ctx context.Context) error {
		err := p.backfill(ctx, guildID)
		p.reconcileBackfillJob(context.WithoutCancel(ctx), guildID)
		return err
	}
}

// backfill works through a guild's pending channels for one slice.
func (p *Plugin) backfill(ctx context.Context, guildID string) error {
	pending, err := p.store.PendingBackfill(ctx, guildID)
	if err != nil {
		return err
	}
	deadline := p.now().Add(backfillSlice)
	tick := time.NewTicker(requestGap)
	defer tick.Stop()
	for _, row := range pending {
		err := p.backfillChannel(ctx, row, deadline, tick.C)
		switch {
		case errors.Is(err, errSliceOver):
			return nil
		case errors.Is(err, errScanStopped):
			return nil // shutdown; the cursor is where it was last written
		case err != nil && !transient(err):
			// Missing Access and its kin: this channel is not readable and
			// asking again will not change that. Recorded and moved past.
			p.log.Warn("statistics: backfill skipped a channel", "guild", guildID, "channel", row.ChannelID, "err", err)
			if serr := p.store.SetBackfillCursor(ctx, guildID, row.ChannelID, row.Cursor, true, err.Error()); serr != nil {
				return serr
			}
		case err != nil:
			return fmt.Errorf("statistics: backfill %s: %w", row.ChannelID, err)
		}
	}
	return nil
}

// backfillChannel pages one channel from its cursor (or from until) back to
// from, writing each hour as it completes.
func (p *Plugin) backfillChannel(ctx context.Context, row BackfillRow, deadline time.Time, tick <-chan time.Time) error {
	before := row.Cursor
	if before == "" {
		before = strconv.FormatInt(snowflake(row.Until), 10)
	}
	after := snowflake(row.From)

	pending := map[string]int{}
	var pendingHour time.Time
	users := map[string]UserSeen{}

	// writeHour lands the completed hour and moves the cursor to just past
	// the newest message not yet written, so a resume re-reads from there.
	writeHour := func(cursor string, done bool) error {
		if !pendingHour.IsZero() {
			if err := p.store.SetHour(ctx, row.GuildID, row.ChannelID, pendingHour, pending); err != nil {
				return err
			}
		}
		seen := make([]UserSeen, 0, len(users))
		for _, u := range users {
			seen = append(seen, u)
		}
		if err := p.store.UpsertUsers(ctx, seen); err != nil {
			p.log.Warn("statistics: backfill users", "err", err)
		}
		users = map[string]UserSeen{}
		pending, pendingHour = map[string]int{}, time.Time{}
		return p.store.SetBackfillCursor(ctx, row.GuildID, row.ChannelID, cursor, done, "")
	}

	for {
		if p.now().After(deadline) {
			return errSliceOver
		}
		msgs, err := page(ctx, p.source, row.ChannelID, before, tick)
		if err != nil {
			return err
		}
		if len(msgs) == 0 {
			return writeHour(before, true)
		}
		for _, m := range msgs {
			id := messageID(m.ID)
			if id == 0 || id >= snowflake(row.Until) {
				continue
			}
			if id < after {
				return writeHour(m.ID, true)
			}
			if m.Author == nil || m.Author.Bot || m.WebhookID != "" {
				continue
			}
			hour := m.Timestamp.UTC().Truncate(time.Hour)
			if !pendingHour.IsZero() && !hour.Equal(pendingHour) {
				// First message of an older hour: the newer one is whole.
				// Resume just above this message, which is not written yet.
				if err := writeHour(strconv.FormatInt(id+1, 10), false); err != nil {
					return err
				}
			}
			pendingHour = hour
			pending[m.Author.ID]++
			name := m.Author.GlobalName
			if name == "" {
				name = m.Author.Username
			}
			users[m.Author.ID] = UserSeen{GuildID: row.GuildID, UserID: m.Author.ID, Name: name, Avatar: m.Author.Avatar, SeenAt: m.Timestamp.UTC()}
		}
		before = msgs[len(msgs)-1].ID
	}
}

// queueBackfill lists the guild's readable channels and active threads and
// queues each from from up to the hour counting began. Returns how many.
func (p *Plugin) queueBackfill(ctx context.Context, guildID string, from, until time.Time) (int, error) {
	channels, err := p.source.GuildChannels(guildID)
	if err != nil {
		return 0, fmt.Errorf("could not list this server's channels: %w", err)
	}
	if threads, terr := p.source.ThreadsActive(guildID); terr == nil && threads != nil {
		channels = append(channels, threads.Threads...)
	}
	var rows []BackfillRow
	var names []ChannelSeen
	for _, ch := range channels {
		if !readable(ch) {
			continue
		}
		rows = append(rows, BackfillRow{GuildID: guildID, ChannelID: ch.ID, From: from, Until: until})
		names = append(names, ChannelSeen{GuildID: guildID, ChannelID: ch.ID, Name: ch.Name})
	}
	if err := p.store.UpsertChannels(ctx, names); err != nil {
		p.log.Warn("statistics: record channel names", "guild", guildID, "err", err)
	}
	if err := p.store.RequestBackfill(ctx, rows); err != nil {
		return 0, err
	}
	p.reconcileBackfillJob(ctx, guildID)
	return len(rows), nil
}

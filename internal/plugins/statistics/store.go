package statistics

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// defaultRetentionDays is how long buckets live where a guild has not said
// otherwise. Long enough to answer "who was around last quarter", short
// enough that the table is never a permanent record of anybody.
const defaultRetentionDays = 90

// Bucket is one (channel, member, hour) cell.
type Bucket struct {
	GuildID, ChannelID, UserID string
	Hour                       time.Time
	Messages                   int
}

// MemberBucket is one hour of joins and departures for a guild.
type MemberBucket struct {
	GuildID          string
	Hour             time.Time
	Joined, Departed int
}

// UserSeen is a member as they appeared on a message.
type UserSeen struct {
	GuildID, UserID, Name, Avatar string
	SeenAt                        time.Time
}

// ChannelSeen is a channel's name at the time a message was counted in it.
type ChannelSeen struct {
	GuildID, ChannelID, Name string
}

// Config is a guild's statistics settings. LiveSince is zero until the bot
// has seen the guild once.
type Config struct {
	GuildID       string
	RetentionDays int
	LiveSince     time.Time
}

// Row is one member's total over a window.
type Row struct {
	UserID   string
	Messages int
	Channels []string
	Last     time.Time
}

// ChannelTotal is one channel's volume over a window.
type ChannelTotal struct {
	ChannelID string
	Messages  int
	People    int
}

// MemberDay is one UTC day of joins and departures.
type MemberDay struct {
	Day              time.Time
	Joined, Departed int
}

// BackfillRow is one channel's place in a backfill.
type BackfillRow struct {
	GuildID, ChannelID string
	From, Until        time.Time
	Cursor             string
	Done               bool
	Error              string
}

// BackfillSummary is what /statistics status reports about a backfill.
type BackfillSummary struct {
	Pending, Done, Failed int
	From                  time.Time // earliest from_at requested, zero if none
}

// Store is this plugin's persistence seam, the narrow-interface pattern
// every plugin here uses so the logic is tested against an in-memory fake.
type Store interface {
	// AddMessages adds each bucket's count onto whatever is stored: the live
	// path, where every flush carries new messages only.
	AddMessages(ctx context.Context, rows []Bucket) error
	// SetHour replaces every row for one channel-hour with counts: the
	// backfill path, which only writes an hour once it has read all of it,
	// so re-reading after a restart lands on the same numbers.
	SetHour(ctx context.Context, guildID, channelID string, hour time.Time, counts map[string]int) error
	AddMembers(ctx context.Context, rows []MemberBucket) error
	// UpsertUsers keeps the newest sighting per member.
	UpsertUsers(ctx context.Context, users []UserSeen) error
	UpsertChannels(ctx context.Context, channels []ChannelSeen) error

	Config(ctx context.Context, guildID string) (Config, error)
	SetRetention(ctx context.Context, guildID string, days int) error
	// MarkLive sets live_since to at if it is not already set.
	MarkLive(ctx context.Context, guildID string, at time.Time) error
	// Prune drops every bucket older than its guild's retention. Returns
	// rows removed.
	Prune(ctx context.Context, now time.Time) (int64, error)

	Report(ctx context.Context, guildID, channelID string, from, to time.Time) ([]Row, error)
	Users(ctx context.Context, guildID string, userIDs []string) (map[string]UserSeen, error)
	Channels(ctx context.Context, guildID string) (map[string]string, error)
	ChannelTotals(ctx context.Context, guildID string, from, to time.Time) ([]ChannelTotal, error)
	MemberDays(ctx context.Context, guildID string, from, to time.Time) ([]MemberDay, error)
	// OldestHour is the earliest bucket a guild holds, zero if none.
	OldestHour(ctx context.Context, guildID string) (time.Time, error)

	// RequestBackfill queues channels, replacing any earlier row for the
	// same channel so a second request restarts it.
	RequestBackfill(ctx context.Context, rows []BackfillRow) error
	PendingBackfill(ctx context.Context, guildID string) ([]BackfillRow, error)
	SetBackfillCursor(ctx context.Context, guildID, channelID, cursor string, done bool, errText string) error
	BackfillStatus(ctx context.Context, guildID string) (BackfillSummary, error)
}

type pgStore struct{ pool *pgxpool.Pool }

// NewPostgresStore backs Store with the stats_* tables (migration 0035).
func NewPostgresStore(pool *pgxpool.Pool) Store { return &pgStore{pool: pool} }

// Every bulk write is one statement over unnested arrays rather than a
// round trip per row: a flush from a busy guild is a few hundred cells.
func (s *pgStore) AddMessages(ctx context.Context, rows []Bucket) error {
	if len(rows) == 0 {
		return nil
	}
	guilds, channels, users := make([]string, len(rows)), make([]string, len(rows)), make([]string, len(rows))
	hours, counts := make([]time.Time, len(rows)), make([]int32, len(rows))
	for i, r := range rows {
		guilds[i], channels[i], users[i], hours[i], counts[i] = r.GuildID, r.ChannelID, r.UserID, r.Hour, int32(r.Messages)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO stats_hourly (guild_id, channel_id, user_id, hour, messages)
		SELECT * FROM unnest($1::text[], $2::text[], $3::text[], $4::timestamptz[], $5::int[])
		ON CONFLICT (guild_id, channel_id, user_id, hour)
		DO UPDATE SET messages = stats_hourly.messages + EXCLUDED.messages
	`, guilds, channels, users, hours, counts)
	if err != nil {
		return fmt.Errorf("statistics store: add messages: %w", err)
	}
	return nil
}

func (s *pgStore) SetHour(ctx context.Context, guildID, channelID string, hour time.Time, counts map[string]int) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("statistics store: set hour: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM stats_hourly WHERE guild_id = $1 AND channel_id = $2 AND hour = $3`,
		guildID, channelID, hour); err != nil {
		return fmt.Errorf("statistics store: set hour: clear: %w", err)
	}
	if len(counts) > 0 {
		users, n := make([]string, 0, len(counts)), make([]int32, 0, len(counts))
		for u, c := range counts {
			users, n = append(users, u), append(n, int32(c))
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO stats_hourly (guild_id, channel_id, user_id, hour, messages)
			SELECT $1::text, $2::text, u, $3::timestamptz, c FROM unnest($4::text[], $5::int[]) AS t(u, c)
		`, guildID, channelID, hour, users, n); err != nil {
			return fmt.Errorf("statistics store: set hour: insert: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("statistics store: set hour: commit: %w", err)
	}
	return nil
}

func (s *pgStore) AddMembers(ctx context.Context, rows []MemberBucket) error {
	if len(rows) == 0 {
		return nil
	}
	guilds, hours := make([]string, len(rows)), make([]time.Time, len(rows))
	joined, departed := make([]int32, len(rows)), make([]int32, len(rows))
	for i, r := range rows {
		guilds[i], hours[i], joined[i], departed[i] = r.GuildID, r.Hour, int32(r.Joined), int32(r.Departed)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO stats_members_hourly (guild_id, hour, joined, departed)
		SELECT * FROM unnest($1::text[], $2::timestamptz[], $3::int[], $4::int[])
		ON CONFLICT (guild_id, hour) DO UPDATE SET
			joined = stats_members_hourly.joined + EXCLUDED.joined,
			departed = stats_members_hourly.departed + EXCLUDED.departed
	`, guilds, hours, joined, departed)
	if err != nil {
		return fmt.Errorf("statistics store: add members: %w", err)
	}
	return nil
}

func (s *pgStore) UpsertUsers(ctx context.Context, users []UserSeen) error {
	if len(users) == 0 {
		return nil
	}
	guilds, ids, names, avatars := make([]string, len(users)), make([]string, len(users)), make([]string, len(users)), make([]string, len(users))
	seen := make([]time.Time, len(users))
	for i, u := range users {
		guilds[i], ids[i], names[i], avatars[i], seen[i] = u.GuildID, u.UserID, u.Name, u.Avatar, u.SeenAt
	}
	// DISTINCT ON: a batch naming one member twice would otherwise be
	// refused outright (ON CONFLICT cannot touch a row twice), and the
	// callers dedupe per flush but nothing forces the next one to.
	_, err := s.pool.Exec(ctx, `
		INSERT INTO stats_users (guild_id, user_id, name, avatar, seen_at)
		SELECT DISTINCT ON (g, u) g, u, n, a, t
		FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::timestamptz[]) AS r(g, u, n, a, t)
		ORDER BY g, u, t DESC
		ON CONFLICT (guild_id, user_id) DO UPDATE SET
			name = EXCLUDED.name, avatar = EXCLUDED.avatar, seen_at = EXCLUDED.seen_at
		WHERE stats_users.seen_at <= EXCLUDED.seen_at
	`, guilds, ids, names, avatars, seen)
	if err != nil {
		return fmt.Errorf("statistics store: upsert users: %w", err)
	}
	return nil
}

func (s *pgStore) UpsertChannels(ctx context.Context, channels []ChannelSeen) error {
	if len(channels) == 0 {
		return nil
	}
	guilds, ids, names := make([]string, len(channels)), make([]string, len(channels)), make([]string, len(channels))
	for i, c := range channels {
		guilds[i], ids[i], names[i] = c.GuildID, c.ChannelID, c.Name
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO stats_channels (guild_id, channel_id, name)
		SELECT DISTINCT ON (g, c) g, c, n
		FROM unnest($1::text[], $2::text[], $3::text[]) AS r(g, c, n)
		ORDER BY g, c, n DESC
		ON CONFLICT (guild_id, channel_id) DO UPDATE SET name = EXCLUDED.name
		WHERE EXCLUDED.name <> ''
	`, guilds, ids, names)
	if err != nil {
		return fmt.Errorf("statistics store: upsert channels: %w", err)
	}
	return nil
}

func (s *pgStore) Config(ctx context.Context, guildID string) (Config, error) {
	cfg := Config{GuildID: guildID, RetentionDays: defaultRetentionDays}
	var live *time.Time
	err := s.pool.QueryRow(ctx, `SELECT retention_days, live_since FROM stats_config WHERE guild_id = $1`, guildID).
		Scan(&cfg.RetentionDays, &live)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return cfg, fmt.Errorf("statistics store: config: %w", err)
	}
	if live != nil {
		cfg.LiveSince = live.UTC()
	}
	return cfg, nil
}

func (s *pgStore) SetRetention(ctx context.Context, guildID string, days int) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO stats_config (guild_id, retention_days) VALUES ($1, $2)
		ON CONFLICT (guild_id) DO UPDATE SET retention_days = EXCLUDED.retention_days
	`, guildID, days)
	if err != nil {
		return fmt.Errorf("statistics store: set retention: %w", err)
	}
	return nil
}

func (s *pgStore) MarkLive(ctx context.Context, guildID string, at time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO stats_config (guild_id, live_since) VALUES ($1, $2)
		ON CONFLICT (guild_id) DO UPDATE SET live_since = COALESCE(stats_config.live_since, EXCLUDED.live_since)
	`, guildID, at)
	if err != nil {
		return fmt.Errorf("statistics store: mark live: %w", err)
	}
	return nil
}

// Prune reads each guild's retention at prune time rather than stamping an
// expiry on every row, the rotation_archives.delete_after lesson: a changed
// retention has to apply to what is already stored, in both directions.
func (s *pgStore) Prune(ctx context.Context, now time.Time) (int64, error) {
	var total int64
	for _, table := range []string{"stats_hourly", "stats_members_hourly"} {
		tag, err := s.pool.Exec(ctx, `
			DELETE FROM `+table+` h
			WHERE h.hour < $1::timestamptz - make_interval(days => COALESCE(
				(SELECT retention_days FROM stats_config c WHERE c.guild_id = h.guild_id), $2::int))
		`, now, defaultRetentionDays)
		if err != nil {
			return total, fmt.Errorf("statistics store: prune %s: %w", table, err)
		}
		total += tag.RowsAffected()
	}
	return total, nil
}

func (s *pgStore) Report(ctx context.Context, guildID, channelID string, from, to time.Time) ([]Row, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT user_id, SUM(messages)::bigint, array_agg(DISTINCT channel_id), MAX(hour)
		FROM stats_hourly
		WHERE guild_id = $1 AND hour >= $2 AND hour < $3 AND ($4::text = '' OR channel_id = $4::text)
		GROUP BY user_id
	`, guildID, from.Truncate(time.Hour), to, channelID)
	if err != nil {
		return nil, fmt.Errorf("statistics store: report: %w", err)
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		var n int64
		if err := rows.Scan(&r.UserID, &n, &r.Channels, &r.Last); err != nil {
			return nil, fmt.Errorf("statistics store: scan report row: %w", err)
		}
		r.Messages = int(n)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *pgStore) Users(ctx context.Context, guildID string, userIDs []string) (map[string]UserSeen, error) {
	rows, err := s.pool.Query(ctx, `SELECT user_id, name, avatar, seen_at FROM stats_users WHERE guild_id = $1 AND user_id = ANY($2)`, guildID, userIDs)
	if err != nil {
		return nil, fmt.Errorf("statistics store: users: %w", err)
	}
	defer rows.Close()
	out := map[string]UserSeen{}
	for rows.Next() {
		u := UserSeen{GuildID: guildID}
		if err := rows.Scan(&u.UserID, &u.Name, &u.Avatar, &u.SeenAt); err != nil {
			return nil, fmt.Errorf("statistics store: scan user: %w", err)
		}
		out[u.UserID] = u
	}
	return out, rows.Err()
}

func (s *pgStore) Channels(ctx context.Context, guildID string) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT channel_id, name FROM stats_channels WHERE guild_id = $1`, guildID)
	if err != nil {
		return nil, fmt.Errorf("statistics store: channels: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("statistics store: scan channel: %w", err)
		}
		out[id] = name
	}
	return out, rows.Err()
}

func (s *pgStore) ChannelTotals(ctx context.Context, guildID string, from, to time.Time) ([]ChannelTotal, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT channel_id, SUM(messages)::bigint, COUNT(DISTINCT user_id)
		FROM stats_hourly WHERE guild_id = $1 AND hour >= $2 AND hour < $3
		GROUP BY channel_id ORDER BY 2 DESC
	`, guildID, from.Truncate(time.Hour), to)
	if err != nil {
		return nil, fmt.Errorf("statistics store: channel totals: %w", err)
	}
	defer rows.Close()
	var out []ChannelTotal
	for rows.Next() {
		var c ChannelTotal
		var n, people int64
		if err := rows.Scan(&c.ChannelID, &n, &people); err != nil {
			return nil, fmt.Errorf("statistics store: scan channel total: %w", err)
		}
		c.Messages, c.People = int(n), int(people)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *pgStore) MemberDays(ctx context.Context, guildID string, from, to time.Time) ([]MemberDay, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT date_trunc('day', hour) AS day, SUM(joined)::bigint, SUM(departed)::bigint
		FROM stats_members_hourly WHERE guild_id = $1 AND hour >= $2 AND hour < $3
		GROUP BY day ORDER BY day
	`, guildID, from.Truncate(time.Hour), to)
	if err != nil {
		return nil, fmt.Errorf("statistics store: member days: %w", err)
	}
	defer rows.Close()
	var out []MemberDay
	for rows.Next() {
		var d MemberDay
		var j, l int64
		if err := rows.Scan(&d.Day, &j, &l); err != nil {
			return nil, fmt.Errorf("statistics store: scan member day: %w", err)
		}
		d.Joined, d.Departed = int(j), int(l)
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *pgStore) OldestHour(ctx context.Context, guildID string) (time.Time, error) {
	var oldest *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT MIN(hour) FROM stats_hourly WHERE guild_id = $1`, guildID).Scan(&oldest); err != nil {
		return time.Time{}, fmt.Errorf("statistics store: oldest hour: %w", err)
	}
	if oldest == nil {
		return time.Time{}, nil
	}
	return oldest.UTC(), nil
}

func (s *pgStore) RequestBackfill(ctx context.Context, rows []BackfillRow) error {
	if len(rows) == 0 {
		return nil
	}
	guilds, channels := make([]string, len(rows)), make([]string, len(rows))
	from, until := make([]time.Time, len(rows)), make([]time.Time, len(rows))
	for i, r := range rows {
		guilds[i], channels[i], from[i], until[i] = r.GuildID, r.ChannelID, r.From, r.Until
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO stats_backfill (guild_id, channel_id, from_at, until_at)
		SELECT * FROM unnest($1::text[], $2::text[], $3::timestamptz[], $4::timestamptz[])
		ON CONFLICT (guild_id, channel_id) DO UPDATE SET
			from_at = EXCLUDED.from_at, until_at = EXCLUDED.until_at, cursor = '', done = false, error = ''
	`, guilds, channels, from, until)
	if err != nil {
		return fmt.Errorf("statistics store: request backfill: %w", err)
	}
	return nil
}

func (s *pgStore) PendingBackfill(ctx context.Context, guildID string) ([]BackfillRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT channel_id, from_at, until_at, cursor FROM stats_backfill
		WHERE guild_id = $1 AND NOT done ORDER BY channel_id
	`, guildID)
	if err != nil {
		return nil, fmt.Errorf("statistics store: pending backfill: %w", err)
	}
	defer rows.Close()
	var out []BackfillRow
	for rows.Next() {
		r := BackfillRow{GuildID: guildID}
		if err := rows.Scan(&r.ChannelID, &r.From, &r.Until, &r.Cursor); err != nil {
			return nil, fmt.Errorf("statistics store: scan backfill: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *pgStore) SetBackfillCursor(ctx context.Context, guildID, channelID, cursor string, done bool, errText string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE stats_backfill SET cursor = $3, done = $4, error = $5 WHERE guild_id = $1 AND channel_id = $2
	`, guildID, channelID, cursor, done, errText)
	if err != nil {
		return fmt.Errorf("statistics store: set backfill cursor: %w", err)
	}
	return nil
}

func (s *pgStore) BackfillStatus(ctx context.Context, guildID string) (BackfillSummary, error) {
	var sum BackfillSummary
	var from *time.Time
	var pending, done, failed int64
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE NOT done), COUNT(*) FILTER (WHERE done AND error = ''),
		       COUNT(*) FILTER (WHERE done AND error <> ''), MIN(from_at)
		FROM stats_backfill WHERE guild_id = $1
	`, guildID).Scan(&pending, &done, &failed, &from)
	if err != nil {
		return sum, fmt.Errorf("statistics store: backfill status: %w", err)
	}
	sum.Pending, sum.Done, sum.Failed = int(pending), int(done), int(failed)
	if from != nil {
		sum.From = from.UTC()
	}
	return sum, nil
}

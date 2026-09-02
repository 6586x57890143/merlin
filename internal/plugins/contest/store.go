// Package contest runs server contests: an announce window, a submission
// window, a voting window, and a results announcement, with a prize ledger
// hanging off the side.
//
// Two decisions shape everything else here.
//
// Submissions arrive through a Discord forum channel rather than a slash
// command or a web upload. Members already know how to drag a file into a
// forum post, Discord hosts the bytes on its own CDN for free, and merlin
// never stores or proxies a single one. The plugin reads a post's starter
// message over REST, on demand, one message at a time, which is strictly
// narrower than aimod's live firehose and needs no gateway intent.
//
// Voting happens on a Cloudflare Worker, because browsing forty pieces of
// art and picking a favourite is the one part of this Discord is genuinely
// bad at. merlin stays outbound-only (spec.MD §4): it pushes a full snapshot
// of the contest to the Worker and asks for a tally at close. There is no
// inbound HTTP surface on this bot, no polling loop, and nothing the Worker
// can make merlin do. The Worker sees an HMAC of each Discord ID and never
// the ID itself, so the vote ledger holds no Discord identities at all.
package contest

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Phase is where a contest is in its life. The order is fixed and the
// transitions are one-way; cancelled is reachable from any of the first
// three and is terminal.
type Phase string

const (
	PhaseAnnounce  Phase = "announce"
	PhaseSubmit    Phase = "submit"
	PhaseVote      Phase = "vote"
	PhaseResults   Phase = "results"
	PhaseCancelled Phase = "cancelled"
)

// ErrNoLiveContest reports that a guild has nothing running. Returned rather
// than an empty record so callers cannot mistake the zero value for a real
// contest that happens to have an empty title.
var ErrNoLiveContest = errors.New("contest: this server has no contest running")

// ErrAlreadyLive reports that a guild already has a contest that has not
// reached results yet. One at a time, deliberately: two overlapping contests
// would share the announce channel, the /contest link command and the prize
// pool, and every one of those would need a disambiguating argument that
// nobody wants to type.
var ErrAlreadyLive = errors.New("contest: this server already has a contest running")

// Config is the per-guild setup. Deliberately small: everything that varies
// per contest is on Contest itself, and everything that varies per
// deployment (the Worker URL and its keys) is env, since one deployment
// means one Worker.
type Config struct {
	GuildID           string
	AnnounceChannelID string
	ForumCategoryID   string
	DefaultMaxVotes   int
}

// Contest is one contest. SubmitAt/VoteAt/ResultsAt are the instants each
// phase ends, not durations, so a restart or a missed tick never drifts a
// deadline the way re-deriving from elapsed time would (the same reason
// core.CalendarSchedule exists).
type Contest struct {
	ID                string
	GuildID           string
	Slug              string
	Title             string
	Theme             string
	Phase             Phase
	SubmitAt          time.Time
	VoteAt            time.Time
	ResultsAt         time.Time
	MaxVotes          int
	ForumChannelID    string
	AnnounceChannelID string
	AnnounceMessageID string
	CreatedBy         string
	CreatedAt         time.Time
	ClosedAt          *time.Time
	Results           []byte // JSON tally as merlin computed it, nil until close
	TallyError        string
}

// Deadline returns when the current phase ends, and whether the phase has
// one at all (results and cancelled do not).
func (c Contest) Deadline() (time.Time, bool) {
	switch c.Phase {
	case PhaseAnnounce:
		return c.SubmitAt, true
	case PhaseSubmit:
		return c.VoteAt, true
	case PhaseVote:
		return c.ResultsAt, true
	default:
		return time.Time{}, false
	}
}

// Live reports whether this contest still needs a scheduler tick.
func (c Contest) Live() bool {
	return c.Phase == PhaseAnnounce || c.Phase == PhaseSubmit || c.Phase == PhaseVote
}

// Submission is one forum post that counts as an entry. MediaURL is a cache,
// not a record: Discord's signed CDN links expire in about a day, and the
// refresh path is to re-read the thread's starter message, which is what the
// tick does during vote and results.
type Submission struct {
	ID        string
	ContestID string
	UserID    string
	ThreadID  string
	Title     string
	Author    string
	Kind      string // image | audio | video | text | link | other
	// MediaURLs is every attachment on the post, in the order they were
	// posted. MediaURL is the first of them, kept because the previous
	// release reads that column and a rollback should still render art.
	MediaURLs   []string
	MediaURL    string
	Link        string
	Body        string
	CreatedAt   time.Time
	WithdrawnAt *time.Time
}

// Prize is one pledge. SecretSealed is the only field in this package that
// is never logged, never audited, and never pushed to the Worker.
type Prize struct {
	ID           string
	ContestID    string
	DonorID      string
	DonorName    string
	Title        string
	Details      string
	SecretSealed []byte
	AwardedTo    *string
	AwardedAt    *time.Time
	CreatedAt    time.Time
}

// HasSecret reports whether a prize carries a code to deliver, without
// anything having to touch the ciphertext to find out.
func (p Prize) HasSecret() bool { return len(p.SecretSealed) > 0 }

// Store is the narrow slice of Postgres this plugin needs. Declared here
// rather than importing a concrete type so tests run against an in-memory
// fake, the same seam rotation.ArchiveStore and roles.Store use.
type Store interface {
	GetConfig(ctx context.Context, guildID string) (Config, error)
	SetConfig(ctx context.Context, cfg Config) error

	CreateContest(ctx context.Context, c Contest) error
	LiveContest(ctx context.Context, guildID string) (Contest, error)
	LatestContest(ctx context.Context, guildID string) (Contest, error)
	GuildsWithLiveContests(ctx context.Context) ([]string, error)
	// AdvancePhase is conditional on the contest still being in `from`, so
	// two overlapping ticks cannot both announce the same transition. It
	// reports whether this caller is the one that won.
	AdvancePhase(ctx context.Context, contestID string, from, to Phase) (bool, error)
	SetForumChannel(ctx context.Context, contestID, channelID string) error
	SetAnnounceMessage(ctx context.Context, contestID, channelID, messageID string) error
	SetResults(ctx context.Context, contestID string, results []byte, closedAt time.Time) error
	SetTallyError(ctx context.Context, contestID, msg string) error

	UpsertSubmission(ctx context.Context, s Submission) error
	Submissions(ctx context.Context, contestID string) ([]Submission, error)
	WithdrawMissing(ctx context.Context, contestID string, liveThreadIDs []string, at time.Time) error

	AddPrize(ctx context.Context, p Prize) error
	Prizes(ctx context.Context, contestID string) ([]Prize, error)
	// PrizesAwardedTo is guild-scoped rather than contest-scoped because
	// /contest claim is the recovery path for a prize DM that bounced, and a
	// bounced DM is the common case. Scoped to the latest contest it went
	// unreachable the moment a mod started the next one, stranding the
	// ciphertext for good.
	PrizesAwardedTo(ctx context.Context, guildID, userID string) ([]Prize, error)
	RemovePrize(ctx context.Context, contestID, prizeID, donorID string) (bool, error)
	MarkPrizeAwarded(ctx context.Context, prizeID, winnerID string, at time.Time) error
	// ClearPrizeSecret wipes the ciphertext once it has been delivered. A
	// separate call from MarkPrizeAwarded on purpose: a failed DM must leave
	// the secret in place to retry, and folding the two together would make
	// the safe ordering impossible to express.
	ClearPrizeSecret(ctx context.Context, prizeID string) error
}

type pgStore struct{ pool *pgxpool.Pool }

// NewPostgresStore returns the production Store.
func NewPostgresStore(pool *pgxpool.Pool) Store { return &pgStore{pool: pool} }

const contestCols = `id, guild_id, slug, title, theme, phase, submit_at, vote_at, results_at,
	max_votes, forum_channel_id, announce_channel_id, announce_message_id,
	created_by, created_at, closed_at, results, tally_error`

func scanContest(row pgx.Row) (Contest, error) {
	var c Contest
	err := row.Scan(&c.ID, &c.GuildID, &c.Slug, &c.Title, &c.Theme, &c.Phase,
		&c.SubmitAt, &c.VoteAt, &c.ResultsAt, &c.MaxVotes, &c.ForumChannelID,
		&c.AnnounceChannelID, &c.AnnounceMessageID, &c.CreatedBy, &c.CreatedAt,
		&c.ClosedAt, &c.Results, &c.TallyError)
	return c, err
}

func (s *pgStore) GetConfig(ctx context.Context, guildID string) (Config, error) {
	cfg := Config{GuildID: guildID, DefaultMaxVotes: 3}
	err := s.pool.QueryRow(ctx, `
		SELECT announce_channel_id, forum_category_id, default_max_votes
		FROM contest_config WHERE guild_id = $1
	`, guildID).Scan(&cfg.AnnounceChannelID, &cfg.ForumCategoryID, &cfg.DefaultMaxVotes)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// An unconfigured guild is not an error. /contest new falls back
			// to the invoking channel, which is what a mod running it in the
			// channel they want the announcement in already meant.
			return Config{GuildID: guildID, DefaultMaxVotes: 3}, nil
		}
		return Config{}, fmt.Errorf("contest store: get config: %w", err)
	}
	return cfg, nil
}

func (s *pgStore) SetConfig(ctx context.Context, cfg Config) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO contest_config (guild_id, announce_channel_id, forum_category_id, default_max_votes, updated_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (guild_id) DO UPDATE SET
			announce_channel_id = EXCLUDED.announce_channel_id,
			forum_category_id   = EXCLUDED.forum_category_id,
			default_max_votes   = EXCLUDED.default_max_votes,
			updated_at          = now()
	`, cfg.GuildID, cfg.AnnounceChannelID, cfg.ForumCategoryID, cfg.DefaultMaxVotes)
	if err != nil {
		return fmt.Errorf("contest store: set config: %w", err)
	}
	return nil
}

func (s *pgStore) CreateContest(ctx context.Context, c Contest) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO contests (id, guild_id, slug, title, theme, phase,
			submit_at, vote_at, results_at, max_votes, announce_channel_id, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
	`, c.ID, c.GuildID, c.Slug, c.Title, c.Theme, c.Phase,
		c.SubmitAt, c.VoteAt, c.ResultsAt, c.MaxVotes, c.AnnounceChannelID, c.CreatedBy)
	if err != nil {
		return fmt.Errorf("contest store: create contest: %w", err)
	}
	return nil
}

func (s *pgStore) LiveContest(ctx context.Context, guildID string) (Contest, error) {
	c, err := scanContest(s.pool.QueryRow(ctx, `
		SELECT `+contestCols+` FROM contests
		WHERE guild_id = $1 AND phase IN ('announce','submit','vote')
		ORDER BY created_at DESC LIMIT 1
	`, guildID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Contest{}, ErrNoLiveContest
		}
		return Contest{}, fmt.Errorf("contest store: live contest: %w", err)
	}
	return c, nil
}

func (s *pgStore) LatestContest(ctx context.Context, guildID string) (Contest, error) {
	c, err := scanContest(s.pool.QueryRow(ctx, `
		SELECT `+contestCols+` FROM contests
		WHERE guild_id = $1 AND phase <> 'cancelled'
		ORDER BY created_at DESC LIMIT 1
	`, guildID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Contest{}, ErrNoLiveContest
		}
		return Contest{}, fmt.Errorf("contest store: latest contest: %w", err)
	}
	return c, nil
}

func (s *pgStore) GuildsWithLiveContests(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT guild_id FROM contests WHERE phase IN ('announce','submit','vote')
	`)
	if err != nil {
		return nil, fmt.Errorf("contest store: guilds with live contests: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, fmt.Errorf("contest store: scan guild: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *pgStore) AdvancePhase(ctx context.Context, contestID string, from, to Phase) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE contests SET phase = $3 WHERE id = $1 AND phase = $2
	`, contestID, from, to)
	if err != nil {
		return false, fmt.Errorf("contest store: advance phase: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s *pgStore) SetForumChannel(ctx context.Context, contestID, channelID string) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE contests SET forum_channel_id = $2 WHERE id = $1`, contestID, channelID); err != nil {
		return fmt.Errorf("contest store: set forum channel: %w", err)
	}
	return nil
}

func (s *pgStore) SetAnnounceMessage(ctx context.Context, contestID, channelID, messageID string) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE contests SET announce_channel_id = $2, announce_message_id = $3 WHERE id = $1`,
		contestID, channelID, messageID); err != nil {
		return fmt.Errorf("contest store: set announce message: %w", err)
	}
	return nil
}

func (s *pgStore) SetResults(ctx context.Context, contestID string, results []byte, closedAt time.Time) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE contests SET results = $2, closed_at = $3, tally_error = '' WHERE id = $1`,
		contestID, results, closedAt); err != nil {
		return fmt.Errorf("contest store: set results: %w", err)
	}
	return nil
}

func (s *pgStore) SetTallyError(ctx context.Context, contestID, msg string) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE contests SET tally_error = $2 WHERE id = $1`, contestID, msg); err != nil {
		return fmt.Errorf("contest store: set tally error: %w", err)
	}
	return nil
}

func (s *pgStore) UpsertSubmission(ctx context.Context, sub Submission) error {
	// pgx encodes a nil slice as NULL, and the column is NOT NULL. An entry
	// with no attachments is the ordinary text-only case, not an error.
	urls := sub.MediaURLs
	if urls == nil {
		urls = []string{}
	}
	// Keyed on thread_id, not (contest, user): the forum post is the entry,
	// and re-reading it on every tick is how a stale CDN link gets refreshed
	// and an edited title gets picked up. The one-live-entry-per-member rule
	// is the partial unique index, enforced by the database rather than by
	// this statement, so a member who posts twice hits a constraint here and
	// gets told which post counts instead of silently replacing the first.
	_, err := s.pool.Exec(ctx, `
		INSERT INTO contest_submissions
			(id, contest_id, user_id, thread_id, title, author, kind, media_url, media_urls, link, body)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (thread_id) DO UPDATE SET
			title = EXCLUDED.title, author = EXCLUDED.author, kind = EXCLUDED.kind,
			media_url = EXCLUDED.media_url, media_urls = EXCLUDED.media_urls,
			link = EXCLUDED.link, body = EXCLUDED.body,
			withdrawn_at = NULL
	`, sub.ID, sub.ContestID, sub.UserID, sub.ThreadID, sub.Title, sub.Author,
		sub.Kind, sub.MediaURL, urls, sub.Link, sub.Body)
	if err != nil {
		return fmt.Errorf("contest store: upsert submission: %w", err)
	}
	return nil
}

func (s *pgStore) Submissions(ctx context.Context, contestID string) ([]Submission, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, contest_id, user_id, thread_id, title, author, kind, media_url, media_urls, link, body, created_at, withdrawn_at
		FROM contest_submissions
		WHERE contest_id = $1 AND withdrawn_at IS NULL
		ORDER BY created_at
	`, contestID)
	if err != nil {
		return nil, fmt.Errorf("contest store: submissions: %w", err)
	}
	defer rows.Close()
	var out []Submission
	for rows.Next() {
		var sub Submission
		if err := rows.Scan(&sub.ID, &sub.ContestID, &sub.UserID, &sub.ThreadID, &sub.Title,
			&sub.Author, &sub.Kind, &sub.MediaURL, &sub.MediaURLs, &sub.Link, &sub.Body,
			&sub.CreatedAt, &sub.WithdrawnAt); err != nil {
			return nil, fmt.Errorf("contest store: scan submission: %w", err)
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

func (s *pgStore) WithdrawMissing(ctx context.Context, contestID string, liveThreadIDs []string, at time.Time) error {
	// Deleting your forum post is how you withdraw, so the live thread list
	// is authoritative and anything not in it is gone. This is the same
	// re-derive-from-live-state rule rotation and roles follow, and it is why
	// there is no /contest withdraw command to keep in sync with it.
	if liveThreadIDs == nil {
		liveThreadIDs = []string{}
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE contest_submissions SET withdrawn_at = $3
		WHERE contest_id = $1 AND withdrawn_at IS NULL AND thread_id <> ALL($2)
	`, contestID, liveThreadIDs, at)
	if err != nil {
		return fmt.Errorf("contest store: withdraw missing: %w", err)
	}
	return nil
}

func (s *pgStore) AddPrize(ctx context.Context, p Prize) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO contest_prizes (id, contest_id, donor_id, donor_name, title, details, secret_sealed)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
	`, p.ID, p.ContestID, p.DonorID, p.DonorName, p.Title, p.Details, p.SecretSealed)
	if err != nil {
		return fmt.Errorf("contest store: add prize: %w", err)
	}
	return nil
}

func (s *pgStore) Prizes(ctx context.Context, contestID string) ([]Prize, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, contest_id, donor_id, donor_name, title, details, secret_sealed, awarded_to, awarded_at, created_at
		FROM contest_prizes WHERE contest_id = $1 ORDER BY created_at
	`, contestID)
	if err != nil {
		return nil, fmt.Errorf("contest store: prizes: %w", err)
	}
	defer rows.Close()
	var out []Prize
	for rows.Next() {
		var p Prize
		if err := rows.Scan(&p.ID, &p.ContestID, &p.DonorID, &p.DonorName, &p.Title,
			&p.Details, &p.SecretSealed, &p.AwardedTo, &p.AwardedAt, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("contest store: scan prize: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *pgStore) PrizesAwardedTo(ctx context.Context, guildID, userID string) ([]Prize, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.id, p.contest_id, p.donor_id, p.donor_name, p.title, p.details,
			p.secret_sealed, p.awarded_to, p.awarded_at, p.created_at
		FROM contest_prizes p JOIN contests c ON c.id = p.contest_id
		WHERE c.guild_id = $1 AND p.awarded_to = $2
		ORDER BY p.awarded_at DESC
	`, guildID, userID)
	if err != nil {
		return nil, fmt.Errorf("contest store: prizes awarded to: %w", err)
	}
	defer rows.Close()
	var out []Prize
	for rows.Next() {
		var p Prize
		if err := rows.Scan(&p.ID, &p.ContestID, &p.DonorID, &p.DonorName, &p.Title,
			&p.Details, &p.SecretSealed, &p.AwardedTo, &p.AwardedAt, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("contest store: scan awarded prize: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *pgStore) RemovePrize(ctx context.Context, contestID, prizeID, donorID string) (bool, error) {
	// donor_id in the WHERE clause, not checked by the caller: a donor may
	// only pull their own pledge, and expressing that as part of the delete
	// means there is no window between the check and the write.
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM contest_prizes
		WHERE id = $1 AND contest_id = $2 AND donor_id = $3 AND awarded_at IS NULL
	`, prizeID, contestID, donorID)
	if err != nil {
		return false, fmt.Errorf("contest store: remove prize: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s *pgStore) MarkPrizeAwarded(ctx context.Context, prizeID, winnerID string, at time.Time) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE contest_prizes SET awarded_to = $2, awarded_at = $3 WHERE id = $1`,
		prizeID, winnerID, at); err != nil {
		return fmt.Errorf("contest store: mark prize awarded: %w", err)
	}
	return nil
}

func (s *pgStore) ClearPrizeSecret(ctx context.Context, prizeID string) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE contest_prizes SET secret_sealed = NULL WHERE id = $1`, prizeID); err != nil {
		return fmt.Errorf("contest store: clear prize secret: %w", err)
	}
	return nil
}

package rapsheet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Kind is what an entry records.
type Kind string

const (
	// KindNote is a moderator's observation. No points, no DM, never
	// escalates: it is the one kind that exists purely for the next mod who
	// opens the sheet.
	KindNote Kind = "note"
	KindWarn Kind = "warn"
	// KindRemoval is an aimod removal or rewrite: the offence itself, which
	// carries the points. The jail that may follow arrives as its own
	// KindJail row at zero points.
	KindRemoval Kind = "removal"
	KindJail    Kind = "jail"
	KindTimeout Kind = "timeout"
	KindKick    Kind = "kick"
	KindBan     Kind = "ban"
	KindUnban   Kind = "unban"
	KindRelease Kind = "release"
	// KindSuggestion is the ladder recording that it recommended something,
	// so a suggestion is on the sheet whether or not a mod acted on it, and
	// so the same band is not suggested again.
	KindSuggestion Kind = "suggestion"
)

// Source is who wrote the row.
type Source string

const (
	SourceCommand Source = "command"
	SourceLadder  Source = "ladder"
	SourceAIMod   Source = "aimod"
	SourceRoles   Source = "roles"
	SourceDiscord Source = "discord"
)

// Mode is the guild's escalation_mode.
type Mode string

const (
	ModeOff     Mode = "off"
	ModeSuggest Mode = "suggest"
	ModeAuto    Mode = "auto"
)

// Entry is one line on a rapsheet.
type Entry struct {
	ID       int64
	GuildID  string
	UserID   string
	Kind     Kind
	Category Category
	Points   int
	Band     int
	ActorID  string
	Reason   string
	// Duration is the sentence as decided; zero where there is none or the
	// ban is permanent (EndsAt nil on a KindBan says which).
	Duration time.Duration
	EndsAt   *time.Time
	Source   Source
	Ref      string
	LiftedAt *time.Time
	VoidedAt *time.Time
	VoidedBy string
	// VoidReason is the moderator's reason for voiding, or for a ladder row
	// that could not be applied, the error that stopped it.
	VoidReason      string
	ThreadMessageID string
	CreatedAt       time.Time
}

// Voided is the one-word form of the check every reader makes.
func (e Entry) Voided() bool { return e.VoidedAt != nil }

// Standing reports whether e is a consequence still in force at now: an
// active timeout or jail, a ban not yet lifted.
func (e Entry) Standing(now time.Time) bool {
	if e.Voided() {
		return false
	}
	switch e.Kind {
	case KindBan:
		return e.LiftedAt == nil && (e.EndsAt == nil || e.EndsAt.After(now))
	case KindJail, KindTimeout:
		return e.EndsAt != nil && e.EndsAt.After(now)
	}
	return false
}

// Config is one guild's rapsheet configuration.
type Config struct {
	GuildID        string
	EscalationMode Mode
	HalfLife       time.Duration
	// CategoryPoints holds overrides only; see pointsFor for the defaults.
	CategoryPoints map[Category]int
	// Bands is the guild's ladder; empty means defaultBands.
	Bands          []Band
	ModChannelID   string
	ForumChannelID string
	AltHints       bool
}

// defaultHalfLife is thirty days. Long enough that a pattern over a month
// reads as a pattern, short enough that one bad week in spring is not still
// deciding sentences in autumn.
const defaultHalfLife = 30 * 24 * time.Hour

func defaultConfig(guildID string) Config {
	return Config{
		GuildID:        guildID,
		EscalationMode: ModeSuggest,
		HalfLife:       defaultHalfLife,
		CategoryPoints: map[Category]int{},
		AltHints:       true,
	}
}

// CaseFile is one member on file.
type CaseFile struct {
	GuildID    string
	UserID     string
	ThreadID   string
	Username   string
	GlobalName string
	AvatarHash string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Link is one account's membership of a group.
type Link struct {
	GuildID   string
	UserID    string
	GroupID   string
	LinkedBy  string
	Reason    string
	CreatedAt time.Time
}

// AltHint is what the join-time heuristics thought of one pair.
type AltHint struct {
	GuildID     string
	UserID      string
	CandidateID string
	Signals     []string
	Score       int
	Opinion     string
	CreatedAt   time.Time
}

// ErrNoEntry is returned for a case id that does not exist in this guild.
// The guild is part of the lookup so a case number from another server is
// unreachable, not merely unlikely to be typed.
var ErrNoEntry = errors.New("rapsheet: no such case")

// Store is the plugin's persistence. Postgres in production, an in-memory
// fake in tests.
type Store interface {
	Config(ctx context.Context, guildID string) (Config, error)
	SetConfig(ctx context.Context, cfg Config) error

	// Insert writes an entry and returns its id. An entry whose (guild,
	// source, ref) is already present is not written and 0 is returned with
	// a nil error: that is the ingestion path seeing an event twice, which
	// is ordinary and not a failure.
	Insert(ctx context.Context, e Entry) (int64, error)
	Entry(ctx context.Context, guildID string, id int64) (Entry, error)
	// Entries lists the entries of every user in userIDs (a link group)
	// newer than since, newest first, at most limit. Voided entries are
	// included; readers that want the live record check Voided.
	Entries(ctx context.Context, guildID string, userIDs []string, since time.Time, limit int) ([]Entry, error)
	// EntryByRef finds the entry an external event produced.
	EntryByRef(ctx context.Context, guildID string, source Source, ref string) (Entry, bool, error)
	UpdateReason(ctx context.Context, guildID string, id int64, reason string) error
	Void(ctx context.Context, guildID string, id int64, by, reason string, at time.Time) error
	SetThreadMessage(ctx context.Context, id int64, messageID string) error
	CountUnmirrored(ctx context.Context, guildID string) (int, error)
	// CountScored counts the scored offences (points > 0, not voided) a
	// member has since `since`. What aimod's ladder reads as priors.
	CountScored(ctx context.Context, guildID string, userIDs []string, since time.Time) (int, error)

	// DueBans are unlifted, unvoided temporary bans whose end has passed.
	DueBans(ctx context.Context, guildID string, now time.Time) ([]Entry, error)
	CountPendingBans(ctx context.Context, guildID string) (int, error)
	MarkLifted(ctx context.Context, id int64, at time.Time) error
	// ActiveBan is the member's standing ban, if any.
	ActiveBan(ctx context.Context, guildID, userID string) (Entry, bool, error)

	CaseFile(ctx context.Context, guildID, userID string) (CaseFile, bool, error)
	// UpsertCaseFile creates the row or refreshes its identity snapshot;
	// thread_id is left alone.
	UpsertCaseFile(ctx context.Context, cf CaseFile) error
	SetCaseThread(ctx context.Context, guildID, userID, threadID string) error
	// CaseFiles lists members on file, newest first, for alt matching.
	CaseFiles(ctx context.Context, guildID string, limit int) ([]CaseFile, error)

	Link(ctx context.Context, l Link) error
	Unlink(ctx context.Context, guildID, userID string) error
	// Group returns every account linked with userID, including userID
	// itself, so callers never special-case the unlinked member.
	Group(ctx context.Context, guildID, userID string) ([]string, error)
	GroupLinks(ctx context.Context, guildID, userID string) ([]Link, error)

	UpsertHint(ctx context.Context, h AltHint) error
	// Hints returns hints naming userID on either side.
	Hints(ctx context.Context, guildID, userID string) ([]AltHint, error)
	DeleteHint(ctx context.Context, guildID, userID, candidateID string) error
}

type pgStore struct{ pool *pgxpool.Pool }

// NewPostgresStore returns the production Store.
func NewPostgresStore(pool *pgxpool.Pool) Store { return &pgStore{pool: pool} }

// bandJSON is the on-disk shape of a Band. Seconds rather than a Go
// duration so the column is readable by hand and by anything that is not
// this binary.
type bandJSON struct {
	Min          float64 `json:"min"`
	Action       Action  `json:"action"`
	DurationSecs int64   `json:"duration_secs,omitempty"`
}

func bandsToJSON(bands []Band) ([]byte, error) {
	out := make([]bandJSON, 0, len(bands))
	for _, b := range bands {
		out = append(out, bandJSON{Min: b.Min, Action: b.Action, DurationSecs: int64(b.Duration / time.Second)})
	}
	return json.Marshal(out)
}

func bandsFromJSON(raw []byte) ([]Band, error) {
	var in []bandJSON
	if len(raw) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	out := make([]Band, 0, len(in))
	for _, b := range in {
		out = append(out, Band{Min: b.Min, Action: b.Action, Duration: time.Duration(b.DurationSecs) * time.Second})
	}
	return out, nil
}

func (s *pgStore) Config(ctx context.Context, guildID string) (Config, error) {
	cfg := defaultConfig(guildID)
	var (
		halfLifeHours int
		points        []byte
		bands         []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT escalation_mode, half_life_hours, category_points, bands,
		       mod_channel_id, forum_channel_id, alt_hints
		FROM rapsheet_config WHERE guild_id = $1
	`, guildID).Scan(&cfg.EscalationMode, &halfLifeHours, &points, &bands,
		&cfg.ModChannelID, &cfg.ForumChannelID, &cfg.AltHints)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return cfg, nil
		}
		return Config{}, fmt.Errorf("rapsheet store: config: %w", err)
	}
	cfg.HalfLife = time.Duration(halfLifeHours) * time.Hour
	if len(points) > 0 {
		if err := json.Unmarshal(points, &cfg.CategoryPoints); err != nil {
			return Config{}, fmt.Errorf("rapsheet store: config points: %w", err)
		}
	}
	if cfg.CategoryPoints == nil {
		cfg.CategoryPoints = map[Category]int{}
	}
	if cfg.Bands, err = bandsFromJSON(bands); err != nil {
		return Config{}, fmt.Errorf("rapsheet store: config bands: %w", err)
	}
	return cfg, nil
}

func (s *pgStore) SetConfig(ctx context.Context, cfg Config) error {
	points, err := json.Marshal(cfg.CategoryPoints)
	if err != nil {
		return fmt.Errorf("rapsheet store: set config: %w", err)
	}
	bands, err := bandsToJSON(cfg.Bands)
	if err != nil {
		return fmt.Errorf("rapsheet store: set config: %w", err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO rapsheet_config (guild_id, escalation_mode, half_life_hours, category_points, bands,
		                             mod_channel_id, forum_channel_id, alt_hints, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())
		ON CONFLICT (guild_id) DO UPDATE SET
			escalation_mode  = EXCLUDED.escalation_mode,
			half_life_hours  = EXCLUDED.half_life_hours,
			category_points  = EXCLUDED.category_points,
			bands            = EXCLUDED.bands,
			mod_channel_id   = EXCLUDED.mod_channel_id,
			forum_channel_id = EXCLUDED.forum_channel_id,
			alt_hints        = EXCLUDED.alt_hints,
			updated_at       = now()
	`, cfg.GuildID, cfg.EscalationMode, int(cfg.HalfLife/time.Hour), points, bands,
		cfg.ModChannelID, cfg.ForumChannelID, cfg.AltHints)
	if err != nil {
		return fmt.Errorf("rapsheet store: set config: %w", err)
	}
	return nil
}

const entryCols = `id, guild_id, user_id, kind, category, points, band, actor_id, reason,
	duration_secs, ends_at, source, ref, lifted_at, voided_at, voided_by, void_reason,
	thread_message_id, created_at`

func scanEntry(row pgx.Row) (Entry, error) {
	var (
		e    Entry
		secs *int64
	)
	err := row.Scan(&e.ID, &e.GuildID, &e.UserID, &e.Kind, &e.Category, &e.Points, &e.Band,
		&e.ActorID, &e.Reason, &secs, &e.EndsAt, &e.Source, &e.Ref, &e.LiftedAt,
		&e.VoidedAt, &e.VoidedBy, &e.VoidReason, &e.ThreadMessageID, &e.CreatedAt)
	if secs != nil {
		e.Duration = time.Duration(*secs) * time.Second
	}
	return e, err
}

// normalizeEntry fills what the schema requires and a caller may leave
// blank. The CHECK on category would otherwise refuse a note, which carries
// no category of its own.
func normalizeEntry(e Entry) Entry {
	if e.Category == "" {
		e.Category = CategoryOther
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	return e
}

func (s *pgStore) Insert(ctx context.Context, e Entry) (int64, error) {
	e = normalizeEntry(e)
	var secs *int64
	if e.Duration > 0 {
		v := int64(e.Duration / time.Second)
		secs = &v
	}
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO rapsheet_entries (guild_id, user_id, kind, category, points, band, actor_id, reason,
		                              duration_secs, ends_at, source, ref, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (guild_id, source, ref) WHERE ref <> '' DO NOTHING
		RETURNING id
	`, e.GuildID, e.UserID, e.Kind, e.Category, e.Points, e.Band, e.ActorID, e.Reason,
		secs, e.EndsAt, e.Source, e.Ref, e.CreatedAt).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// DO NOTHING fired: already recorded.
			return 0, nil
		}
		return 0, fmt.Errorf("rapsheet store: insert: %w", err)
	}
	return id, nil
}

func (s *pgStore) Entry(ctx context.Context, guildID string, id int64) (Entry, error) {
	e, err := scanEntry(s.pool.QueryRow(ctx,
		`SELECT `+entryCols+` FROM rapsheet_entries WHERE guild_id = $1 AND id = $2`, guildID, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Entry{}, ErrNoEntry
		}
		return Entry{}, fmt.Errorf("rapsheet store: entry: %w", err)
	}
	return e, nil
}

func (s *pgStore) Entries(ctx context.Context, guildID string, userIDs []string, since time.Time, limit int) ([]Entry, error) {
	if len(userIDs) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+entryCols+` FROM rapsheet_entries
		WHERE guild_id = $1 AND user_id = ANY($2) AND created_at > $3
		ORDER BY created_at DESC, id DESC
		LIMIT $4
	`, guildID, userIDs, since, limit)
	if err != nil {
		return nil, fmt.Errorf("rapsheet store: entries: %w", err)
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("rapsheet store: entries: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *pgStore) EntryByRef(ctx context.Context, guildID string, source Source, ref string) (Entry, bool, error) {
	e, err := scanEntry(s.pool.QueryRow(ctx,
		`SELECT `+entryCols+` FROM rapsheet_entries WHERE guild_id = $1 AND source = $2 AND ref = $3 AND ref <> ''`,
		guildID, source, ref))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Entry{}, false, nil
		}
		return Entry{}, false, fmt.Errorf("rapsheet store: entry by ref: %w", err)
	}
	return e, true, nil
}

func (s *pgStore) UpdateReason(ctx context.Context, guildID string, id int64, reason string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE rapsheet_entries SET reason = $3 WHERE guild_id = $1 AND id = $2`, guildID, id, reason)
	if err != nil {
		return fmt.Errorf("rapsheet store: update reason: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoEntry
	}
	return nil
}

func (s *pgStore) Void(ctx context.Context, guildID string, id int64, by, reason string, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE rapsheet_entries SET voided_at = $3, voided_by = $4, void_reason = $5
		WHERE guild_id = $1 AND id = $2 AND voided_at IS NULL
	`, guildID, id, at, by, reason)
	if err != nil {
		return fmt.Errorf("rapsheet store: void: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoEntry
	}
	return nil
}

func (s *pgStore) SetThreadMessage(ctx context.Context, id int64, messageID string) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE rapsheet_entries SET thread_message_id = $2 WHERE id = $1`, id, messageID); err != nil {
		return fmt.Errorf("rapsheet store: set thread message: %w", err)
	}
	return nil
}

func (s *pgStore) CountUnmirrored(ctx context.Context, guildID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM rapsheet_entries WHERE guild_id = $1 AND thread_message_id = ''`, guildID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("rapsheet store: count unmirrored: %w", err)
	}
	return n, nil
}

func (s *pgStore) CountScored(ctx context.Context, guildID string, userIDs []string, since time.Time) (int, error) {
	if len(userIDs) == 0 {
		return 0, nil
	}
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM rapsheet_entries
		WHERE guild_id = $1 AND user_id = ANY($2) AND created_at > $3 AND points > 0 AND voided_at IS NULL
	`, guildID, userIDs, since).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("rapsheet store: count scored: %w", err)
	}
	return n, nil
}

const banDue = `kind = 'ban' AND ends_at IS NOT NULL AND lifted_at IS NULL AND voided_at IS NULL`

func (s *pgStore) DueBans(ctx context.Context, guildID string, now time.Time) ([]Entry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+entryCols+` FROM rapsheet_entries
		WHERE guild_id = $1 AND `+banDue+` AND ends_at <= $2
		ORDER BY ends_at
	`, guildID, now)
	if err != nil {
		return nil, fmt.Errorf("rapsheet store: due bans: %w", err)
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("rapsheet store: due bans: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *pgStore) CountPendingBans(ctx context.Context, guildID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM rapsheet_entries WHERE guild_id = $1 AND `+banDue, guildID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("rapsheet store: count pending bans: %w", err)
	}
	return n, nil
}

func (s *pgStore) MarkLifted(ctx context.Context, id int64, at time.Time) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE rapsheet_entries SET lifted_at = $2 WHERE id = $1 AND lifted_at IS NULL`, id, at); err != nil {
		return fmt.Errorf("rapsheet store: mark lifted: %w", err)
	}
	return nil
}

func (s *pgStore) ActiveBan(ctx context.Context, guildID, userID string) (Entry, bool, error) {
	e, err := scanEntry(s.pool.QueryRow(ctx, `
		SELECT `+entryCols+` FROM rapsheet_entries
		WHERE guild_id = $1 AND user_id = $2 AND kind = 'ban' AND lifted_at IS NULL AND voided_at IS NULL
		  AND (ends_at IS NULL OR ends_at > now())
		ORDER BY created_at DESC LIMIT 1
	`, guildID, userID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Entry{}, false, nil
		}
		return Entry{}, false, fmt.Errorf("rapsheet store: active ban: %w", err)
	}
	return e, true, nil
}

const caseFileCols = `guild_id, user_id, thread_id, username, global_name, avatar_hash, created_at, updated_at`

func scanCaseFile(row pgx.Row) (CaseFile, error) {
	var cf CaseFile
	err := row.Scan(&cf.GuildID, &cf.UserID, &cf.ThreadID, &cf.Username, &cf.GlobalName,
		&cf.AvatarHash, &cf.CreatedAt, &cf.UpdatedAt)
	return cf, err
}

func (s *pgStore) CaseFile(ctx context.Context, guildID, userID string) (CaseFile, bool, error) {
	cf, err := scanCaseFile(s.pool.QueryRow(ctx,
		`SELECT `+caseFileCols+` FROM rapsheet_case_files WHERE guild_id = $1 AND user_id = $2`, guildID, userID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return CaseFile{}, false, nil
		}
		return CaseFile{}, false, fmt.Errorf("rapsheet store: case file: %w", err)
	}
	return cf, true, nil
}

func (s *pgStore) UpsertCaseFile(ctx context.Context, cf CaseFile) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO rapsheet_case_files (guild_id, user_id, username, global_name, avatar_hash)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (guild_id, user_id) DO UPDATE SET
			username    = CASE WHEN EXCLUDED.username    <> '' THEN EXCLUDED.username    ELSE rapsheet_case_files.username    END,
			global_name = CASE WHEN EXCLUDED.global_name <> '' THEN EXCLUDED.global_name ELSE rapsheet_case_files.global_name END,
			avatar_hash = CASE WHEN EXCLUDED.avatar_hash <> '' THEN EXCLUDED.avatar_hash ELSE rapsheet_case_files.avatar_hash END,
			updated_at  = now()
	`, cf.GuildID, cf.UserID, cf.Username, cf.GlobalName, cf.AvatarHash)
	if err != nil {
		return fmt.Errorf("rapsheet store: upsert case file: %w", err)
	}
	return nil
}

func (s *pgStore) SetCaseThread(ctx context.Context, guildID, userID, threadID string) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE rapsheet_case_files SET thread_id = $3, updated_at = now() WHERE guild_id = $1 AND user_id = $2
	`, guildID, userID, threadID); err != nil {
		return fmt.Errorf("rapsheet store: set case thread: %w", err)
	}
	return nil
}

func (s *pgStore) CaseFiles(ctx context.Context, guildID string, limit int) ([]CaseFile, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+caseFileCols+` FROM rapsheet_case_files WHERE guild_id = $1 ORDER BY created_at DESC LIMIT $2`,
		guildID, limit)
	if err != nil {
		return nil, fmt.Errorf("rapsheet store: case files: %w", err)
	}
	defer rows.Close()
	var out []CaseFile
	for rows.Next() {
		cf, err := scanCaseFile(rows)
		if err != nil {
			return nil, fmt.Errorf("rapsheet store: case files: %w", err)
		}
		out = append(out, cf)
	}
	return out, rows.Err()
}

func (s *pgStore) Link(ctx context.Context, l Link) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO rapsheet_links (guild_id, user_id, group_id, linked_by, reason)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (guild_id, user_id) DO UPDATE SET
			group_id = EXCLUDED.group_id, linked_by = EXCLUDED.linked_by, reason = EXCLUDED.reason,
			created_at = now()
	`, l.GuildID, l.UserID, l.GroupID, l.LinkedBy, l.Reason)
	if err != nil {
		return fmt.Errorf("rapsheet store: link: %w", err)
	}
	return nil
}

func (s *pgStore) Unlink(ctx context.Context, guildID, userID string) error {
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM rapsheet_links WHERE guild_id = $1 AND user_id = $2`, guildID, userID); err != nil {
		return fmt.Errorf("rapsheet store: unlink: %w", err)
	}
	return nil
}

func (s *pgStore) Group(ctx context.Context, guildID, userID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT user_id FROM rapsheet_links
		WHERE guild_id = $1 AND group_id = (SELECT group_id FROM rapsheet_links WHERE guild_id = $1 AND user_id = $2)
	`, guildID, userID)
	if err != nil {
		return nil, fmt.Errorf("rapsheet store: group: %w", err)
	}
	defer rows.Close()
	out := []string{userID}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("rapsheet store: group: %w", err)
		}
		if id != userID {
			out = append(out, id)
		}
	}
	return out, rows.Err()
}

func (s *pgStore) GroupLinks(ctx context.Context, guildID, userID string) ([]Link, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT guild_id, user_id, group_id, linked_by, reason, created_at FROM rapsheet_links
		WHERE guild_id = $1 AND group_id = (SELECT group_id FROM rapsheet_links WHERE guild_id = $1 AND user_id = $2)
		ORDER BY created_at
	`, guildID, userID)
	if err != nil {
		return nil, fmt.Errorf("rapsheet store: group links: %w", err)
	}
	defer rows.Close()
	var out []Link
	for rows.Next() {
		var l Link
		if err := rows.Scan(&l.GuildID, &l.UserID, &l.GroupID, &l.LinkedBy, &l.Reason, &l.CreatedAt); err != nil {
			return nil, fmt.Errorf("rapsheet store: group links: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *pgStore) UpsertHint(ctx context.Context, h AltHint) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO rapsheet_alt_hints (guild_id, user_id, candidate_id, signals, score, opinion)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (guild_id, user_id, candidate_id) DO UPDATE SET
			signals = EXCLUDED.signals, score = EXCLUDED.score,
			opinion = CASE WHEN EXCLUDED.opinion <> '' THEN EXCLUDED.opinion ELSE rapsheet_alt_hints.opinion END
	`, h.GuildID, h.UserID, h.CandidateID, h.Signals, h.Score, h.Opinion)
	if err != nil {
		return fmt.Errorf("rapsheet store: upsert hint: %w", err)
	}
	return nil
}

func (s *pgStore) Hints(ctx context.Context, guildID, userID string) ([]AltHint, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT guild_id, user_id, candidate_id, signals, score, opinion, created_at FROM rapsheet_alt_hints
		WHERE guild_id = $1 AND (user_id = $2 OR candidate_id = $2)
		ORDER BY score DESC, created_at DESC
	`, guildID, userID)
	if err != nil {
		return nil, fmt.Errorf("rapsheet store: hints: %w", err)
	}
	defer rows.Close()
	var out []AltHint
	for rows.Next() {
		var h AltHint
		if err := rows.Scan(&h.GuildID, &h.UserID, &h.CandidateID, &h.Signals, &h.Score, &h.Opinion, &h.CreatedAt); err != nil {
			return nil, fmt.Errorf("rapsheet store: hints: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *pgStore) DeleteHint(ctx context.Context, guildID, userID, candidateID string) error {
	if _, err := s.pool.Exec(ctx, `
		DELETE FROM rapsheet_alt_hints
		WHERE guild_id = $1 AND ((user_id = $2 AND candidate_id = $3) OR (user_id = $3 AND candidate_id = $2))
	`, guildID, userID, candidateID); err != nil {
		return fmt.Errorf("rapsheet store: delete hint: %w", err)
	}
	return nil
}

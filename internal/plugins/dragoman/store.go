package dragoman

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Config is one guild's settings. The zero value, off, is what a guild with
// no row has.
type Config struct {
	Enabled          bool
	ExemptChannelIDs []string
	// OptOutUserIDs are members whose messages are left as they wrote them,
	// each put there by themselves.
	OptOutUserIDs []string
}

// Translation is one repost: whose it is and where, never what it says.
type Translation struct {
	GuildID, ChannelID, RepostID, AuthorID, Language string
	CreatedAt                                        time.Time
}

var errNotFound = errors.New("dragoman: no such translation")

type Store interface {
	Config(ctx context.Context, guildID string) (Config, error)
	SetEnabled(ctx context.Context, guildID string, on bool) error
	SetExempt(ctx context.Context, guildID, channelID string, on bool) error
	SetOptOut(ctx context.Context, guildID, userID string, on bool) error
	Record(ctx context.Context, t Translation) error
	Find(ctx context.Context, guildID, repostID string) (Translation, error)
	CountSince(ctx context.Context, guildID string, since time.Time) (int, error)
	// PruneBefore is audit.Pruner.
	PruneBefore(ctx context.Context, cutoff time.Time) (int64, error)
}

type pgStore struct{ pool *pgxpool.Pool }

func NewPostgresStore(pool *pgxpool.Pool) Store { return &pgStore{pool: pool} }

func (s *pgStore) Config(ctx context.Context, guildID string) (Config, error) {
	var c Config
	err := s.pool.QueryRow(ctx, `SELECT enabled, exempt_channel_ids, opt_out_user_ids FROM dragoman_config WHERE guild_id = $1`, guildID).
		Scan(&c.Enabled, &c.ExemptChannelIDs, &c.OptOutUserIDs)
	if errors.Is(err, pgx.ErrNoRows) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("dragoman store: read config: %w", err)
	}
	return c, nil
}

func (s *pgStore) SetEnabled(ctx context.Context, guildID string, on bool) error {
	if _, err := s.pool.Exec(ctx, `INSERT INTO dragoman_config (guild_id, enabled) VALUES ($1, $2)
		ON CONFLICT (guild_id) DO UPDATE SET enabled = EXCLUDED.enabled`, guildID, on); err != nil {
		return fmt.Errorf("dragoman store: set enabled: %w", err)
	}
	return nil
}

func (s *pgStore) SetExempt(ctx context.Context, guildID, channelID string, on bool) error {
	return s.setMember(ctx, "exempt_channel_ids", guildID, channelID, on)
}

func (s *pgStore) SetOptOut(ctx context.Context, guildID, userID string, on bool) error {
	return s.setMember(ctx, "opt_out_user_ids", guildID, userID, on)
}

// setMember adds id to a list column or takes it off, in one statement, so
// two members opting out at once both land. col is one of the two literals
// above, never input.
func (s *pgStore) setMember(ctx context.Context, col, guildID, id string, on bool) error {
	list := `array_remove(c.` + col + `, $2)`
	if on {
		list = `array_append(` + list + `, $2)`
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO dragoman_config AS c (guild_id, `+col+`)
		VALUES ($1, CASE WHEN $3 THEN ARRAY[$2::text] ELSE '{}'::text[] END)
		ON CONFLICT (guild_id) DO UPDATE SET `+col+` = `+list, guildID, id, on); err != nil {
		return fmt.Errorf("dragoman store: set %s: %w", col, err)
	}
	return nil
}

func (s *pgStore) Record(ctx context.Context, t Translation) error {
	if _, err := s.pool.Exec(ctx, `INSERT INTO dragoman_translations (guild_id, repost_id, channel_id, author_id, language, created_at)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`,
		t.GuildID, t.RepostID, t.ChannelID, t.AuthorID, t.Language, t.CreatedAt); err != nil {
		return fmt.Errorf("dragoman store: record: %w", err)
	}
	return nil
}

func (s *pgStore) Find(ctx context.Context, guildID, repostID string) (Translation, error) {
	t := Translation{GuildID: guildID, RepostID: repostID}
	err := s.pool.QueryRow(ctx, `SELECT channel_id, author_id, language, created_at FROM dragoman_translations
		WHERE guild_id = $1 AND repost_id = $2`, guildID, repostID).Scan(&t.ChannelID, &t.AuthorID, &t.Language, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Translation{}, errNotFound
	}
	if err != nil {
		return Translation{}, fmt.Errorf("dragoman store: find: %w", err)
	}
	return t, nil
}

func (s *pgStore) CountSince(ctx context.Context, guildID string, since time.Time) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM dragoman_translations WHERE guild_id = $1 AND created_at >= $2`,
		guildID, since).Scan(&n); err != nil {
		return 0, fmt.Errorf("dragoman store: count: %w", err)
	}
	return n, nil
}

func (s *pgStore) PruneBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM dragoman_translations WHERE created_at < $1`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("dragoman store: prune: %w", err)
	}
	return tag.RowsAffected(), nil
}

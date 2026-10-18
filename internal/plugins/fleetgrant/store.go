package fleetgrant

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

type pgStore struct{ pool *pgxpool.Pool }

// NewPostgresStore keeps what birds may ask for in fleet_grantable.
func NewPostgresStore(pool *pgxpool.Pool) Store { return &pgStore{pool: pool} }

func (s *pgStore) Allow(ctx context.Context, guildID, caller, roleID, by string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO fleet_grantable (guild_id, caller, role_id, added_by) VALUES ($1, $2, $3, $4)
		ON CONFLICT (guild_id, caller, role_id) DO NOTHING`, guildID, caller, roleID, by)
	return err
}

func (s *pgStore) Deny(ctx context.Context, guildID, caller, roleID string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM fleet_grantable WHERE guild_id = $1 AND caller = $2 AND role_id = $3`, guildID, caller, roleID)
	return tag.RowsAffected() > 0, err
}

func (s *pgStore) Allowed(ctx context.Context, guildID, caller, roleID string) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM fleet_grantable WHERE guild_id = $1 AND caller = $2 AND role_id = $3)`,
		guildID, caller, roleID).Scan(&ok)
	return ok, err
}

func (s *pgStore) List(ctx context.Context, guildID string) ([]Entry, error) {
	rows, err := s.pool.Query(ctx, `SELECT caller, role_id, added_by, added_at FROM fleet_grantable WHERE guild_id = $1 ORDER BY caller, added_at`, guildID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.Caller, &e.RoleID, &e.AddedBy, &e.AddedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

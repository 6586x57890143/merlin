// Package aimod implements AI-assisted moderation against Discord's own
// Community Guidelines (https://discord.com/guidelines).
//
// It exists for one failure mode the rest of this bot cannot cover. Merlin's
// other defences are reactive to moderators: jail, rotation, audit. Nothing
// watches what is actually posted, so a server whose exposure is weaponized
// mass-reporting (spec.MD section 4) is protected only when a human happens
// to be awake. This plugin reads messages, decides whether they breach the
// guidelines, and removes or rewrites the ones that do.
//
// Three properties shape every decision in the package:
//
//   - Cheap. An admin brings their own OpenRouter key and a daily USD cap.
//     A message climbs a ladder and stops at the first rung that decides:
//     free local filters, then a cheap model on a micro-batch, then a
//     frontier model only on something already flagged. The cap degrades
//     the plugin rather than overspending.
//   - Private. Text leaves the process only when a rung needs it to, and
//     only to providers pinned to zero data retention. It is never logged.
//   - Portable. Nothing is hardcoded to any one server. Key, budget,
//     models, thresholds, exemptions and per-bucket policy are all per
//     guild, in Postgres, set through /aimod.
//
// Persistence mirrors internal/plugins/roles: a plugin-owned store for
// runtime state rather than internal/settings, which holds guild
// configuration. Here that split has a second, sharper reason: settings.Store
// caches every column it reads in memory and is handed to core.Permissions
// as GuildAuthData, so a third-party API key stored there would sit in the
// struct the authorization layer reads on every command.
package aimod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Config is one guild's settings for this plugin. APIKeySealed stays sealed
// in this struct: it is opened only where a request is about to be made, so
// a plaintext key never sits in a long-lived cache.
type Config struct {
	GuildID          string
	APIKeySealed     []byte
	Mode             Mode
	DailyBudgetUSD   float64
	EvidenceHours    int
	FastModels       []string
	DeepModels       []string
	ExemptChannelIDs []string
	ExemptRoleIDs    []string
	SanctionAction   SanctionAction
	// SanctionOptInUserIDs only ever widens who can be sanctioned; see
	// optin.go for why that invariant is what makes it safe.
	SanctionOptInUserIDs []string
	BucketActions        map[Bucket]Action
}

// Mode is the guild-wide switch, above the per-bucket actions.
type Mode string

const (
	// ModeOff scans nothing. The default, so adding this plugin to an
	// existing deployment changes nothing until somebody asks it to.
	ModeOff Mode = "off"
	// ModeFlag classifies and records but never touches a message, whatever
	// the per-bucket actions say. What a guild should run for its first
	// week, to see what the filter would have done before it does it.
	ModeFlag Mode = "flag"
	// ModeEnforce honours the per-bucket actions.
	ModeEnforce Mode = "enforce"
)

// Modes lists every mode, for command choices.
var Modes = []Mode{ModeOff, ModeFlag, ModeEnforce}

// Incident is one message this plugin acted on.
type Incident struct {
	ID          int64
	GuildID     string
	ChannelID   string
	MessageID   string
	AuthorID    string
	Bucket      Bucket
	Action      Action
	Confidence  float64
	Reason      string
	Content     string
	Replacement string
	Undone      bool
	CreatedAt   time.Time
}

// Spend is one guild's usage for one UTC day.
type Spend struct {
	Day                  time.Time
	SpentUSD             float64
	Scanned              int
	FastCalls            int
	DeepCalls            int
	FastPromptTokens     int64
	FastCompletionTokens int64
	DeepPromptTokens     int64
	DeepCompletionTokens int64
	// ReasoningTokens is the share of the completion tokens above that was
	// the model thinking rather than answering. Billed at the completion
	// rate and already counted inside them; broken out because it is the one
	// part of the bill that buys nothing a JSON schema does not already pin
	// down, and an admin deciding on a model should be able to see it.
	ReasoningTokens int64
}

// ErrNoIncident reports that no incident exists for a message, which is what
// /aimod why and /aimod undo get for a message this plugin never touched.
var ErrNoIncident = errors.New("aimod: no incident recorded for that message")

// Store is the narrow persistence seam, mirroring roles.Store's role: this
// plugin's own runtime state, kept out of internal/settings.
type Store interface {
	Config(ctx context.Context, guildID string) (Config, error)
	SetAPIKey(ctx context.Context, guildID string, sealed []byte) error
	SetMode(ctx context.Context, guildID string, mode Mode) error
	SetBudget(ctx context.Context, guildID string, usd float64) error
	SetEvidenceHours(ctx context.Context, guildID string, hours int) error
	SetModels(ctx context.Context, guildID string, fast, deep []string) error
	SetBucketAction(ctx context.Context, guildID string, bucket Bucket, action Action) error
	SetExemptChannels(ctx context.Context, guildID string, ids []string) error
	SetExemptRoles(ctx context.Context, guildID string, ids []string) error
	SetSanctionAction(ctx context.Context, guildID string, action SanctionAction) error
	SetSanctionOptIn(ctx context.Context, guildID string, userIDs []string) error

	// AddSpend accumulates one model call. Separate from the classify path's
	// error handling on purpose: the money was spent whether or not the
	// response parsed, so this is called for any response that carried a
	// usage block.
	AddSpend(ctx context.Context, guildID string, day time.Time, u Usage, deep bool) error
	// AddScanned counts messages that reached a model, which is the
	// denominator every cost estimate in /aimod models show divides by.
	AddScanned(ctx context.Context, guildID string, day time.Time, n int) error
	SpendToday(ctx context.Context, guildID string, day time.Time) (Spend, error)
	SpendSince(ctx context.Context, guildID string, since time.Time) ([]Spend, error)

	// RecordIncident writes before anything is done to the message. See
	// enforce.go: the other order loses the only copy of what was removed.
	RecordIncident(ctx context.Context, inc Incident) (int64, error)
	IncidentByMessage(ctx context.Context, guildID, messageID string) (Incident, error)
	// CountSanctions counts a member's prior enforcement history since a
	// cutoff, which is what the escalation ladder in sanction.go multiplies
	// by. Flags do not count: being looked at is not a punishment.
	CountSanctions(ctx context.Context, guildID, userID string, since time.Time) (int, error)
	// PendingFlags returns a member's flagged-but-unactioned incidents since
	// a cutoff: the messages the scan ceiling stopped this plugin confirming.
	// See sanctionForAbuse, which is what clears them.
	PendingFlags(ctx context.Context, guildID, userID string, since time.Time) ([]Incident, error)
	// MarkActioned records that an incident stopped being a flag and became
	// something that was done.
	MarkActioned(ctx context.Context, id int64, action Action) error
	MarkUndone(ctx context.Context, id int64) error
	// PruneEvidence clears stored message text past each guild's own
	// retention window. Takes no cutoff: the window is per guild and is
	// re-derived here rather than frozen at insert time.
	PruneEvidence(ctx context.Context) (int64, error)
	// PruneBefore satisfies audit.Pruner so evidence retention runs on the
	// same housekeeping tick as audit_log and the action journal. The cutoff
	// is deliberately ignored; see the implementation.
	PruneBefore(ctx context.Context, cutoff time.Time) (int64, error)
}

type pgStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStore returns the production Store.
func NewPostgresStore(pool *pgxpool.Pool) Store { return &pgStore{pool: pool} }

// defaultConfig is what a guild with no row yet gets. Mode off and no key:
// this plugin does nothing at all until an admin asks it to, which is the
// only safe default for something that deletes messages.
func defaultConfig(guildID string) Config {
	return Config{
		GuildID:        guildID,
		Mode:           ModeOff,
		DailyBudgetUSD: 0.50,
		EvidenceHours:  24,
		SanctionAction: SanctionFlag,
		BucketActions:  map[Bucket]Action{},
	}
}

func (s *pgStore) Config(ctx context.Context, guildID string) (Config, error) {
	cfg := defaultConfig(guildID)
	var actionsJSON []byte
	err := s.pool.QueryRow(ctx, `
		SELECT api_key_sealed, mode, daily_budget_usd, evidence_hours,
		       fast_models, deep_models, exempt_channel_ids, exempt_role_ids, sanction_action, sanction_optin_user_ids, bucket_actions
		FROM aimod_config WHERE guild_id = $1
	`, guildID).Scan(&cfg.APIKeySealed, &cfg.Mode, &cfg.DailyBudgetUSD, &cfg.EvidenceHours,
		&cfg.FastModels, &cfg.DeepModels, &cfg.ExemptChannelIDs, &cfg.ExemptRoleIDs, &cfg.SanctionAction, &cfg.SanctionOptInUserIDs, &actionsJSON)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return cfg, nil
		}
		return Config{}, fmt.Errorf("aimod store: config: %w", err)
	}
	if len(actionsJSON) > 0 {
		raw := map[string]string{}
		if err := json.Unmarshal(actionsJSON, &raw); err != nil {
			return Config{}, fmt.Errorf("aimod store: parse bucket actions: %w", err)
		}
		for b, a := range raw {
			cfg.BucketActions[Bucket(b)] = Action(a)
		}
	}
	return cfg, nil
}

// upsert is the one write shape every scalar setter uses. Every setter has
// to create the row if it does not exist, since a guild's first /aimod
// command is as likely to be `configure budget` as `configure key`.
func (s *pgStore) upsert(ctx context.Context, guildID, column string, value any) error {
	// column is never user input: every call site passes a literal below.
	sql := fmt.Sprintf(`
		INSERT INTO aimod_config (guild_id, %[1]s) VALUES ($1, $2)
		ON CONFLICT (guild_id) DO UPDATE SET %[1]s = EXCLUDED.%[1]s, updated_at = now()
	`, column)
	if _, err := s.pool.Exec(ctx, sql, guildID, value); err != nil {
		return fmt.Errorf("aimod store: set %s: %w", column, err)
	}
	return nil
}

func (s *pgStore) SetAPIKey(ctx context.Context, guildID string, sealed []byte) error {
	return s.upsert(ctx, guildID, "api_key_sealed", sealed)
}

func (s *pgStore) SetMode(ctx context.Context, guildID string, mode Mode) error {
	return s.upsert(ctx, guildID, "mode", string(mode))
}

func (s *pgStore) SetBudget(ctx context.Context, guildID string, usd float64) error {
	return s.upsert(ctx, guildID, "daily_budget_usd", usd)
}

func (s *pgStore) SetEvidenceHours(ctx context.Context, guildID string, hours int) error {
	return s.upsert(ctx, guildID, "evidence_hours", hours)
}

func (s *pgStore) SetExemptChannels(ctx context.Context, guildID string, ids []string) error {
	return s.upsert(ctx, guildID, "exempt_channel_ids", ids)
}

func (s *pgStore) SetExemptRoles(ctx context.Context, guildID string, ids []string) error {
	return s.upsert(ctx, guildID, "exempt_role_ids", ids)
}

func (s *pgStore) SetSanctionAction(ctx context.Context, guildID string, action SanctionAction) error {
	return s.upsert(ctx, guildID, "sanction_action", string(action))
}

func (s *pgStore) SetSanctionOptIn(ctx context.Context, guildID string, userIDs []string) error {
	return s.upsert(ctx, guildID, "sanction_optin_user_ids", userIDs)
}

func (s *pgStore) SetModels(ctx context.Context, guildID string, fast, deep []string) error {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO aimod_config (guild_id, fast_models, deep_models) VALUES ($1, $2, $3)
		ON CONFLICT (guild_id) DO UPDATE SET fast_models = EXCLUDED.fast_models,
		                                     deep_models = EXCLUDED.deep_models,
		                                     updated_at = now()
	`, guildID, fast, deep); err != nil {
		return fmt.Errorf("aimod store: set models: %w", err)
	}
	return nil
}

// SetBucketAction stores one bucket's override, merging into the existing
// JSONB rather than replacing it. jsonb_set would need the row to exist
// already; the concatenation operator works on the default '{}' too, which
// is what lets a guild's very first command be a policy change.
func (s *pgStore) SetBucketAction(ctx context.Context, guildID string, bucket Bucket, action Action) error {
	patch, err := json.Marshal(map[string]string{string(bucket): string(action)})
	if err != nil {
		return fmt.Errorf("aimod store: encode bucket action: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO aimod_config (guild_id, bucket_actions) VALUES ($1, $2::jsonb)
		ON CONFLICT (guild_id) DO UPDATE SET bucket_actions = aimod_config.bucket_actions || EXCLUDED.bucket_actions,
		                                     updated_at = now()
	`, guildID, patch); err != nil {
		return fmt.Errorf("aimod store: set bucket action: %w", err)
	}
	return nil
}

func (s *pgStore) AddSpend(ctx context.Context, guildID string, day time.Time, u Usage, deep bool) error {
	fastCalls, deepCalls := 1, 0
	fastPrompt, fastCompletion := u.PromptTokens, u.CompletionTokens
	deepPrompt, deepCompletion := int64(0), int64(0)
	if deep {
		fastCalls, deepCalls = 0, 1
		fastPrompt, fastCompletion = 0, 0
		deepPrompt, deepCompletion = u.PromptTokens, u.CompletionTokens
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO aimod_spend (guild_id, day, spent_usd, fast_calls, deep_calls,
		                         fast_prompt_tokens, fast_completion_tokens,
		                         deep_prompt_tokens, deep_completion_tokens)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (guild_id, day) DO UPDATE SET
			spent_usd              = aimod_spend.spent_usd + EXCLUDED.spent_usd,
			fast_calls             = aimod_spend.fast_calls + EXCLUDED.fast_calls,
			deep_calls             = aimod_spend.deep_calls + EXCLUDED.deep_calls,
			fast_prompt_tokens     = aimod_spend.fast_prompt_tokens + EXCLUDED.fast_prompt_tokens,
			fast_completion_tokens = aimod_spend.fast_completion_tokens + EXCLUDED.fast_completion_tokens,
			deep_prompt_tokens     = aimod_spend.deep_prompt_tokens + EXCLUDED.deep_prompt_tokens,
			deep_completion_tokens = aimod_spend.deep_completion_tokens + EXCLUDED.deep_completion_tokens
	`, guildID, day, u.Cost, fastCalls, deepCalls, fastPrompt, fastCompletion, deepPrompt, deepCompletion); err != nil {
		return fmt.Errorf("aimod store: add spend: %w", err)
	}
	return nil
}

func (s *pgStore) AddScanned(ctx context.Context, guildID string, day time.Time, n int) error {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO aimod_spend (guild_id, day, scanned) VALUES ($1, $2, $3)
		ON CONFLICT (guild_id, day) DO UPDATE SET scanned = aimod_spend.scanned + EXCLUDED.scanned
	`, guildID, day, n); err != nil {
		return fmt.Errorf("aimod store: add scanned: %w", err)
	}
	return nil
}

func (s *pgStore) SpendToday(ctx context.Context, guildID string, day time.Time) (Spend, error) {
	sp := Spend{Day: day}
	err := s.pool.QueryRow(ctx, `
		SELECT spent_usd, scanned, fast_calls, deep_calls,
		       fast_prompt_tokens, fast_completion_tokens, deep_prompt_tokens, deep_completion_tokens
		FROM aimod_spend WHERE guild_id = $1 AND day = $2
	`, guildID, day).Scan(&sp.SpentUSD, &sp.Scanned, &sp.FastCalls, &sp.DeepCalls,
		&sp.FastPromptTokens, &sp.FastCompletionTokens, &sp.DeepPromptTokens, &sp.DeepCompletionTokens)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return sp, nil
		}
		return Spend{}, fmt.Errorf("aimod store: spend today: %w", err)
	}
	return sp, nil
}

func (s *pgStore) SpendSince(ctx context.Context, guildID string, since time.Time) ([]Spend, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT day, spent_usd, scanned, fast_calls, deep_calls,
		       fast_prompt_tokens, fast_completion_tokens, deep_prompt_tokens, deep_completion_tokens
		FROM aimod_spend WHERE guild_id = $1 AND day >= $2 ORDER BY day DESC
	`, guildID, since)
	if err != nil {
		return nil, fmt.Errorf("aimod store: spend since: %w", err)
	}
	defer rows.Close()

	var out []Spend
	for rows.Next() {
		var sp Spend
		if err := rows.Scan(&sp.Day, &sp.SpentUSD, &sp.Scanned, &sp.FastCalls, &sp.DeepCalls,
			&sp.FastPromptTokens, &sp.FastCompletionTokens, &sp.DeepPromptTokens, &sp.DeepCompletionTokens); err != nil {
			return nil, fmt.Errorf("aimod store: scan spend: %w", err)
		}
		out = append(out, sp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("aimod store: iterate spend: %w", err)
	}
	return out, nil
}

// RecordIncident upserts on (guild_id, message_id): a retry after a partial
// failure must not leave two rows for one message, and DO UPDATE rather than
// DO NOTHING so a re-run refreshes the verdict rather than keeping a stale
// one. Unlike roles.InsertJail's DO NOTHING, there is no snapshot here that
// a second write could destroy: the content column is the same text either
// time, because it comes from the message, not from live Discord state.
func (s *pgStore) RecordIncident(ctx context.Context, inc Incident) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO aimod_incidents (guild_id, channel_id, message_id, author_id, bucket, action,
		                             confidence, reason, content, replacement, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (guild_id, message_id) DO UPDATE SET
			bucket = EXCLUDED.bucket, action = EXCLUDED.action, confidence = EXCLUDED.confidence,
			reason = EXCLUDED.reason, content = EXCLUDED.content, replacement = EXCLUDED.replacement
		RETURNING id
	`, inc.GuildID, inc.ChannelID, inc.MessageID, inc.AuthorID, string(inc.Bucket), string(inc.Action),
		inc.Confidence, inc.Reason, inc.Content, inc.Replacement, inc.CreatedAt).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("aimod store: record incident: %w", err)
	}
	return id, nil
}

func (s *pgStore) IncidentByMessage(ctx context.Context, guildID, messageID string) (Incident, error) {
	inc := Incident{GuildID: guildID, MessageID: messageID}
	var bucket, action string
	err := s.pool.QueryRow(ctx, `
		SELECT id, channel_id, author_id, bucket, action, confidence, reason, content, replacement, undone, created_at
		FROM aimod_incidents WHERE guild_id = $1 AND message_id = $2
	`, guildID, messageID).Scan(&inc.ID, &inc.ChannelID, &inc.AuthorID, &bucket, &action,
		&inc.Confidence, &inc.Reason, &inc.Content, &inc.Replacement, &inc.Undone, &inc.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Incident{}, ErrNoIncident
		}
		return Incident{}, fmt.Errorf("aimod store: incident by message: %w", err)
	}
	inc.Bucket, inc.Action = Bucket(bucket), Action(action)
	return inc, nil
}

// CountSanctions excludes flags, and excludes anything a moderator reversed.
// Both exclusions matter. Counting flags would escalate somebody for being
// argued about; counting a reversed incident would mean a single false
// positive permanently lengthens every sentence that member ever gets after
// it, which is the worst available interaction between an automated ladder
// and a mistake.
func (s *pgStore) CountSanctions(ctx context.Context, guildID, userID string, since time.Time) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM aimod_incidents
		WHERE guild_id = $1 AND author_id = $2 AND created_at >= $3
		  AND action <> 'flag' AND NOT undone
	`, guildID, userID, since).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("aimod store: count sanctions: %w", err)
	}
	return n, nil
}

// PendingFlags is deliberately narrow: flags only, never something already
// acted on, and never something a moderator reversed. Re-deleting a message
// a mod deliberately restored would be the bot overruling a human.
func (s *pgStore) PendingFlags(ctx context.Context, guildID, userID string, since time.Time) ([]Incident, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, channel_id, message_id, bucket, confidence, reason, content, replacement, created_at
		FROM aimod_incidents
		WHERE guild_id = $1 AND author_id = $2 AND created_at >= $3
		  AND action = 'flag' AND NOT undone
		ORDER BY created_at
	`, guildID, userID, since)
	if err != nil {
		return nil, fmt.Errorf("aimod store: pending flags: %w", err)
	}
	defer rows.Close()

	var out []Incident
	for rows.Next() {
		inc := Incident{GuildID: guildID, AuthorID: userID, Action: ActionFlag}
		var bucket string
		if err := rows.Scan(&inc.ID, &inc.ChannelID, &inc.MessageID, &bucket, &inc.Confidence,
			&inc.Reason, &inc.Content, &inc.Replacement, &inc.CreatedAt); err != nil {
			return nil, fmt.Errorf("aimod store: scan pending flag: %w", err)
		}
		inc.Bucket = Bucket(bucket)
		out = append(out, inc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("aimod store: iterate pending flags: %w", err)
	}
	return out, nil
}

func (s *pgStore) MarkActioned(ctx context.Context, id int64, action Action) error {
	if _, err := s.pool.Exec(ctx, `UPDATE aimod_incidents SET action = $2 WHERE id = $1`, id, string(action)); err != nil {
		return fmt.Errorf("aimod store: mark actioned: %w", err)
	}
	return nil
}

func (s *pgStore) MarkUndone(ctx context.Context, id int64) error {
	if _, err := s.pool.Exec(ctx, `UPDATE aimod_incidents SET undone = TRUE WHERE id = $1`, id); err != nil {
		return fmt.Errorf("aimod store: mark undone: %w", err)
	}
	return nil
}

// PruneEvidence clears the stored text of incidents past their guild's
// retention window, leaving the row itself as the durable audit fact.
//
// The window is re-derived here, on every run, by joining aimod_config,
// rather than being written into a per-row expiry column at insert time.
// That is the rotation_archives.delete_after mistake, and this is the same
// mistake pointing the other way: a frozen column means an admin who
// *shortens* the window watches old message text sit in the database on the
// schedule that was in force when it was written. Widening it is a
// convenience; narrowing it is a privacy promise, and it is the direction
// that has to work.
//
// A guild with no config row keeps nothing, via the COALESCE default of 0:
// text can only exist for a guild that configured the plugin, but if a row
// were ever deleted out from under its incidents, dropping the text is the
// direction to fail in.
func (s *pgStore) PruneEvidence(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE aimod_incidents i
		SET content = '', replacement = ''
		WHERE i.content <> ''
		  AND i.created_at < $1::timestamptz - make_interval(hours => COALESCE(
		        (SELECT c.evidence_hours FROM aimod_config c WHERE c.guild_id = i.guild_id), 0))
	`, time.Now().UTC())
	if err != nil {
		return 0, fmt.Errorf("aimod store: prune evidence: %w", err)
	}
	return tag.RowsAffected(), nil
}

// PruneBefore satisfies audit.Pruner so this plugin's evidence retention
// runs on the same housekeeping tick as audit_log and the action journal.
//
// The cutoff is deliberately ignored: audit.StartRetention passes one fixed
// process-wide age, and this window is per guild and admin-settable. Taking
// the tick without taking the cutoff is what lets the schedule be shared
// while the policy stays where it belongs.
func (s *pgStore) PruneBefore(ctx context.Context, _ time.Time) (int64, error) {
	return s.PruneEvidence(ctx)
}

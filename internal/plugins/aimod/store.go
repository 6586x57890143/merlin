// Package aimod implements AI-assisted moderation against Discord's own
// Community Guidelines (https://discord.com/guidelines).
//
// It exists for one failure mode the rest of this bot cannot cover. merlin's
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
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Config is one guild's settings for this plugin. APIKeySealed stays sealed
// in this struct: it is opened only where a request is about to be made, so
// a plaintext key never sits in a long-lived cache.
type Config struct {
	GuildID string
	// APIKeySealed is the OpenRouter credential and OrcaKeySealed the
	// OrcaRouter one. Which gateway a guild's traffic goes through is
	// derived from which of these is set rather than stored beside them:
	// see aimod.route.
	APIKeySealed     []byte
	OrcaKeySealed    []byte
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

	// Calibration is the active learned example set, rendered into both
	// classifier prompts. CalibrationPending is a proposal awaiting
	// /aimod calibrate apply and is never read by the classifier: keeping
	// them in separate columns is what stops a run in suggest mode changing
	// behaviour simply by having happened.
	Calibration        []CalibrationExample
	CalibrationPending []CalibrationExample
	CalibrationMode    CalibrationMode
	// CalibrationRanAt is zero until the first review. Displayed by
	// /aimod calibrate show; nothing schedules from it, because the
	// Scheduler owns its own last-run bookkeeping.
	CalibrationRanAt time.Time

	// TriageMode is how much the local rung 1.5 model may do. On aimod_config
	// rather than beside the weights, because this one is read on the message
	// hot path and belongs behind cachingStore; the weights are runtime state
	// written every few hundred messages and live in their own table, exactly
	// the split the tip jar uses.
	TriageMode TriageMode
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

// CalibrationMode is the guild's setting for the weekly self-review. It
// mirrors Mode's ladder deliberately: off, then a step where the mechanism
// runs and reports without changing anything, then the one where it acts.
type CalibrationMode string

const (
	CalibrationOff CalibrationMode = "off"
	// CalibrationSuggest reviews on schedule and posts a proposal to the
	// audit channel, leaving the active set alone until an admin runs
	// /aimod calibrate apply. The default, for the same reason ModeFlag sits
	// between off and enforce: this changes what a filter that deletes
	// messages does, and a guild should get to read the first one.
	CalibrationSuggest CalibrationMode = "suggest"
	CalibrationAuto    CalibrationMode = "auto"
)

// CalibrationModes lists every mode, for command choices.
var CalibrationModes = []CalibrationMode{CalibrationOff, CalibrationSuggest, CalibrationAuto}

func (m CalibrationMode) valid() bool { return slices.Contains(CalibrationModes, m) }

// CalibrationExample is one labelled example learned from a guild's own
// moderation history and rendered into the classifier prompts.
//
// Typed fields, not prose, and that is the entire safety argument for this
// feature. A model proposing free text would be writing instructions into a
// prompt that deletes messages, with nothing able to check them. Here the
// model fills four slots and this package writes the sentence, so the worst a
// bad entry can do is be a wrong example: it cannot enable a bucket the guild
// switched off, change an action, move a threshold, or say anything the
// renderer in classify.go does not have a format string for.
//
// Text is a paraphrase or a synthetic stand-in, never a verbatim quote. That
// is asked for in the prompt and enforced in validateCalibration, because
// this column is not covered by the evidence-retention window and a verbatim
// copy of somebody's message would quietly outlive it.
type CalibrationExample struct {
	Text      string `json:"text"`
	Bucket    Bucket `json:"bucket"`
	ShouldAct bool   `json:"should_act"`
	Note      string `json:"note"`
}

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
	// SetAPIKey stores one gateway's sealed credential. provider is a
	// providerSpec name; an unknown one is an error rather than a silent
	// write to the wrong column.
	SetAPIKey(ctx context.Context, guildID, provider string, sealed []byte) error
	SetMode(ctx context.Context, guildID string, mode Mode) error
	SetBudget(ctx context.Context, guildID string, usd float64) error
	SetEvidenceHours(ctx context.Context, guildID string, hours int) error
	SetModels(ctx context.Context, guildID string, fast, deep []string) error
	SetBucketAction(ctx context.Context, guildID string, bucket Bucket, action Action) error
	SetExemptChannels(ctx context.Context, guildID string, ids []string) error
	SetExemptRoles(ctx context.Context, guildID string, ids []string) error
	SetSanctionAction(ctx context.Context, guildID string, action SanctionAction) error
	SetSanctionOptIn(ctx context.Context, guildID string, userIDs []string) error
	// SetCalibration writes both sets at once, because every path that
	// changes one changes the other: a review in suggest mode fills pending
	// and leaves active, apply moves pending into active and empties
	// pending, and clear empties both. Two setters would make "apply" two
	// writes that can half-fail.
	SetCalibration(ctx context.Context, guildID string, active, pending []CalibrationExample, ranAt time.Time) error
	SetCalibrationMode(ctx context.Context, guildID string, mode CalibrationMode) error

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

	// The tip jar. These live in their own table rather than on aimod_config,
	// so none of them needs a cachingStore override: the poller writes every
	// 15 minutes and the message hot path never reads them.
	Funding(ctx context.Context, guildID string) (Funding, error)
	SetFundingAddress(ctx context.Context, guildID, address, setBy string, at time.Time, baseline float64, balances map[string]float64) error
	ClearFunding(ctx context.Context, guildID string) error
	UpdateFundingBalance(ctx context.Context, guildID string, balance, donation float64, balances map[string]float64, at time.Time) error

	// The local triage model's weights. Their own table for the same reason
	// the tip jar has one: written on a cadence of its own and never read by
	// the config hot path, so neither needs a cachingStore override.
	TriageModel(ctx context.Context, guildID string) (raw []byte, examples int64, err error)
	SaveTriageModel(ctx context.Context, guildID string, raw []byte, examples int64) error
	SetTriageMode(ctx context.Context, guildID string, mode TriageMode) error

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
	// IncidentsSince returns a guild's incidents newest-first since a cutoff,
	// bounded by limit. What the weekly calibration review reads: which
	// channels this filter has actually been busy in, what it decided, and
	// (via Undone) which of those decisions a moderator reversed.
	IncidentsSince(ctx context.Context, guildID string, since time.Time, limit int) ([]Incident, error)
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
		// Matches the column default. It costs nothing while Mode is off,
		// since reconcileCalibrationJob registers no job unless both are on.
		CalibrationMode: CalibrationSuggest,
		// Matches the column default, and for the same reason calibration
		// defaults to suggest: this changes what gets looked at, so a guild
		// watches it work before it acts.
		TriageMode:    TriageShadow,
		BucketActions: map[Bucket]Action{},
	}
}

func (s *pgStore) Config(ctx context.Context, guildID string) (Config, error) {
	cfg := defaultConfig(guildID)
	var actionsJSON, calJSON, calPendingJSON []byte
	var ranAt *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT api_key_sealed, orca_key_sealed, mode, daily_budget_usd, evidence_hours,
		       fast_models, deep_models, exempt_channel_ids, exempt_role_ids, sanction_action, sanction_optin_user_ids, bucket_actions,
		       calibration, calibration_pending, calibration_mode, calibration_ran_at,
		       triage_mode
		FROM aimod_config WHERE guild_id = $1
	`, guildID).Scan(&cfg.APIKeySealed, &cfg.OrcaKeySealed, &cfg.Mode, &cfg.DailyBudgetUSD, &cfg.EvidenceHours,
		&cfg.FastModels, &cfg.DeepModels, &cfg.ExemptChannelIDs, &cfg.ExemptRoleIDs, &cfg.SanctionAction, &cfg.SanctionOptInUserIDs, &actionsJSON,
		&calJSON, &calPendingJSON, &cfg.CalibrationMode, &ranAt, &cfg.TriageMode)
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
	// Unreadable calibration is dropped, not fatal, and the difference
	// matters: this is read on the hot path for every message, so a column
	// somebody hand-edited into nonsense must not stop the guild being
	// moderated at all. An empty set is exactly the pre-calibration
	// behaviour, which is a safe place to land.
	cfg.Calibration = decodeCalibration(calJSON)
	cfg.CalibrationPending = decodeCalibration(calPendingJSON)
	if ranAt != nil {
		cfg.CalibrationRanAt = ranAt.UTC()
	}
	return cfg, nil
}

func decodeCalibration(raw []byte) []CalibrationExample {
	if len(raw) == 0 {
		return nil
	}
	var out []CalibrationExample
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
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

func (s *pgStore) SetAPIKey(ctx context.Context, guildID, provider string, sealed []byte) error {
	column, err := keyColumn(provider)
	if err != nil {
		return err
	}
	return s.upsert(ctx, guildID, column, sealed)
}

// keyColumn maps a gateway name to its credential column. Explicitly, and
// with an error for anything unknown, because upsert interpolates the column
// name into the statement: the one place in this file where a caller's
// string reaching the query unchecked would matter.
func keyColumn(provider string) (string, error) {
	switch provider {
	case "openrouter":
		return "api_key_sealed", nil
	case "orcarouter":
		return "orca_key_sealed", nil
	default:
		return "", fmt.Errorf("aimod store: unknown provider %q", provider)
	}
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

func (s *pgStore) SetCalibrationMode(ctx context.Context, guildID string, mode CalibrationMode) error {
	return s.upsert(ctx, guildID, "calibration_mode", string(mode))
}

// SetCalibration writes the active set, the pending proposal and the run
// timestamp in one statement.
//
// One write rather than three setters, because no caller ever changes one
// without the others: a review in suggest mode fills pending and leaves
// active alone, apply moves pending into active and empties pending, and
// clear empties both. Split across separate statements, an apply that
// half-failed would leave a guild enforcing a set it had also kept queued.
func (s *pgStore) SetCalibration(ctx context.Context, guildID string, active, pending []CalibrationExample, ranAt time.Time) error {
	// Marshalled from a non-nil slice so a cleared set stores '[]' rather
	// than 'null', which the CHECK-less JSONB column would happily accept
	// and decodeCalibration would then have to special-case.
	activeJSON, err := json.Marshal(nonNil(active))
	if err != nil {
		return fmt.Errorf("aimod store: encode calibration: %w", err)
	}
	pendingJSON, err := json.Marshal(nonNil(pending))
	if err != nil {
		return fmt.Errorf("aimod store: encode pending calibration: %w", err)
	}
	var ran *time.Time
	if !ranAt.IsZero() {
		ran = &ranAt
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO aimod_config (guild_id, calibration, calibration_pending, calibration_ran_at)
		VALUES ($1, $2::jsonb, $3::jsonb, $4)
		ON CONFLICT (guild_id) DO UPDATE SET calibration = EXCLUDED.calibration,
		                                     calibration_pending = EXCLUDED.calibration_pending,
		                                     calibration_ran_at = COALESCE(EXCLUDED.calibration_ran_at, aimod_config.calibration_ran_at),
		                                     updated_at = now()
	`, guildID, activeJSON, pendingJSON, ran); err != nil {
		return fmt.Errorf("aimod store: set calibration: %w", err)
	}
	return nil
}

func nonNil(in []CalibrationExample) []CalibrationExample {
	if in == nil {
		return []CalibrationExample{}
	}
	return in
}

// IncidentsSince is the calibration review's input: what this filter did
// lately, and (via undone) which of it a moderator disagreed with.
//
// Deliberately not filtered the way CountSanctions and PendingFlags are.
// Those two are asking about one member's history and must exclude reversals;
// this one is asking how the filter is performing, and a reversal is the most
// informative row in the table.
func (s *pgStore) IncidentsSince(ctx context.Context, guildID string, since time.Time, limit int) ([]Incident, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, channel_id, message_id, author_id, bucket, action, confidence, reason, content, replacement, undone, created_at
		FROM aimod_incidents
		WHERE guild_id = $1 AND created_at >= $2
		ORDER BY created_at DESC
		LIMIT $3
	`, guildID, since, limit)
	if err != nil {
		return nil, fmt.Errorf("aimod store: incidents since: %w", err)
	}
	defer rows.Close()

	var out []Incident
	for rows.Next() {
		inc := Incident{GuildID: guildID}
		var bucket, action string
		if err := rows.Scan(&inc.ID, &inc.ChannelID, &inc.MessageID, &inc.AuthorID, &bucket, &action,
			&inc.Confidence, &inc.Reason, &inc.Content, &inc.Replacement, &inc.Undone, &inc.CreatedAt); err != nil {
			return nil, fmt.Errorf("aimod store: scan incident: %w", err)
		}
		inc.Bucket, inc.Action = Bucket(bucket), Action(action)
		out = append(out, inc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("aimod store: iterate incidents: %w", err)
	}
	return out, nil
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
		                         deep_prompt_tokens, deep_completion_tokens, reasoning_tokens)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (guild_id, day) DO UPDATE SET
			spent_usd              = aimod_spend.spent_usd + EXCLUDED.spent_usd,
			fast_calls             = aimod_spend.fast_calls + EXCLUDED.fast_calls,
			deep_calls             = aimod_spend.deep_calls + EXCLUDED.deep_calls,
			fast_prompt_tokens     = aimod_spend.fast_prompt_tokens + EXCLUDED.fast_prompt_tokens,
			fast_completion_tokens = aimod_spend.fast_completion_tokens + EXCLUDED.fast_completion_tokens,
			deep_prompt_tokens     = aimod_spend.deep_prompt_tokens + EXCLUDED.deep_prompt_tokens,
			deep_completion_tokens = aimod_spend.deep_completion_tokens + EXCLUDED.deep_completion_tokens,
			reasoning_tokens       = aimod_spend.reasoning_tokens + EXCLUDED.reasoning_tokens
	`, guildID, day, u.Cost, fastCalls, deepCalls, fastPrompt, fastCompletion, deepPrompt, deepCompletion,
		// Not split by tier, and accumulated across both: reasoning is a
		// property of the endpoint rather than of the pass, and the question
		// it answers is whether this guild is paying for thinking at all.
		u.CompletionTokensDetails.ReasoningTokens); err != nil {
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
		       fast_prompt_tokens, fast_completion_tokens, deep_prompt_tokens, deep_completion_tokens,
		       reasoning_tokens
		FROM aimod_spend WHERE guild_id = $1 AND day = $2
	`, guildID, day).Scan(&sp.SpentUSD, &sp.Scanned, &sp.FastCalls, &sp.DeepCalls,
		&sp.FastPromptTokens, &sp.FastCompletionTokens, &sp.DeepPromptTokens, &sp.DeepCompletionTokens,
		&sp.ReasoningTokens)
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
		       fast_prompt_tokens, fast_completion_tokens, deep_prompt_tokens, deep_completion_tokens,
		       reasoning_tokens
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
			&sp.FastPromptTokens, &sp.FastCompletionTokens, &sp.DeepPromptTokens, &sp.DeepCompletionTokens,
			&sp.ReasoningTokens); err != nil {
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

// Funding is one guild's tip jar: the wallet donations go to, who pointed
// this bot at it, and the running totals the poller has observed on it.
//
// A zero Address means no jar is configured, which is what a guild that has
// never run /aimod funding set-address gets. A zero CheckedAt means the
// address has never been polled, so the next poll records a baseline rather
// than reporting the whole existing balance as a donation.
type Funding struct {
	GuildID     string
	Address     string
	SetBy       string
	SetAt       time.Time
	BalanceUSD  float64
	ReceivedUSD float64
	Donations   int
	CheckedAt   time.Time
	// Balances is the per-rail breakdown behind BalanceUSD, keyed "chain:asset".
	// Written only by a complete poll, so it either agrees with BalanceUSD or
	// is empty; it is never a partial view of it.
	Balances map[string]float64
}

// Configured reports whether this guild has a tip jar at all.
func (f Funding) Configured() bool { return f.Address != "" }

func (s *pgStore) Funding(ctx context.Context, guildID string) (Funding, error) {
	f := Funding{GuildID: guildID}
	var checkedAt *time.Time
	var balancesJSON []byte
	err := s.pool.QueryRow(ctx, `
		SELECT address, set_by, set_at, balance_usd, received_usd, donations, checked_at, balances
		FROM aimod_funding WHERE guild_id = $1
	`, guildID).Scan(&f.Address, &f.SetBy, &f.SetAt, &f.BalanceUSD, &f.ReceivedUSD, &f.Donations, &checkedAt, &balancesJSON)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return f, nil
		}
		return Funding{}, fmt.Errorf("aimod store: funding: %w", err)
	}
	if checkedAt != nil {
		f.CheckedAt = *checkedAt
	}
	// An unreadable breakdown is dropped rather than fatal, the same call as
	// decodeCalibration next door and for the same reason: the breakdown is a
	// display detail, while the totals beside it are the answer somebody ran
	// the command for. A hand-edited column must not take the whole tip jar
	// down, and an empty map renders exactly as a jar that has not been
	// polled since the column was added.
	if len(balancesJSON) > 0 {
		decoded := map[string]float64{}
		if err := json.Unmarshal(balancesJSON, &decoded); err == nil {
			f.Balances = decoded
		}
	}
	return f, nil
}

// SetFundingAddress points a guild's jar at a wallet, recording who did it
// and when.
//
// baseline is the balance already sitting on that wallet, read in the same
// command that stored it. Writing it here with checked_at set is what stops
// the first poll counting an existing balance as a donation.
//
// Re-pointing resets received_usd and donations, because those totals belong
// to the wallet that earned them and carrying them onto a different address
// would credit a new jar with another one's history.
func (s *pgStore) SetFundingAddress(ctx context.Context, guildID, address, setBy string, at time.Time, baseline float64, balances map[string]float64) error {
	encoded, err := encodeBalances(balances)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO aimod_funding (guild_id, address, set_by, set_at, balance_usd, checked_at, balances)
		VALUES ($1, $2, $3, $4, $5, $4, $6::jsonb)
		ON CONFLICT (guild_id) DO UPDATE SET
			address      = EXCLUDED.address,
			set_by       = EXCLUDED.set_by,
			set_at       = EXCLUDED.set_at,
			balance_usd  = EXCLUDED.balance_usd,
			checked_at   = EXCLUDED.checked_at,
			balances     = EXCLUDED.balances,
			received_usd = 0,
			donations    = 0
	`, guildID, address, setBy, at, baseline, encoded)
	if err != nil {
		return fmt.Errorf("aimod store: set funding address: %w", err)
	}
	return nil
}

// encodeBalances renders a per-rail breakdown for the JSONB column.
//
// Marshalled here and cast at the call site rather than handed to pgx as a
// map, matching how bucket_actions and calibration are already written. Never
// NULL: the column is NOT NULL, and "{}" is also the honest rendering of a
// family whose rails all read zero.
func encodeBalances(balances map[string]float64) ([]byte, error) {
	if balances == nil {
		balances = map[string]float64{}
	}
	encoded, err := json.Marshal(balances)
	if err != nil {
		return nil, fmt.Errorf("aimod store: encode balances: %w", err)
	}
	return encoded, nil
}

func (s *pgStore) SetTriageMode(ctx context.Context, guildID string, mode TriageMode) error {
	return s.upsert(ctx, guildID, "triage_mode", string(mode))
}

// TriageModel reads a guild's stored weights. A missing row is not an error:
// it is a guild whose model has never been saved, which starts fresh and
// therefore skips nothing until it has warmed up.
func (s *pgStore) TriageModel(ctx context.Context, guildID string) ([]byte, int64, error) {
	var raw []byte
	var examples int64
	err := s.pool.QueryRow(ctx, `
		SELECT weights, examples FROM aimod_triage WHERE guild_id = $1
	`, guildID).Scan(&raw, &examples)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("aimod store: triage model: %w", err)
	}
	return raw, examples, nil
}

func (s *pgStore) SaveTriageModel(ctx context.Context, guildID string, raw []byte, examples int64) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO aimod_triage (guild_id, weights, examples, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (guild_id) DO UPDATE SET
			weights    = EXCLUDED.weights,
			examples   = EXCLUDED.examples,
			updated_at = EXCLUDED.updated_at
	`, guildID, raw, examples)
	if err != nil {
		return fmt.Errorf("aimod store: save triage model: %w", err)
	}
	return nil
}

func (s *pgStore) ClearFunding(ctx context.Context, guildID string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM aimod_funding WHERE guild_id = $1`, guildID); err != nil {
		return fmt.Errorf("aimod store: clear funding: %w", err)
	}
	return nil
}

// UpdateFundingBalance records one poll. donation is the increase since the
// last poll, or 0 when the balance held steady or fell.
//
// One statement rather than a read-modify-write, so two overlapping polls
// cannot lose a donation between them. A fall in balance is the operator
// moving funds out to buy credits: the new balance records it and no donation
// is counted.
func (s *pgStore) UpdateFundingBalance(ctx context.Context, guildID string, balance, donation float64, balances map[string]float64, at time.Time) error {
	encoded, err := encodeBalances(balances)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		UPDATE aimod_funding
		SET balance_usd  = $2,
		    checked_at   = $3,
		    received_usd = received_usd + $4,
		    donations    = donations + CASE WHEN $4 > 0 THEN 1 ELSE 0 END,
		    balances     = $5::jsonb
		WHERE guild_id = $1
	`, guildID, balance, at, donation, encoded)
	if err != nil {
		return fmt.Errorf("aimod store: update funding balance: %w", err)
	}
	return nil
}

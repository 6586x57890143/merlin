package aimod

import (
	"context"
	"sync"
	"time"
)

// configTTL is how long a guild's config is reused before being re-read.
//
// This plugin reads config on the hot path: once per message that gets past
// the free author checks, in every guild, forever. Every other store in this
// codebase caches per guild (internal/settings holds its whole cache in
// memory and invalidates on mutation), and without something equivalent a
// busy server pays a Postgres round trip per message purely to learn that
// nothing has changed since the last one.
//
// Short rather than event-driven, because the two mechanisms available here
// are not equal. Invalidation on this process's own mutations is exact and
// is what cachingStore does below. The case a TTL covers is a row changed
// from outside this process, which today means a hand-edited database, and
// ten seconds of staleness on that is not worth an EventConfigChanged
// subscription and the ordering questions it brings.
//
// Ten seconds is chosen against the failure it allows: an admin who runs
// /aimod configure mode off during an incident sees it take effect
// immediately (their own command invalidates), and anybody watching from
// outside waits at most this long.
const configTTL = 10 * time.Second

// cachingStore is a Store that remembers Config per guild and drops the
// entry whenever anything is written for that guild.
//
// A decorator rather than a cache inside pgStore, so the caching is visible
// at the wiring point in cmd/bot/main.go rather than hidden in the thing
// that talks to Postgres, and so the tests can use the bare store.
//
// Every mutating method is overridden purely to invalidate. That is
// mechanical and it is the one thing to remember when adding a method to
// Store: a new setter that is not listed here keeps serving a stale config
// for up to configTTL. The TTL is what stops that being a permanent bug
// rather than a brief one, which is most of why it exists.
type cachingStore struct {
	Store

	now func() time.Time

	mu      sync.RWMutex
	entries map[string]cachedConfig
}

type cachedConfig struct {
	cfg    Config
	readAt time.Time
}

// NewCachingStore wraps s so guild config is not re-read from Postgres on
// every message.
func NewCachingStore(s Store) Store {
	return &cachingStore{
		Store:   s,
		now:     func() time.Time { return time.Now().UTC() },
		entries: make(map[string]cachedConfig),
	}
}

func (c *cachingStore) Config(ctx context.Context, guildID string) (Config, error) {
	now := c.now()

	c.mu.RLock()
	e, ok := c.entries[guildID]
	c.mu.RUnlock()
	if ok && now.Sub(e.readAt) < configTTL {
		return e.cfg, nil
	}

	cfg, err := c.Store.Config(ctx, guildID)
	if err != nil {
		// Deliberately not cached, and deliberately not served stale either.
		// A read error is the caller's to handle: checkBudget turns it into
		// a refusal to spend, which is the fail-closed direction, and
		// quietly handing back an older config would take that decision
		// away from it.
		return Config{}, err
	}

	c.mu.Lock()
	c.entries[guildID] = cachedConfig{cfg: cfg, readAt: now}
	c.mu.Unlock()
	return cfg, nil
}

func (c *cachingStore) invalidate(guildID string) {
	c.mu.Lock()
	delete(c.entries, guildID)
	c.mu.Unlock()
}

// Forget drops a guild's cached config outright, for when the bot leaves it.
func (c *cachingStore) Forget(guildID string) { c.invalidate(guildID) }

func (c *cachingStore) SetAPIKey(ctx context.Context, guildID, provider string, sealed []byte) error {
	defer c.invalidate(guildID)
	return c.Store.SetAPIKey(ctx, guildID, provider, sealed)
}

func (c *cachingStore) SetMode(ctx context.Context, guildID string, mode Mode) error {
	defer c.invalidate(guildID)
	return c.Store.SetMode(ctx, guildID, mode)
}

func (c *cachingStore) SetBudget(ctx context.Context, guildID string, usd float64) error {
	defer c.invalidate(guildID)
	return c.Store.SetBudget(ctx, guildID, usd)
}

func (c *cachingStore) SetEvidenceHours(ctx context.Context, guildID string, hours int) error {
	defer c.invalidate(guildID)
	return c.Store.SetEvidenceHours(ctx, guildID, hours)
}

func (c *cachingStore) SetModels(ctx context.Context, guildID string, fast, deep []string) error {
	defer c.invalidate(guildID)
	return c.Store.SetModels(ctx, guildID, fast, deep)
}

func (c *cachingStore) SetBucketAction(ctx context.Context, guildID string, bucket Bucket, action Action) error {
	defer c.invalidate(guildID)
	return c.Store.SetBucketAction(ctx, guildID, bucket, action)
}

func (c *cachingStore) SetExemptChannels(ctx context.Context, guildID string, ids []string) error {
	defer c.invalidate(guildID)
	return c.Store.SetExemptChannels(ctx, guildID, ids)
}

func (c *cachingStore) SetExemptRoles(ctx context.Context, guildID string, ids []string) error {
	defer c.invalidate(guildID)
	return c.Store.SetExemptRoles(ctx, guildID, ids)
}

func (c *cachingStore) SetSanctionAction(ctx context.Context, guildID string, action SanctionAction) error {
	defer c.invalidate(guildID)
	return c.Store.SetSanctionAction(ctx, guildID, action)
}

func (c *cachingStore) SetSanctionOptIn(ctx context.Context, guildID string, userIDs []string) error {
	defer c.invalidate(guildID)
	return c.Store.SetSanctionOptIn(ctx, guildID, userIDs)
}

func (c *cachingStore) SetCalibration(ctx context.Context, guildID string, active, pending []CalibrationExample, ranAt time.Time) error {
	defer c.invalidate(guildID)
	return c.Store.SetCalibration(ctx, guildID, active, pending, ranAt)
}

func (c *cachingStore) SetCalibrationMode(ctx context.Context, guildID string, mode CalibrationMode) error {
	defer c.invalidate(guildID)
	return c.Store.SetCalibrationMode(ctx, guildID, mode)
}

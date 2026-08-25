package aimod

import (
	"context"
	"errors"
	"testing"
	"time"
)

// countingStore counts Config reads so the cache can be observed doing its
// job, which is the only observable thing about it.
type countingStore struct {
	Store
	reads int
}

func (c *countingStore) Config(ctx context.Context, guildID string) (Config, error) {
	c.reads++
	return c.Store.Config(ctx, guildID)
}

func newCaching(t *testing.T) (*cachingStore, *countingStore) {
	t.Helper()
	counting := &countingStore{Store: newFakeStore()}
	c := NewCachingStore(counting).(*cachingStore)
	c.now = func() time.Time { return testNow }
	return c, counting
}

// The point of the whole file: a busy guild must not pay a Postgres round
// trip per message to learn that nothing changed.
func TestConfigIsReadOncePerWindow(t *testing.T) {
	c, counting := newCaching(t)

	for range 50 {
		if _, err := c.Config(context.Background(), "g1"); err != nil {
			t.Fatalf("Config: %v", err)
		}
	}
	if counting.reads != 1 {
		t.Errorf("read the store %d times for 50 messages, want 1", counting.reads)
	}

	// A different guild is its own entry.
	if _, err := c.Config(context.Background(), "g2"); err != nil {
		t.Fatalf("Config: %v", err)
	}
	if counting.reads != 2 {
		t.Errorf("reads = %d, want a second guild to be read separately", counting.reads)
	}
}

func TestConfigCacheExpires(t *testing.T) {
	c, counting := newCaching(t)
	now := testNow
	c.now = func() time.Time { return now }

	if _, err := c.Config(context.Background(), "g1"); err != nil {
		t.Fatalf("Config: %v", err)
	}
	now = now.Add(configTTL - time.Second)
	if _, err := c.Config(context.Background(), "g1"); err != nil {
		t.Fatalf("Config: %v", err)
	}
	if counting.reads != 1 {
		t.Errorf("reads = %d inside the window, want 1", counting.reads)
	}

	now = now.Add(2 * time.Second)
	if _, err := c.Config(context.Background(), "g1"); err != nil {
		t.Fatalf("Config: %v", err)
	}
	if counting.reads != 2 {
		t.Errorf("reads = %d past the window, want the config re-read", counting.reads)
	}
}

// An admin turning the plugin off during an incident must see it take effect
// now, not in ten seconds. Their own command is what invalidates.
func TestMutationsInvalidateImmediately(t *testing.T) {
	c, counting := newCaching(t)

	if _, err := c.Config(context.Background(), "g1"); err != nil {
		t.Fatalf("Config: %v", err)
	}
	if err := c.SetMode(context.Background(), "g1", ModeEnforce); err != nil {
		t.Fatalf("SetMode: %v", err)
	}

	cfg, err := c.Config(context.Background(), "g1")
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if counting.reads != 2 {
		t.Errorf("reads = %d, want the write to have dropped the cached copy", counting.reads)
	}
	if cfg.Mode != ModeEnforce {
		t.Errorf("Mode = %q after SetMode, want enforce: the cache served a stale config", cfg.Mode)
	}
}

// Every setter has to invalidate, and this is the check that catches one
// being added to Store without being listed in cachingStore. It exercises
// them rather than reflecting over the interface, so a new method shows up
// as an obvious gap here rather than a silent one in production.
func TestEverySetterInvalidates(t *testing.T) {
	ctx := context.Background()
	setters := map[string]func(*cachingStore) error{
		"SetAPIKey":         func(c *cachingStore) error { return c.SetAPIKey(ctx, "g1", []byte("x")) },
		"SetMode":           func(c *cachingStore) error { return c.SetMode(ctx, "g1", ModeFlag) },
		"SetBudget":         func(c *cachingStore) error { return c.SetBudget(ctx, "g1", 1) },
		"SetEvidenceHours":  func(c *cachingStore) error { return c.SetEvidenceHours(ctx, "g1", 1) },
		"SetModels":         func(c *cachingStore) error { return c.SetModels(ctx, "g1", nil, nil) },
		"SetBucketAction":   func(c *cachingStore) error { return c.SetBucketAction(ctx, "g1", BucketGore, ActionFlag) },
		"SetExemptChannels": func(c *cachingStore) error { return c.SetExemptChannels(ctx, "g1", nil) },
		"SetExemptRoles":    func(c *cachingStore) error { return c.SetExemptRoles(ctx, "g1", nil) },
		"SetSanctionAction": func(c *cachingStore) error { return c.SetSanctionAction(ctx, "g1", SanctionJail) },
		"SetSanctionOptIn":  func(c *cachingStore) error { return c.SetSanctionOptIn(ctx, "g1", nil) },
	}

	for name, set := range setters {
		t.Run(name, func(t *testing.T) {
			c, counting := newCaching(t)
			if _, err := c.Config(ctx, "g1"); err != nil {
				t.Fatalf("Config: %v", err)
			}
			if err := set(c); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if _, err := c.Config(ctx, "g1"); err != nil {
				t.Fatalf("Config: %v", err)
			}
			if counting.reads != 2 {
				t.Errorf("%s did not invalidate the cached config", name)
			}
		})
	}
}

// A read error must reach the caller rather than being papered over with an
// older copy. checkBudget turns it into a refusal to spend, and serving
// stale config would take that decision away from it.
func TestReadErrorsAreNotCachedOrMasked(t *testing.T) {
	inner := newFakeStore()
	counting := &countingStore{Store: inner}
	c := NewCachingStore(counting).(*cachingStore)
	c.now = func() time.Time { return testNow }

	if _, err := c.Config(context.Background(), "g1"); err != nil {
		t.Fatalf("Config: %v", err)
	}
	c.Forget("g1")

	inner.configErr = errors.New("database is down")
	if _, err := c.Config(context.Background(), "g1"); err == nil {
		t.Error("a failed read was masked by the cache")
	}

	// And the failure is not itself cached: the next call tries again.
	inner.configErr = nil
	if _, err := c.Config(context.Background(), "g1"); err != nil {
		t.Errorf("Config after recovery: %v", err)
	}
}

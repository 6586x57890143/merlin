// Package voice is where Merlin's own words live.
//
// Two things make this a package rather than string literals at their call
// sites. The first is variety: a bot that says the identical sentence every
// rotation stops reading as a character and starts reading as furniture.
// The second, and the reason the validation is as strict as it is, is that
// some of what she says is load bearing. The rotation notice is the
// server's published retention policy, and personality is never allowed to
// write the retention window out of it.
//
// The split is deliberate. The lines are data (lines/*.yaml, embedded) so
// they can be reviewed as writing. The contract is code (keys.go) so it
// cannot drift. A line that breaks the contract fails startup rather than
// reaching a channel, in the same spirit as core.CommandRouter.Finalize
// refusing to boot on a command with no declared tier.
package voice

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
)

// Source is where a line comes from.
//
// Consumers depend on this rather than on *Speaker so that a generator, or
// a live model, can be slotted in later as a constructor change and nothing
// else. Whatever ends up behind it still has to pass Validate before its
// output reaches a channel, which is the part that makes "add an LLM later"
// a change of source rather than a second unchecked path to two thousand
// people.
//
// Line never fails. Every call site is about to post something, and an
// error there just means the message silently does not happen; the
// compiled-in fallback in each spec is a better answer than nothing.
type Source interface {
	Line(ctx context.Context, guildID string, key Key, vars map[string]string) string
}

// Speaker serves lines from the validated catalog.
type Speaker struct {
	cat catalog
	log *slog.Logger

	// rnd is injected so tests are deterministic, mirroring the fake clock
	// the Scheduler's own tests use rather than inventing a second pattern.
	rnd func(n int) int

	mu sync.Mutex
	// last remembers the index used per guild and key, so the same channel
	// does not see the same intro twice running. Bounded by guilds times
	// keys, which is small enough that nothing needs evicting.
	last map[string]int
}

// Option configures a Speaker.
type Option func(*Speaker)

// WithRand replaces the random source. n is always at least 1, and the
// returned value must be in [0, n).
func WithRand(fn func(n int) int) Option {
	return func(s *Speaker) { s.rnd = fn }
}

// New loads and validates the embedded catalog. An invalid catalog is a
// startup failure, never a degraded runtime.
func New(log *slog.Logger, opts ...Option) (*Speaker, error) {
	cat, err := loadCatalog()
	if err != nil {
		return nil, err
	}
	s := &Speaker{
		cat:  cat,
		log:  log,
		rnd:  rand.IntN,
		last: map[string]int{},
	}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// Line returns something to say for key, with vars substituted.
//
// It cannot fail. Startup validation has already established that every
// catalog line renders given the placeholders its spec declares, so the
// fallback path below is reachable only if a caller passes the wrong vars,
// which is a programming error that TestEveryKeyRendersWithItsDeclaredVars
// is there to catch before it ships.
func (s *Speaker) Line(_ context.Context, guildID string, key Key, vars map[string]string) string {
	sp, known := specs[key]
	if !known {
		// Nothing sensible to say and no fallback to reach for. Returning
		// empty lets the caller skip rather than post a broken sentence.
		s.log.Error("voice: no such key", "key", key)
		return ""
	}

	if line, ok := render(s.pick(guildID, key), vars); ok {
		return line
	}

	line, ok := render(sp.fallback, vars)
	if !ok {
		// The fallback could not render either, so the caller is missing a
		// placeholder the spec requires. Say the plainest true thing rather
		// than posting text with visible braces in it.
		s.log.Error("voice: missing required placeholders, and the fallback needs them too",
			"key", key, "vars", len(vars))
		return ""
	}
	s.log.Warn("voice: fell back to the compiled-in line", "key", key)
	return line
}

// pick chooses a line for key, never the one used last time in this guild.
//
// One re-roll, then a deterministic step. The re-roll keeps the
// distribution close to uniform in the ordinary case; the step is what
// makes "not the same twice running" a guarantee rather than a likelihood,
// which matters because the failure is not rare enough to ignore. With
// five lines a plain re-roll still repeats about one time in twenty five,
// and the one place people would notice is a channel they are watching.
func (s *Speaker) pick(guildID string, key Key) string {
	lines := s.cat[key]
	switch len(lines) {
	case 0:
		return specs[key].fallback
	case 1:
		return lines[0]
	}

	memo := guildID + "\x00" + string(key)

	s.mu.Lock()
	defer s.mu.Unlock()

	prev, seen := s.last[memo]
	i := s.rnd(len(lines))
	if seen && i == prev {
		if i = s.rnd(len(lines)); i == prev {
			i = (prev + 1) % len(lines)
		}
	}
	s.last[memo] = i
	return lines[i]
}

// Keys returns every key the catalog knows, for tests and for any future
// tooling that wants to enumerate what this bot can say.
func Keys() []Key {
	out := make([]Key, 0, len(specs))
	for k := range specs {
		out = append(out, k)
	}
	return out
}

// RequiredVars reports the placeholders every line for key must contain.
// Exported so a caller can be checked against the contract in a test
// instead of by reading the table.
func RequiredVars(key Key) []string {
	return append([]string(nil), specs[key].required...)
}

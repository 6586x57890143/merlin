package voice

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func testSpeaker(t *testing.T, opts ...Option) *Speaker {
	t.Helper()
	s, err := New(slog.New(slog.NewTextHandler(io.Discard, nil)), opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// sampleVars returns a plausible value for every placeholder a key
// requires, so a test can render any key without knowing which one it is.
func sampleVars(key Key) map[string]string {
	vars := map[string]string{}
	for _, name := range RequiredVars(key) {
		switch name {
		case "cadence":
			vars[name] = "24 hours"
		case "retention":
			vars[name] = "7 days"
		case "when":
			vars[name] = "10 minutes"
		case "guild":
			vars[name] = "The Melting Pot"
		case "until":
			vars[name] = "in 3 hours"
		default:
			vars[name] = "value-for-" + name
		}
	}
	return vars
}

// The shipped catalog has to load. This is the test that turns every rule
// in Validate into something enforced at build time rather than a comment.
func TestEmbeddedCatalogIsValid(t *testing.T) {
	if _, err := loadCatalog(); err != nil {
		t.Fatalf("the shipped catalog does not pass its own validation:\n%v", err)
	}
}

// Variety is the feature, so it gets an assertion rather than a hope. The
// floor is the contract; the keys a member sees hourly carry well above it,
// and this reports the real numbers on failure so whoever trips it can see
// which key got thin rather than guessing.
func TestEveryKeyCarriesEnoughPermutations(t *testing.T) {
	s := testSpeaker(t)

	for _, key := range Keys() {
		lines := s.cat[key]
		if len(lines) < minLinesPerKey {
			t.Errorf("%s has %d lines, below the floor of %d", key, len(lines), minLinesPerKey)
		}
		// Duplicates pass the count while quietly costing a permutation and
		// making the repeated line twice as likely as its neighbours.
		seen := map[string]int{}
		for i, line := range lines {
			if j, dup := seen[line]; dup {
				t.Errorf("%s[%d] is identical to [%d]: %q", key, i, j, line)
			}
			seen[line] = i
		}
	}
}

// Every key must render with exactly the placeholders its spec declares. A
// caller passing the wrong vars is the one runtime failure startup
// validation cannot catch, so it gets caught here instead.
func TestEveryKeyRendersWithItsDeclaredVars(t *testing.T) {
	s := testSpeaker(t)

	for _, key := range Keys() {
		t.Run(string(key), func(t *testing.T) {
			vars := sampleVars(key)
			// Exercise every line, not just whichever one the RNG picks,
			// or a single broken line hides behind its neighbours.
			for i, line := range s.cat[key] {
				got, ok := render(line, vars)
				if !ok {
					t.Errorf("line %d does not render with %v: %q", i, vars, line)
				}
				if strings.ContainsAny(got, "{}") {
					t.Errorf("line %d still has braces after rendering: %q", i, got)
				}
			}
		})
	}
}

// The whole point of the required-placeholder rule. Rotation's notice is
// the server's published retention policy, and the code has already once
// reported the cadence as though it were the deletion window. No amount of
// rewriting for tone is allowed to drop either fact.
func TestEveryRotationIntroStatesItsRetentionFacts(t *testing.T) {
	s := testSpeaker(t)

	for i, line := range s.cat[KeyRotationIntroFull] {
		if !strings.Contains(line, "{cadence}") {
			t.Errorf("rotation.intro.kept[%d] never states how often the channel resets: %q", i, line)
		}
		if !strings.Contains(line, "{retention}") {
			t.Errorf("rotation.intro.kept[%d] never states how long the archive survives: %q", i, line)
		}
	}
	for i, line := range s.cat[KeyRotationIntroFullForever] {
		if !strings.Contains(line, "{cadence}") {
			t.Errorf("rotation.intro.forever[%d] never states how often the channel resets: %q", i, line)
		}
	}
}

// A line that drops a required placeholder must fail validation, not merely
// be discouraged by a comment. This is the guard that would have caught the
// original cadence-as-retention bug.
func TestValidateRejectsALineMissingItsFacts(t *testing.T) {
	cases := []struct {
		name string
		key  Key
		line string
		want string
	}{
		{
			name: "retention silently dropped",
			key:  KeyRotationIntroFull,
			line: "fresh channel, resets every {cadence} 🌿",
			want: "missing required placeholder {retention}",
		},
		{
			name: "placeholder typo",
			key:  KeyRotationIntroFullForever,
			line: "resets every {cadance}",
			want: "unknown placeholder {cadance}",
		},
		{
			name: "malformed placeholder posts a literal brace",
			key:  KeyRotationIntroFullForever,
			line: "resets every {cadence",
			want: "unbalanced braces",
		},
		{
			name: "em dash",
			key:  KeyPing,
			// Written as an escape, not the character, so this file does
			// not trip the repository-wide CI check it is testing a
			// package-level version of.
			line: "kik-ong " + string(rune(0x2014)) + " still here",
			want: "em dash",
		},
		{
			name: "empty",
			key:  KeyPing,
			line: "   ",
			want: "line is empty",
		},
		{
			name: "stray whitespace",
			key:  KeyPing,
			line: "kik-ong! ",
			want: "leading or trailing whitespace",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			problems := Validate(c.key, c.line)
			if len(problems) == 0 {
				t.Fatalf("Validate accepted %q", c.line)
			}
			if !strings.Contains(strings.Join(problems, "; "), c.want) {
				t.Errorf("Validate reported %v, want something mentioning %q", problems, c.want)
			}
		})
	}
}

// A valid line must actually pass, or the validator is just a way of
// rejecting everything.
func TestValidateAcceptsAGoodLine(t *testing.T) {
	if problems := Validate(KeyRotationIntroFull,
		"clean slate. wipes every {cadence}, archives last {retention} and then they're gone"); len(problems) != 0 {
		t.Errorf("Validate rejected a good line: %v", problems)
	}
}

// Nobody should see the same intro twice running in the same channel. That
// is the difference between a bot with a voice and a bot with a string
// constant.
func TestConsecutiveLinesDiffer(t *testing.T) {
	// An RNG that always returns the same index is the worst case: without
	// the re-roll, every call would return the identical line forever.
	stuck := func(int) int { return 2 }
	s := testSpeaker(t, WithRand(stuck))

	first := s.pick("g1", KeyPing)
	second := s.pick("g1", KeyPing)
	if first == second {
		t.Errorf("said %q twice in a row despite %d lines being available", first, len(s.cat[KeyPing]))
	}
}

// The no-repeat memory is per guild and per key: one server's rotation must
// not influence what another server hears, and a run of /ping must not
// affect the next rotation notice.
func TestRepeatMemoryIsScopedPerGuildAndKey(t *testing.T) {
	calls := 0
	// Alternates 0, 1, 0, 1... so a shared memo would visibly interfere.
	seq := func(int) int { calls++; return calls % 2 }
	s := testSpeaker(t, WithRand(seq))

	s.pick("g1", KeyPing)
	before := len(s.last)
	s.pick("g2", KeyPing)
	s.pick("g1", KeyDenied)
	if len(s.last) != before+2 {
		t.Errorf("memo entries = %d, want %d; the guild and key are not both part of the memory key", len(s.last), before+2)
	}
}

// With a single line there is nothing to alternate with, and the re-roll
// must not spin or panic.
func TestSingleLineKeyIsStable(t *testing.T) {
	s := testSpeaker(t)
	s.cat["only-one"] = []string{"just this"}
	specs["only-one"] = spec{maxLen: maxMessageContent, fallback: "just this"}
	defer delete(specs, "only-one")

	for range 3 {
		if got := s.pick("g1", "only-one"); got != "just this" {
			t.Fatalf("got %q", got)
		}
	}
}

// Line must never hand back text with visible braces in it. If the caller
// forgets a var, the compiled-in fallback is the answer, and if that cannot
// render either then saying nothing beats saying "{cadence}" to the server.
func TestLineNeverLeaksAPlaceholder(t *testing.T) {
	s := testSpeaker(t)
	ctx := context.Background()

	// Missing retention: the fallback needs it too, so this is the
	// say-nothing case rather than the fall-back case.
	got := s.Line(ctx, "g1", KeyRotationIntroFull, map[string]string{"cadence": "24 hours"})
	if strings.ContainsAny(got, "{}") {
		t.Errorf("leaked a placeholder to the channel: %q", got)
	}

	// Fully supplied: a real line, no braces.
	got = s.Line(ctx, "g1", KeyRotationIntroFull, sampleVars(KeyRotationIntroFull))
	if got == "" || strings.ContainsAny(got, "{}") {
		t.Errorf("expected a rendered line, got %q", got)
	}
	if !strings.Contains(got, "24 hours") || !strings.Contains(got, "7 days") {
		t.Errorf("rendered line dropped one of its facts: %q", got)
	}
}

// An unknown key cannot say anything sensible and must not guess.
func TestUnknownKeySaysNothing(t *testing.T) {
	s := testSpeaker(t)
	if got := s.Line(context.Background(), "g1", "no.such.key", nil); got != "" {
		t.Errorf("invented %q for an unknown key", got)
	}
}

// Speaker is the catalog implementation of Source. If this stops compiling,
// swapping in a generator later stops being a constructor change.
var _ Source = (*Speaker)(nil)

// The guarantee has to hold across a long run, not just the first pair,
// and for every key that carries enough lines to have a choice.
func TestNoImmediateRepeatOverALongRun(t *testing.T) {
	s := testSpeaker(t)

	for _, key := range Keys() {
		if len(s.cat[key]) < 2 {
			continue
		}
		t.Run(string(key), func(t *testing.T) {
			prev := ""
			for i := range 200 {
				got := s.pick("g1", key)
				if got == prev {
					t.Fatalf("repeated %q at draw %d", got, i)
				}
				prev = got
			}
		})
	}
}

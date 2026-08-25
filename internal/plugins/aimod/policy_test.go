package aimod

import (
	"strings"
	"testing"
)

// The catalogue is the product here: these files are what a model is told
// Discord's rules are, and a gap in one of them is a gap in the filter that
// no amount of prompt engineering elsewhere recovers. So it is checked the
// way internal/voice checks its own, at the same startup-failing bar.

func TestPolicyCatalogueLoads(t *testing.T) {
	policies, err := LoadPolicies()
	if err != nil {
		t.Fatalf("LoadPolicies: %v", err)
	}
	for _, b := range AllBuckets {
		p, ok := policies[b]
		if !ok {
			t.Errorf("bucket %q has no policy file", b)
			continue
		}
		if p.Bucket != b {
			t.Errorf("file for %q declares bucket %q", b, p.Bucket)
		}
	}
	if len(policies) != len(AllBuckets) {
		t.Errorf("loaded %d policies, AllBuckets lists %d", len(policies), len(AllBuckets))
	}
}

// Every bucket needs a severity the sanction ladder recognises. An
// unrecognised one silently falls back to the middle of the ladder, which is
// a defensible runtime behaviour and a bad thing to ship on purpose.
func TestEveryPolicyHasALadderSeverity(t *testing.T) {
	policies, err := LoadPolicies()
	if err != nil {
		t.Fatalf("LoadPolicies: %v", err)
	}
	for b, p := range policies {
		if _, ok := severityBase[p.Severity]; !ok {
			t.Errorf("bucket %q has severity %q, which is not in severityBase", b, p.Severity)
		}
	}
}

// The fast pass sends one line per enforced bucket and nothing else, so
// Short is the entire budget for describing a rule at that tier. A long one
// is a cost regression on every single scanned message.
func TestPolicyShortLinesStayShort(t *testing.T) {
	const maxShort = 160
	policies, err := LoadPolicies()
	if err != nil {
		t.Fatalf("LoadPolicies: %v", err)
	}
	for b, p := range policies {
		if len(p.Short) > maxShort {
			t.Errorf("bucket %q short is %d bytes, want at most %d: it is sent on every scanned batch", b, len(p.Short), maxShort)
		}
	}
}

func TestValidatePolicyRejectsAThinBoundary(t *testing.T) {
	// A policy with a full violations list and a token boundary is the exact
	// shape an over-broad filter ships as, so it has to fail the same way a
	// missing violations list does.
	p := Policy{
		Bucket:        BucketHateSpeech,
		Severity:      "high",
		Short:         "something",
		Definitions:   []string{"a definition"},
		Violations:    []string{"one", "two", "three", "four"},
		NotViolations: []string{"only one"},
	}
	problems := validatePolicy(p)
	if len(problems) == 0 {
		t.Fatal("a policy with one not_violations line was accepted")
	}
	if !strings.Contains(strings.Join(problems, " "), "not_violations") {
		t.Errorf("problems did not mention not_violations: %v", problems)
	}
}

func TestValidatePolicyRejectsUnknownBucket(t *testing.T) {
	p := Policy{
		Bucket:        Bucket("nonsense"),
		Short:         "something",
		Definitions:   []string{"d"},
		Violations:    []string{"a", "b", "c"},
		NotViolations: []string{"a", "b", "c"},
	}
	if len(validatePolicy(p)) == 0 {
		t.Fatal("a policy naming a bucket outside AllBuckets was accepted")
	}
}

// CI rejects these characters across the repo, and these strings are
// interpolated into prompts and echoed into Discord embeds, so they are held
// to the same bar. Checked against the real files, not a synthetic one.
func TestPolicyFilesUsePlainPunctuation(t *testing.T) {
	entries, err := policyFS.ReadDir("policy")
	if err != nil {
		t.Fatalf("read policy dir: %v", err)
	}
	for _, e := range entries {
		raw, err := policyFS.ReadFile("policy/" + e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, bad := range generatedPunctuation {
			if strings.ContainsRune(string(raw), bad) {
				t.Errorf("%s contains %q, use plain punctuation", e.Name(), bad)
			}
		}
	}
}

// child_safety is the one bucket with no off switch. If this ever starts
// passing for another value, somebody has made the setting configurable and
// the guard in handlePolicySet is the only thing left standing.
func TestChildSafetyCannotBeTurnedOff(t *testing.T) {
	for _, a := range Actions {
		overrides := map[Bucket]Action{BucketChildSafety: a}
		if got := EffectiveAction(overrides, BucketChildSafety); got != ActionRemove {
			t.Errorf("stored override %q produced %q, want remove: child safety is not configurable", a, got)
		}
	}
	// Including via a hand-edited row holding something that is not an
	// action at all.
	if got := EffectiveAction(map[Bucket]Action{BucketChildSafety: "off "}, BucketChildSafety); got != ActionRemove {
		t.Errorf("a corrupt stored value produced %q, want remove", got)
	}
}

func TestEffectiveActionDefaults(t *testing.T) {
	// The free-speech posture, asserted rather than assumed: the buckets
	// that get a server terminated are on, and the ones that are a question
	// about the room are off until a guild says otherwise.
	on := []Bucket{BucketViolentExtremism, BucketThreats, BucketDoxxing, BucketNCII, BucketMalicious}
	off := []Bucket{BucketHateSpeech, BucketGore, BucketSelfHarm, BucketSpam}

	for _, b := range on {
		if got := EffectiveAction(nil, b); got != ActionRemove {
			t.Errorf("default for %q is %q, want remove", b, got)
		}
	}
	for _, b := range off {
		if got := EffectiveAction(nil, b); got != ActionOff {
			t.Errorf("default for %q is %q, want off", b, got)
		}
	}
}

func TestEffectiveActionHonoursOverrides(t *testing.T) {
	overrides := map[Bucket]Action{BucketHateSpeech: ActionFlag, BucketThreats: ActionOff}
	if got := EffectiveAction(overrides, BucketHateSpeech); got != ActionFlag {
		t.Errorf("hate_speech = %q, want flag", got)
	}
	if got := EffectiveAction(overrides, BucketThreats); got != ActionOff {
		t.Errorf("threats = %q, want off", got)
	}
}

// An unknown bucket can only reach EffectiveAction from a model inventing
// one or a hand-edited row. Off, never flag: acting on a category this build
// has no definition for is acting on nothing in particular.
func TestUnknownBucketIsOff(t *testing.T) {
	if got := EffectiveAction(nil, Bucket("invented_by_a_model")); got != ActionOff {
		t.Errorf("unknown bucket = %q, want off", got)
	}
}

// ActionSanction marks a row about a member, not a message. Offering it in
// /aimod policy set would let a guild configure a bucket that jails somebody
// while leaving what they posted exactly where it was.
func TestSanctionIsNotAConfigurableAction(t *testing.T) {
	for _, a := range Actions {
		if a == ActionSanction {
			t.Fatal("ActionSanction is in Actions, so /aimod policy set will offer it as a choice")
		}
	}
}

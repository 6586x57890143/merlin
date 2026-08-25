package aimod

import (
	"embed"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Bucket names one policy area this plugin can enforce. The ten of them
// collapse Discord's twenty-seven Community Guidelines into the groups a
// moderator actually configures: nobody wants a switch for rule 16 on its
// own, and nobody wants child safety sharing a switch with anything.
type Bucket string

const (
	BucketChildSafety      Bucket = "child_safety"
	BucketViolentExtremism Bucket = "violent_extremism"
	BucketThreats          Bucket = "threats"
	BucketDoxxing          Bucket = "doxxing"
	BucketNCII             Bucket = "ncii"
	BucketMalicious        Bucket = "malicious"
	BucketHateSpeech       Bucket = "hate_speech"
	BucketGore             Bucket = "gore"
	BucketSelfHarm         Bucket = "self_harm"
	BucketSpam             Bucket = "spam"
)

// Action is what happens to a message that lands in a bucket.
type Action string

const (
	// ActionOff means the bucket is not enforced at all: the fast pass is
	// not even told to look for it, which also makes turning buckets off a
	// token saving rather than just a policy choice.
	ActionOff Action = "off"
	// ActionFlag records an incident and posts to the audit log, leaving the
	// message alone. The safe setting to run a new guild on for a week.
	ActionFlag Action = "flag"
	// ActionRewrite deletes the message and reposts a cleaned version of it
	// through a webhook wearing the author's name and avatar.
	ActionRewrite Action = "rewrite"
	// ActionRemove deletes the message.
	ActionRemove Action = "remove"
	// ActionSanction is not something that happens to a message. It marks an
	// incident row recording what happened to the *member*, written by the
	// escalation ladder in sanction.go and counted by their next offence.
	// Deliberately absent from Actions below, so /aimod policy set can never
	// offer it: a bucket set to "sanction" would jail somebody while leaving
	// what they posted exactly where it was.
	ActionSanction Action = "sanction"
)

// Actions lists every action in escalating order, for command choices.
var Actions = []Action{ActionOff, ActionFlag, ActionRewrite, ActionRemove}

func (a Action) valid() bool { return slices.Contains(Actions, a) }

// acts reports whether a touches the message rather than only recording it.
func (a Action) acts() bool { return a == ActionRewrite || a == ActionRemove }

// defaultActions is what a guild that has configured nothing enforces.
//
// The split is the free-speech posture spec.MD is written around, made
// concrete: on by default is the set that gets a *server* terminated or
// mass-reported off the platform, and off by default is the set that is
// really a question about what kind of room this is. A guild that wants the
// second set turns it on with /aimod policy set, which is a decision worth
// making on purpose rather than inheriting from a default nobody chose.
//
// child_safety is not in this map because it is not configurable; see
// EffectiveAction.
var defaultActions = map[Bucket]Action{
	BucketViolentExtremism: ActionRemove,
	BucketThreats:          ActionRemove,
	BucketDoxxing:          ActionRemove,
	BucketNCII:             ActionRemove,
	BucketMalicious:        ActionRemove,

	BucketHateSpeech: ActionOff,
	BucketGore:       ActionOff,
	BucketSelfHarm:   ActionOff,
	BucketSpam:       ActionOff,
}

// EffectiveAction resolves what guildID does about bucket, given whatever
// overrides an admin has stored.
//
// child_safety always removes and ignores both the override and the stored
// value entirely. This is the same shape as adminconfig refusing to let
// /config plugins set disable adminconfig, and for the same reason: the
// setting exists to be changed, but this particular change has exactly one
// outcome, which is that a guild removes its own last defence against the
// one category that ends servers and carries a reporting obligation. There
// is no legitimate reason to turn it off, so there is no way to.
func EffectiveAction(overrides map[Bucket]Action, bucket Bucket) Action {
	if bucket == BucketChildSafety {
		return ActionRemove
	}
	if a, ok := overrides[bucket]; ok && a.valid() {
		return a
	}
	if a, ok := defaultActions[bucket]; ok {
		return a
	}
	// An unknown bucket can only come from a model inventing one or from a
	// hand-edited row. Off, not flag: acting on a category this build does
	// not have a definition for means acting on nothing in particular.
	return ActionOff
}

// Policy is one bucket's definition, loaded from policy/*.yaml.
//
// The lists are the point of the file. Violations tell a model what the rule
// covers; NotViolations tell it where the rule stops, and on this bot that
// second list is the one that matters. Discord's own explainers mostly do
// not state exceptions, so most of these are derived from the definitions'
// own qualifiers ("with the intention to cause harm", "intentional actions
// meant to cause distress") rather than invented as carve-outs. Without
// them a general purpose model reads "hate speech" as "rude" and this plugin
// becomes the thing it was built to protect the server from.
type Policy struct {
	Bucket        Bucket   `yaml:"bucket"`
	Severity      string   `yaml:"severity"`
	Short         string   `yaml:"short"`
	Definitions   []string `yaml:"definitions"`
	Violations    []string `yaml:"violations"`
	NotViolations []string `yaml:"not_violations"`
}

// The catalog is loaded once by LoadPolicies at startup and held on the
// Plugin, which fails the process rather than shipping a half-defined rule
// to a live server, exactly as voice.New does for the line catalog.
//
//go:embed policy/*.yaml
var policyFS embed.FS

// AllBuckets lists every bucket in a stable order: critical ones first, then
// the opt-in ones, so /aimod policy list reads top down by seriousness
// rather than alphabetically.
var AllBuckets = []Bucket{
	BucketChildSafety,
	BucketViolentExtremism,
	BucketThreats,
	BucketDoxxing,
	BucketNCII,
	BucketMalicious,
	BucketHateSpeech,
	BucketGore,
	BucketSelfHarm,
	BucketSpam,
}

// generatedPunctuation is the punctuation nobody types on a keyboard: em
// and en dashes, an ellipsis character, and curly quotes. CI rejects them
// across the whole repo, and these strings are interpolated into prompts
// and echoed into Discord embeds, so they are held to the same bar.
//
// Code points rather than the characters themselves, and that is the point:
// spelling them out would make the one validator that rejects them the only
// place in the repo they appear, and CI would fail on this very file. The
// repo-wide check solves its own version of this with byte escapes. Numeric
// literals rather than backslash-u escapes, because gofmt rewrites those
// back into the literal characters.
var generatedPunctuation = []rune{0x2014, 0x2013, 0x2026, 0x2018, 0x2019, 0x201c, 0x201d}

// minListItems is the floor on both lists. A bucket described by one line
// each way is a bucket nobody can predict the behaviour of, and a short
// not_violations list in particular is how a filter quietly becomes
// over-broad without anyone editing a line of code.
const minListItems = 3

// LoadPolicies reads and validates the embedded catalog. Call once at
// startup; every problem is reported at once rather than one per run.
func LoadPolicies() (map[Bucket]Policy, error) {
	entries, err := policyFS.ReadDir("policy")
	if err != nil {
		return nil, fmt.Errorf("aimod: read policy dir: %w", err)
	}

	loaded := make(map[Bucket]Policy, len(entries))
	var problems []string
	for _, e := range entries {
		name := path.Join("policy", e.Name())
		raw, err := policyFS.ReadFile(name)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		var p Policy
		if err := yaml.Unmarshal(raw, &p); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		for _, problem := range validatePolicy(p) {
			problems = append(problems, fmt.Sprintf("%s: %s", name, problem))
		}
		if _, dup := loaded[p.Bucket]; dup {
			problems = append(problems, fmt.Sprintf("%s: bucket %q is defined twice", name, p.Bucket))
			continue
		}
		loaded[p.Bucket] = p
	}

	for _, b := range AllBuckets {
		if _, ok := loaded[b]; !ok {
			problems = append(problems, fmt.Sprintf("bucket %q has no policy file", b))
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("aimod: policy catalog invalid:\n  %s", strings.Join(problems, "\n  "))
	}
	return loaded, nil
}

// known reports whether b is a bucket this build has a definition for.
func known(b Bucket) bool { return slices.Contains(AllBuckets, b) }

// validatePolicy is exported behaviour in all but name: it is the contract a
// policy file owes, and the reason the catalog is data rather than a string
// literal in a prompt builder. Mirrors voice.Validate.
func validatePolicy(p Policy) []string {
	var problems []string
	if p.Bucket == "" {
		return []string{"missing bucket name"}
	}
	if !known(p.Bucket) {
		problems = append(problems, fmt.Sprintf("bucket %q is not in AllBuckets", p.Bucket))
	}
	if strings.TrimSpace(p.Short) == "" {
		problems = append(problems, "missing short: it is the only line the fast pass ever sees")
	}
	if len(p.Definitions) == 0 {
		problems = append(problems, "no definitions")
	}
	if len(p.Violations) < minListItems {
		problems = append(problems, fmt.Sprintf("only %d violations, want at least %d", len(p.Violations), minListItems))
	}
	// Deliberately as hard a requirement as the violations list. A policy
	// file with no stated boundary is how this plugin turns into a censor.
	if len(p.NotViolations) < minListItems {
		problems = append(problems, fmt.Sprintf("only %d not_violations, want at least %d: a bucket with no stated boundary is over-broad by construction", len(p.NotViolations), minListItems))
	}

	all := append([]string{p.Short}, p.Definitions...)
	all = append(all, p.Violations...)
	all = append(all, p.NotViolations...)
	for _, line := range all {
		if strings.TrimSpace(line) != line {
			problems = append(problems, fmt.Sprintf("line has leading or trailing whitespace: %q", line))
		}
		// The same punctuation CI rejects across every .go file in this
		// repo. These strings are interpolated into prompts and echoed back
		// into Discord embeds, so they are held to the same bar.
		for _, bad := range generatedPunctuation {
			if strings.ContainsRune(line, bad) {
				problems = append(problems, fmt.Sprintf("line contains %q, use plain punctuation: %q", bad, line))
			}
		}
	}
	return problems
}

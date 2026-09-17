package rapsheet

import (
	"strings"

	"github.com/6586x57890143/merlin/internal/core"
)

// Category is what an entry was for.
//
// The ten aimod policy buckets, by the same names, so an aimod removal and a
// moderator's warning for the same thing land under the same heading and
// the sheet reads as one record rather than two. Plus two the platform's
// rules do not cover: a server's own rule, and everything else.
type Category string

const (
	CategoryChildSafety      Category = "child_safety"
	CategoryViolentExtremism Category = "violent_extremism"
	CategoryThreats          Category = "threats"
	CategoryDoxxing          Category = "doxxing"
	CategoryNCII             Category = "ncii"
	CategoryMalicious        Category = "malicious"
	CategoryHateSpeech       Category = "hate_speech"
	CategoryGore             Category = "gore"
	CategorySelfHarm         Category = "self_harm"
	CategorySpam             Category = "spam"
	CategoryServerRule       Category = "server_rule"
	CategoryOther            Category = "other"
)

// categories is the closed set, in the order the picker shows them: the
// platform's rules by severity, then the server's own.
var categories = []Category{
	CategoryChildSafety, CategoryViolentExtremism, CategoryThreats, CategoryDoxxing,
	CategoryNCII, CategoryMalicious, CategoryHateSpeech, CategoryGore, CategorySelfHarm,
	CategorySpam, CategoryServerRule, CategoryOther,
}

// defaultPoints is what one offence in each category is worth before a guild
// tunes it. The tiers mirror the severities in aimod's policy files, so a
// bucket that aimod jails for a day is also the one that moves the score
// most; the numbers themselves are chosen against defaultBands so that a
// single first offence in any category short of critical never reaches a
// jail band on its own.
var defaultPoints = map[Category]int{
	CategoryChildSafety:      100,
	CategoryViolentExtremism: 100,
	CategoryThreats:          100,
	CategoryDoxxing:          100,
	CategoryNCII:             100,
	CategoryMalicious:        100,
	CategoryHateSpeech:       50,
	CategorySelfHarm:         50,
	CategoryGore:             25,
	CategoryServerRule:       25,
	CategorySpam:             10,
	CategoryOther:            10,
}

// maxPoints bounds an explicit points override on /rapsheet warn and a
// configured per-category value. Two critical offences' worth: enough to
// express "this one was bad", not enough to put somebody at the top of the
// ladder with a typo.
const maxPoints = 200

// validCategory reports whether c is one this build knows.
func validCategory(c Category) bool {
	for _, k := range categories {
		if k == c {
			return true
		}
	}
	return false
}

// categoryLabel is the human form for pickers and embeds.
func categoryLabel(c Category) string {
	return strings.ReplaceAll(string(c), "_", " ")
}

// pointsFor resolves what an entry is worth at the moment it is written.
//
// Three rules, and the middle one is the one that matters. Notes,
// suggestions and reversals carry nothing. An automatic consequence (a jail
// aimod or the ladder applied, actor core.ActorSystem) carries nothing
// either, because the offence behind it was already scored when it was
// recorded, and counting both would charge a member twice for one message.
// Everything else, which is to say a human's decision, carries the
// category's points, unless the human said otherwise.
func pointsFor(cfg Config, kind Kind, category Category, actorID string, override int) int {
	switch kind {
	case KindNote, KindUnban, KindRelease, KindSuggestion:
		return 0
	case KindJail, KindTimeout, KindKick, KindBan:
		if actorID == core.ActorSystem {
			return 0
		}
	}
	if override > 0 {
		return min(override, maxPoints)
	}
	if v, ok := cfg.CategoryPoints[category]; ok {
		return v
	}
	if v, ok := defaultPoints[category]; ok {
		return v
	}
	return defaultPoints[CategoryOther]
}

package rapsheet

import (
	"errors"
	"math"
	"testing"
	"time"
)

// The score and the ladder are pure so their whole behaviour can be read
// off tables. These are those tables.

func TestDecayedHalvesEveryHalfLife(t *testing.T) {
	hl := 30 * 24 * time.Hour
	cases := []struct {
		age  time.Duration
		want float64
	}{
		{0, 100},
		{hl, 50},
		{2 * hl, 25},
		{3 * hl, 12.5},
		// A future-dated entry counts at full value, never more.
		{-time.Hour, 100},
	}
	for _, tc := range cases {
		if got := Decayed(100, tc.age, hl); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("Decayed(100, %s) = %v, want %v", tc.age, got, tc.want)
		}
	}
	if Decayed(0, 0, hl) != 0 || Decayed(-5, 0, hl) != 0 {
		t.Error("non-positive points should be worth nothing")
	}
	if Decayed(100, 0, 0) != 0 {
		t.Error("a zero half-life must not divide by zero")
	}
}

func TestScoreSkipsVoidedEntries(t *testing.T) {
	hl := 30 * 24 * time.Hour
	voided := testNow
	entries := []Entry{
		{Points: 50, CreatedAt: testNow},
		{Points: 50, CreatedAt: testNow.Add(-hl)},
		{Points: 1000, CreatedAt: testNow, VoidedAt: &voided},
		{Points: 0, CreatedAt: testNow},
	}
	if got := Score(entries, testNow, hl); math.Abs(got-75) > 1e-9 {
		t.Errorf("Score = %v, want 75 (50 now + 50 one half-life ago, voided ignored)", got)
	}
}

func TestLadderPicksTheHighestBandReached(t *testing.T) {
	cases := []struct {
		score float64
		band  int
		act   Action
	}{
		{0, 0, ActionNone},
		{24.9, 0, ActionNone},
		{25, 1, ActionNotice},
		{49.99, 1, ActionNotice},
		{50, 2, ActionJail},
		{100, 3, ActionJail},
		{199, 3, ActionJail},
		{200, 4, ActionBan},
		{400, 5, ActionBan},
		{100000, 5, ActionBan},
	}
	for _, tc := range cases {
		got := Ladder(tc.score, nil)
		if got.Band != tc.band || got.Action != tc.act {
			t.Errorf("Ladder(%v) = band %d %s, want band %d %s", tc.score, got.Band, got.Action, tc.band, tc.act)
		}
	}
	// The defaults never reach a permanent ban.
	for _, b := range defaultBands {
		if b.Action == ActionBan && b.Duration <= 0 {
			t.Error("defaultBands has a permanent ban")
		}
	}
	if err := ValidateBands(defaultBands); err != nil {
		t.Errorf("defaultBands do not validate: %v", err)
	}
}

func TestValidateBandsRefusesWhatTheLadderMustNeverDo(t *testing.T) {
	h := time.Hour
	cases := map[string][]Band{
		"first band not zero":        {{Min: 10, Action: ActionNone}},
		"first band not none":        {{Min: 0, Action: ActionNotice}},
		"out of order":               {{0, ActionNone, 0}, {50, ActionJail, h}, {25, ActionNotice, 0}},
		"duplicate min":              {{0, ActionNone, 0}, {50, ActionJail, h}, {50, ActionJail, 2 * h}},
		"permanent ban":              {{0, ActionNone, 0}, {50, ActionBan, 0}},
		"jail without duration":      {{0, ActionNone, 0}, {50, ActionJail, 0}},
		"timeout over discord's cap": {{0, ActionNone, 0}, {50, ActionTimeout, 29 * 24 * h}},
		"notice with a duration":     {{0, ActionNone, 0}, {50, ActionNotice, h}},
		"unknown action":             {{0, ActionNone, 0}, {50, Action("kick"), 0}},
	}
	for name, bands := range cases {
		if err := ValidateBands(bands); !errors.Is(err, ErrBadBands) {
			t.Errorf("%s: err = %v, want ErrBadBands", name, err)
		}
	}
	ok := []Band{{0, ActionNone, 0}, {20, ActionNotice, 0}, {40, ActionTimeout, h}, {80, ActionJail, 2 * h}, {160, ActionBan, 24 * h}}
	if err := ValidateBands(ok); err != nil {
		t.Errorf("a well-formed ladder was refused: %v", err)
	}
	if err := ValidateBands(nil); err != nil {
		t.Errorf("empty bands (use defaults) refused: %v", err)
	}
}

func TestActionStrengthOrdersConsequences(t *testing.T) {
	order := []Action{ActionNone, ActionNotice, ActionTimeout, ActionJail, ActionBan}
	for i := 1; i < len(order); i++ {
		if order[i].Strength() <= order[i-1].Strength() {
			t.Errorf("%s should be stronger than %s", order[i], order[i-1])
		}
	}
}

func TestPointsForRules(t *testing.T) {
	cfg := defaultConfig(testGuild)
	cfg.CategoryPoints[CategorySpam] = 40
	cases := []struct {
		name     string
		kind     Kind
		cat      Category
		actor    string
		override int
		want     int
	}{
		{"note carries nothing", KindNote, CategoryHateSpeech, modID, 0, 0},
		{"suggestion carries nothing", KindSuggestion, CategoryHateSpeech, modID, 0, 0},
		{"release carries nothing", KindRelease, CategoryHateSpeech, "system", 0, 0},
		{"automatic jail carries nothing", KindJail, CategoryHateSpeech, "system", 0, 0},
		{"a mod's jail carries the category", KindJail, CategoryHateSpeech, modID, 0, 50},
		{"warn takes the default", KindWarn, CategoryThreats, modID, 0, 100},
		{"warn takes the guild's tuning", KindWarn, CategorySpam, modID, 0, 40},
		{"override wins", KindWarn, CategorySpam, modID, 15, 15},
		{"override is capped", KindWarn, CategorySpam, modID, 9999, maxPoints},
		{"aimod removal carries the bucket", KindRemoval, CategoryChildSafety, "system", 0, 100},
		{"unknown category falls to other", KindWarn, Category("nonsense"), modID, 0, defaultPoints[CategoryOther]},
	}
	for _, tc := range cases {
		if got := pointsFor(cfg, tc.kind, tc.cat, tc.actor, tc.override); got != tc.want {
			t.Errorf("%s: pointsFor = %d, want %d", tc.name, got, tc.want)
		}
	}
	// Every category the picker offers has a default, or a warning in it
	// would silently score as "other".
	for _, c := range categories {
		if _, ok := defaultPoints[c]; !ok {
			t.Errorf("category %s has no default points", c)
		}
	}
}

func TestStandingPicksTheStrongestConsequenceInForce(t *testing.T) {
	in := func(d time.Duration) *time.Time { t := testNow.Add(d); return &t }
	entries := []Entry{
		{ID: 1, Kind: KindTimeout, EndsAt: in(time.Hour)},
		{ID: 2, Kind: KindJail, EndsAt: in(-time.Hour)}, // expired
		{ID: 3, Kind: KindJail, EndsAt: in(2 * time.Hour)},
		{ID: 4, Kind: KindWarn},
	}
	st, ok := standing(entries, testNow)
	if !ok || st.ID != 3 {
		t.Fatalf("standing = %+v %v, want the live jail (#3)", st, ok)
	}
	entries = append(entries, Entry{ID: 5, Kind: KindBan})
	if st, _ := standing(entries, testNow); st.ID != 5 {
		t.Errorf("a permanent ban should outrank a jail, got #%d", st.ID)
	}
	lifted := testNow
	entries[4].LiftedAt = &lifted
	if st, _ := standing(entries, testNow); st.ID != 3 {
		t.Errorf("a lifted ban is not standing, got #%d", st.ID)
	}
	if _, ok := standing([]Entry{{Kind: KindWarn}}, testNow); ok {
		t.Error("a warning is not a standing consequence")
	}
}

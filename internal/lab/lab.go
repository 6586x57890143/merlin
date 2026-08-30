// Package lab answers the questions cmd/lab puts to a browser, by calling the
// same functions the bot calls.
//
// The point of the whole exercise is that nothing here reimplements anything.
// A JavaScript rotation simulator would be a second copy of the schedule
// arithmetic and the disclosure rules, and a second copy drifts; what it would
// drift on is a guild's published retention policy and the moment a channel
// gets wiped. Compiling the real packages to wasm is the only way to put that
// logic in a browser and keep one source of truth.
//
// This package is deliberately free of syscall/js and of any build tag, so it
// compiles and is tested natively. cmd/lab is the thin binding on top, which
// keeps the untestable part down to argument marshalling.
package lab

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/plugins/rotation"
	"github.com/6586x57890143/merlin/internal/settings"
	"github.com/6586x57890143/merlin/internal/voice"
)

// Lab holds the one speaker every answer is rendered through.
type Lab struct {
	speaker voice.Source
	now     func() time.Time
}

// New loads the embedded voice catalog, failing exactly as the bot's startup
// does when a line is malformed. A lab that booted with a broken catalog
// would be previewing something the bot would refuse to run.
func New(log *slog.Logger) (*Lab, error) {
	speaker, err := voice.New(log)
	if err != nil {
		return nil, fmt.Errorf("lab: load voice catalog: %w", err)
	}
	return &Lab{speaker: speaker, now: func() time.Time { return time.Now().UTC() }}, nil
}

// RotationRequest is one hypothetical rotation slot, in the shape a form
// produces: strings and numbers, no domain types.
type RotationRequest struct {
	Interval string `json:"interval"`
	// LeadMinutes is the pre-rotation warning, 0 for off.
	LeadMinutes int    `json:"leadMinutes"`
	Disclosure  string `json:"disclosure"`
	// RetentionHours is nil for "keep forever", which is a real and
	// meaningfully different setting rather than a missing value.
	RetentionHours *int `json:"retentionHours"`
	// Fires is how many upcoming rotations to list.
	Fires int `json:"fires"`
}

// RotationResult is what the page renders.
type RotationResult struct {
	// Error is set when the configuration would be refused outright, in which
	// case nothing below it is filled in. It is the real validator's message,
	// so the page and the command refuse for the same reasons in the same
	// words.
	Error string `json:"error,omitempty"`
	// Cadence is the interval as the bot would print it, which is not always
	// how it was typed: "90m" comes back as the same thing "1h30m" does.
	Cadence string `json:"cadence,omitempty"`
	// Fires are the next rotation instants, in UTC.
	Fires []string `json:"fires,omitempty"`
	// HeadsUp is the warning posted LeadMinutes before a rotation, empty when
	// the lead is off.
	HeadsUp string `json:"headsUp,omitempty"`
	// Intro is the notice posted into the fresh channel afterwards. This is
	// the guild's published retention policy and the reason the preview
	// exists.
	Intro string `json:"intro,omitempty"`
	// Notes are things worth saying that are not refusals.
	Notes []string `json:"notes,omitempty"`
}

// maxFires bounds the schedule walk, since the count comes from a page.
const maxFires = 50

// Rotation answers one hypothetical slot.
func (l *Lab) Rotation(req RotationRequest) RotationResult {
	interval, err := core.ParseFlexibleDuration(req.Interval)
	if err != nil {
		return RotationResult{Error: err.Error()}
	}

	rc := settings.RotationChannel{
		GuildID: "lab", ChannelID: "lab-channel", ArchiveCategoryID: "lab-archive",
		ArchiveVisibility: "mod_only",
		IntervalMinutes:   int(interval / time.Minute),
		NoticeLeadMinutes: req.LeadMinutes,
		Disclosure:        settings.Disclosure(req.Disclosure),
		RetentionHours:    req.RetentionHours,
	}
	// The real validator, so the page refuses what the command refuses. A
	// separate list of rules here is the drift this whole package exists to
	// avoid.
	if err := rotation.ValidateChannel(rc); err != nil {
		return RotationResult{Error: err.Error()}
	}

	fires := req.Fires
	if fires < 1 {
		fires = 5
	}
	if fires > maxFires {
		fires = maxFires
	}

	res := RotationResult{Cadence: core.FormatDuration(interval)}

	// Walked through the real Schedule rather than by repeatedly adding the
	// interval, so a calendar-anchored schedule would land on the same
	// instants here as in the Scheduler.
	schedule := core.IntervalSchedule{Interval: interval}
	at := l.now()
	for i := 0; i < fires; i++ {
		at = schedule.Next(at)
		res.Fires = append(res.Fires, at.Format(time.RFC3339))
	}

	ctx := context.Background()
	if req.LeadMinutes > 0 {
		lead := time.Duration(req.LeadMinutes) * time.Minute
		res.HeadsUp = rotation.HeadsUpNotice(ctx, l.speaker, rc, lead)
	} else {
		res.Notes = append(res.Notes, "No heads-up: notice lead is off for this channel.")
	}
	res.Intro = rotation.RetentionNotice(ctx, l.speaker, rc)

	if rc.RetentionHours == nil {
		res.Notes = append(res.Notes, "Archives are kept forever: nothing sweeps them.")
	}
	switch settings.Disclosure(req.Disclosure).Resolve() {
	case settings.DisclosureGeneric:
		res.Notes = append(res.Notes,
			"Generic disclosure: neither the cadence nor the deletion window is stated, and the heads-up carries no countdown.")
	case settings.DisclosureCadence:
		res.Notes = append(res.Notes, "Cadence disclosure: the archival window is deliberately not stated.")
	case settings.DisclosureRetention:
		res.Notes = append(res.Notes, "Retention disclosure: the rotation cadence is deliberately not stated.")
	}
	return res
}

// KeyInfo describes one voice key to the page.
type KeyInfo struct {
	Key string `json:"key"`
	// Required are the placeholders every line for this key must contain.
	// The catalog refuses to boot on a line missing one, and refuses equally
	// on a line carrying one that is not listed here.
	Required []string `json:"required"`
}

// Keys lists the catalog's contract, which is what somebody writing a line
// needs before they write it.
func (l *Lab) Keys() []KeyInfo {
	keys := voice.Keys()
	out := make([]KeyInfo, 0, len(keys))
	for _, k := range keys {
		req := voice.RequiredVars(k)
		if req == nil {
			req = []string{}
		}
		out = append(out, KeyInfo{Key: string(k), Required: req})
	}
	return out
}

// Roll renders one key n times, which is how the variation is reviewed.
//
// Distinct guild IDs per roll, deliberately. Selection guarantees no
// immediate repeat per guild and key, so rolling against one guild would show
// the anti-repeat rule rather than the spread of the key, and somebody judging
// whether a key has enough range would be reading an artefact of the sampler.
func (l *Lab) Roll(key string, vars map[string]string, n int) []string {
	if n < 1 {
		n = 5
	}
	if n > maxFires {
		n = maxFires
	}
	ctx := context.Background()
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, l.speaker.Line(ctx, fmt.Sprintf("lab-%d", i), voice.Key(key), vars))
	}
	return out
}

// Lint reports what the catalog would say about a candidate line, which is the
// same answer the bot gives at startup.
//
// An empty result means the line would boot. This is the whole reason the page
// exists for whoever writes the lines: internal/voice is designed so lines are
// data that can be reviewed as writing, and until now reviewing them needed a
// Go toolchain.
func (l *Lab) Lint(key, line string) []string {
	problems := voice.Validate(voice.Key(key), line)
	if problems == nil {
		return []string{}
	}
	return problems
}

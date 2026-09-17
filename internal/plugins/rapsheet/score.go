package rapsheet

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/6586x57890143/merlin/internal/core"
)

// The score, and the ladder that reads it.
//
// Everything in this file is pure, for the same reason aimod's sanctionFor
// is: these functions decide how long a real person loses access to a
// community for, and it should be possible to read their whole behaviour off
// a table of inputs without a database, a Discord session or a clock.
//
// Bernard's score is additive over a fixed two-week window with thresholds
// that cannot be changed. Both halves are improved on here. Points decay
// continuously (a half-life rather than a window), so a warning from three
// weeks ago counts for something and a warning from three months ago counts
// for almost nothing, and there is no cliff a member can wait out to the
// day. And the bands are per guild, because a server that runs on dark
// humour and a server for teenagers should not be on the same ladder.

// Decayed is what points are worth at age, given halfLife. Exactly half at
// one half-life, a quarter at two, and never zero: an entry contributes
// until it is voided, it just stops mattering.
func Decayed(points int, age, halfLife time.Duration) float64 {
	if points <= 0 || halfLife <= 0 {
		return 0
	}
	if age < 0 {
		// A clock that says the entry is from the future is a clock, not a
		// reason to inflate somebody's score. Count it at full value.
		age = 0
	}
	return float64(points) * math.Exp2(-age.Hours()/halfLife.Hours())
}

// Score sums the decayed points of every non-voided entry.
func Score(entries []Entry, now time.Time, halfLife time.Duration) float64 {
	var total float64
	for _, e := range entries {
		if e.VoidedAt != nil {
			continue
		}
		total += Decayed(e.Points, now.Sub(e.CreatedAt), halfLife)
	}
	return total
}

// Action is what a band asks for.
type Action string

const (
	// ActionNone is band zero: nothing is owed.
	ActionNone Action = "none"
	// ActionNotice DMs the member that their record is building up. No
	// consequence beyond being told, which is itself the point: the first
	// rung of any ladder should be information.
	ActionNotice  Action = "notice"
	ActionJail    Action = "jail"
	ActionTimeout Action = "timeout"
	ActionBan     Action = "ban"
)

// Band is one rung: at or above Min points, Action is owed, for Duration
// where the action has one.
type Band struct {
	Min      float64
	Action   Action
	Duration time.Duration
}

// Recommendation is what the ladder says a record currently deserves. Band
// is the index into the guild's bands, which is what escalation idempotency
// is keyed on: the same band is never suggested twice inside a half-life.
type Recommendation struct {
	Band     int
	Action   Action
	Duration time.Duration
}

// defaultBands is the ladder a guild gets before it configures one.
//
// It never reaches a permanent ban. An unbounded ladder arrives at
// "effectively forever" in a few bad weeks, and a permanent ban dressed up
// as arithmetic is a decision a human should be making; the same reasoning
// aimod's maxSanction rests on. A guild that wants the ladder to go further
// can raise the top band, and ValidateBands still refuses a ban with no end.
var defaultBands = []Band{
	{Min: 0, Action: ActionNone},
	{Min: 25, Action: ActionNotice},
	{Min: 50, Action: ActionJail, Duration: 2 * time.Hour},
	{Min: 100, Action: ActionJail, Duration: 24 * time.Hour},
	{Min: 200, Action: ActionBan, Duration: 7 * 24 * time.Hour},
	{Min: 400, Action: ActionBan, Duration: 30 * 24 * time.Hour},
}

// maxDiscordTimeout is Discord's own ceiling on a timeout.
const maxDiscordTimeout = 28 * 24 * time.Hour

// Ladder returns the highest band whose Min the score reaches. bands must
// have passed ValidateBands; an empty slice means the defaults.
func Ladder(score float64, bands []Band) Recommendation {
	if len(bands) == 0 {
		bands = defaultBands
	}
	rec := Recommendation{Band: 0, Action: ActionNone}
	for i, b := range bands {
		if score >= b.Min {
			rec = Recommendation{Band: i, Action: b.Action, Duration: b.Duration}
		}
	}
	return rec
}

// ErrBadBands is wrapped by every ValidateBands failure.
var ErrBadBands = errors.New("rapsheet: invalid bands")

// ValidateBands is the whole contract a configured ladder has to meet, and
// the reason a permanent ban is unreachable by automation: a ban band with
// no duration is refused here, at the point of configuration, so nothing
// downstream has to check.
func ValidateBands(bands []Band) error {
	if len(bands) == 0 {
		return nil
	}
	if bands[0].Min != 0 || bands[0].Action != ActionNone {
		return fmt.Errorf("%w: the first band must be {0, none}", ErrBadBands)
	}
	if !sort.SliceIsSorted(bands, func(i, j int) bool { return bands[i].Min < bands[j].Min }) {
		return fmt.Errorf("%w: bands must be in ascending order of points", ErrBadBands)
	}
	for i := 1; i < len(bands); i++ {
		if bands[i].Min <= bands[i-1].Min {
			return fmt.Errorf("%w: two bands start at %v points", ErrBadBands, bands[i].Min)
		}
	}
	for _, b := range bands {
		switch b.Action {
		case ActionNone, ActionNotice:
			if b.Duration != 0 {
				return fmt.Errorf("%w: %s takes no duration", ErrBadBands, b.Action)
			}
		case ActionJail:
			if b.Duration <= 0 {
				return fmt.Errorf("%w: jail needs a duration", ErrBadBands)
			}
		case ActionTimeout:
			if b.Duration <= 0 || b.Duration > maxDiscordTimeout {
				return fmt.Errorf("%w: a timeout needs a duration of at most %s", ErrBadBands, core.FormatDuration(maxDiscordTimeout))
			}
		case ActionBan:
			if b.Duration <= 0 {
				return fmt.Errorf("%w: a ban band needs a duration; the ladder never bans permanently", ErrBadBands)
			}
		default:
			return fmt.Errorf("%w: unknown action %q", ErrBadBands, b.Action)
		}
	}
	return nil
}

// Strength orders actions so "is what is already standing at least this
// severe" is one comparison. A timeout and a jail are both restrictions and
// a jail is the stronger one: it strips roles as well as muting.
func (a Action) Strength() int {
	switch a {
	case ActionNotice:
		return 1
	case ActionTimeout:
		return 2
	case ActionJail:
		return 3
	case ActionBan:
		return 4
	}
	return 0
}

// String renders a band for /rapsheet list bands and the configure echo.
func (b Band) String() string {
	switch b.Action {
	case ActionNone:
		return fmt.Sprintf("%.0f+: nothing", b.Min)
	case ActionNotice:
		return fmt.Sprintf("%.0f+: notice", b.Min)
	}
	return fmt.Sprintf("%.0f+: %s %s", b.Min, b.Action, core.FormatDuration(b.Duration))
}

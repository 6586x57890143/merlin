package dragoman

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"golang.org/x/text/language"
	"golang.org/x/text/language/display"

	"github.com/6586x57890143/merlin/internal/core"
)

// Which English each language becomes: one row per language a server has
// picked a dialect for, and Standard American English for the rest.

// Dialect is one language's English and the guide it is written by.
type Dialect struct {
	GuildID, Language, Name string
	// Guide is nil until it has been written, and Previous nil until it has
	// been replaced.
	Guide, Previous *guide
	// Version counts the guides there have been. A write against any other
	// than the current one lands nowhere.
	Version int
	// Uses and Missed are how many messages the current guide has served and
	// how many of them needed a second answer or got none.
	Uses, Missed int
	// Losses is how many rounds in a row have changed nothing.
	Losses int
	// Notes is what admins have said about it, oldest first.
	Notes []string
	// Problem is why there is no guide yet.
	Problem string
}

// errStale is a dialect that was changed or removed meanwhile.
var errStale = errors.New("dragoman: the dialect was changed meanwhile")

// DialectStore keeps the dialects.
type DialectStore interface {
	Dialects(ctx context.Context, guildID string) ([]Dialect, error)
	Dialect(ctx context.Context, guildID, language string) (Dialect, error)
	// SetDialect starts a language on a dialect, from nothing.
	SetDialect(ctx context.Context, guildID, language, name string) error
	RemoveDialect(ctx context.Context, guildID, language string) error
	// SaveGuide puts g in d's place and starts its count, the next round
	// being due after reviewAfter uses.
	SaveGuide(ctx context.Context, d Dialect, g guide, reviewAfter int) (Dialect, error)
	// UndoGuide goes back to the guide before, and holds rounds off.
	UndoGuide(ctx context.Context, d Dialect, reviewAfter int) error
	// DeferReview is a round that changed nothing.
	DeferReview(ctx context.Context, d Dialect, reviewAfter int) error
	SetDialectProblem(ctx context.Context, d Dialect, why string) error
	SetDialectNotes(ctx context.Context, d Dialect, notes []string) error
	// CountDialect counts a message and reports whether a round is due.
	CountDialect(ctx context.Context, d Dialect, missed bool) (due bool, err error)
}

const dialectCols = `guild_id, language, name, guide, previous, version, uses, missed, losses, notes, problem`

func scanDialect(row pgx.Row) (Dialect, error) {
	var d Dialect
	var g, prev []byte
	if err := row.Scan(&d.GuildID, &d.Language, &d.Name, &g, &prev, &d.Version, &d.Uses, &d.Missed, &d.Losses, &d.Notes, &d.Problem); err != nil {
		return d, err
	}
	for _, c := range []struct {
		raw []byte
		to  **guide
	}{{g, &d.Guide}, {prev, &d.Previous}} {
		if c.raw == nil {
			continue
		}
		*c.to = new(guide)
		if err := json.Unmarshal(c.raw, *c.to); err != nil {
			return d, fmt.Errorf("guide: %w", err)
		}
	}
	return d, nil
}

func (s *pgStore) Dialects(ctx context.Context, guildID string) ([]Dialect, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+dialectCols+` FROM dragoman_dialects WHERE guild_id = $1 ORDER BY language`, guildID)
	if err != nil {
		return nil, fmt.Errorf("dragoman store: dialects: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Dialect, error) { return scanDialect(r) })
	if err != nil {
		return nil, fmt.Errorf("dragoman store: dialects: %w", err)
	}
	return out, nil
}

func (s *pgStore) Dialect(ctx context.Context, guildID, lang string) (Dialect, error) {
	d, err := scanDialect(s.pool.QueryRow(ctx, `SELECT `+dialectCols+` FROM dragoman_dialects WHERE guild_id = $1 AND language = $2`, guildID, lang))
	if errors.Is(err, pgx.ErrNoRows) {
		return Dialect{}, errStale
	}
	if err != nil {
		return Dialect{}, fmt.Errorf("dragoman store: dialect: %w", err)
	}
	return d, nil
}

func (s *pgStore) SetDialect(ctx context.Context, guildID, lang, name string) error {
	// The version goes on counting, so what was under way for the dialect
	// before this one lands nowhere.
	if _, err := s.pool.Exec(ctx, `INSERT INTO dragoman_dialects AS d (guild_id, language, name) VALUES ($1, $2, $3)
		ON CONFLICT (guild_id, language) DO UPDATE SET name = EXCLUDED.name, guide = NULL, previous = NULL, version = d.version + 1,
			uses = 0, missed = 0, losses = 0, next_review = 0, notes = '{}', problem = ''`, guildID, lang, name); err != nil {
		return fmt.Errorf("dragoman store: set dialect: %w", err)
	}
	return nil
}

func (s *pgStore) RemoveDialect(ctx context.Context, guildID, lang string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM dragoman_dialects WHERE guild_id = $1 AND language = $2`, guildID, lang); err != nil {
		return fmt.Errorf("dragoman store: remove dialect: %w", err)
	}
	return nil
}

// change is an update to exactly the dialect d was read as.
func (s *pgStore) change(ctx context.Context, d Dialect, set string, args ...any) error {
	tag, err := s.pool.Exec(ctx, `UPDATE dragoman_dialects SET `+set+` WHERE guild_id = $1 AND language = $2 AND version = $3 AND name = $4`,
		append([]any{d.GuildID, d.Language, d.Version, d.Name}, args...)...)
	if err != nil {
		return fmt.Errorf("dragoman store: change dialect: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errStale
	}
	return nil
}

func (s *pgStore) SaveGuide(ctx context.Context, d Dialect, g guide, reviewAfter int) (Dialect, error) {
	raw, err := json.Marshal(g)
	if err != nil {
		return d, fmt.Errorf("dragoman store: encode guide: %w", err)
	}
	if err := s.change(ctx, d, `previous = guide, guide = $5, version = version + 1, uses = 0, missed = 0, losses = 0, next_review = $6, problem = ''`,
		raw, reviewAfter); err != nil {
		return d, err
	}
	d.Previous, d.Guide, d.Version = d.Guide, &g, d.Version+1
	d.Uses, d.Missed, d.Losses, d.Problem = 0, 0, 0, ""
	return d, nil
}

func (s *pgStore) UndoGuide(ctx context.Context, d Dialect, reviewAfter int) error {
	return s.change(ctx, d, `guide = previous, previous = NULL, version = version + 1, uses = 0, missed = 0, losses = 0, next_review = $5`, reviewAfter)
}

func (s *pgStore) DeferReview(ctx context.Context, d Dialect, reviewAfter int) error {
	return s.change(ctx, d, `losses = losses + 1, next_review = uses + $5`, reviewAfter)
}

func (s *pgStore) SetDialectProblem(ctx context.Context, d Dialect, why string) error {
	return s.change(ctx, d, `problem = $5`, why)
}

func (s *pgStore) SetDialectNotes(ctx context.Context, d Dialect, notes []string) error {
	return s.change(ctx, d, `notes = $5, losses = 0`, notes)
}

func (s *pgStore) CountDialect(ctx context.Context, d Dialect, missed bool) (bool, error) {
	var due bool
	err := s.pool.QueryRow(ctx, `UPDATE dragoman_dialects SET uses = uses + 1, missed = missed + $5
		WHERE guild_id = $1 AND language = $2 AND version = $3 AND name = $4 RETURNING uses >= next_review`,
		d.GuildID, d.Language, d.Version, d.Name, map[bool]int{true: 1}[missed]).Scan(&due)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // replaced while the message was being written
	}
	if err != nil {
		return false, fmt.Errorf("dragoman store: count dialect: %w", err)
	}
	return due, nil
}

// dialects is a guild's dialects by language, read once as config is.
func (p *Plugin) dialects(ctx context.Context, guildID string) (map[string]Dialect, error) {
	id := core.ID(guildID)
	p.cfgMu.RLock()
	m, ok := p.spoken[id]
	p.cfgMu.RUnlock()
	if ok {
		return m, nil
	}
	p.cfgMu.Lock()
	defer p.cfgMu.Unlock()
	all, err := p.store.Dialects(ctx, guildID)
	if err != nil {
		return nil, err
	}
	m = make(map[string]Dialect, len(all))
	for _, d := range all {
		m[d.Language] = d
	}
	p.spoken[id] = m
	return m, nil
}

// dialectFor is the dialect tag becomes in this guild. Unreadable is none.
func (p *Plugin) dialectFor(ctx context.Context, guildID string, tag language.Tag) (Dialect, bool) {
	m, err := p.dialects(ctx, guildID)
	if err != nil {
		p.log.Error("dragoman: load dialects", "guild", guildID, "err", err)
		return Dialect{}, false
	}
	d, ok := m[languageKey(tag)]
	return d, ok
}

// speak puts t into the dialect its language has here, if any, and keeps the
// guide looked after. Any failure leaves t in Standard English.
func (p *Plugin) speak(ctx context.Context, j job, t translation, original string, parts []string) translation {
	d, ok := p.dialectFor(ctx, j.guildID, t.source)
	if !ok {
		return t
	}
	if d.Guide == nil {
		p.tend(j.guildID, d.Language, false)
		p.setTrouble(j.guildID, "dialect: the guide to "+d.Name+" is not written yet")
		return t
	}
	styled, missed, err := p.restyle(ctx, j, t, d, original, parts)
	var u interface{ Unavailable() bool }
	if err != nil {
		p.log.Warn("dragoman: posting without the dialect", "guild", j.guildID, "message", j.messageID, "err", err)
		p.setTrouble(j.guildID, "dialect: "+err.Error())
		// No money or no gateway says nothing about the guide.
		if errors.As(err, &u) || ctx.Err() != nil {
			return styled
		}
	}
	due, err := p.store.CountDialect(context.WithoutCancel(ctx), d, missed)
	if err != nil {
		p.log.Error("dragoman: count dialect", "guild", j.guildID, "err", err)
	}
	if due {
		p.tend(j.guildID, d.Language, false)
	}
	return styled
}

// languageKey is what a language is filed under. Models and admins name some
// of them more than one way.
func languageKey(tag language.Tag) string {
	switch b := baseOf(tag); b {
	case "nb", "nn":
		return "no"
	case "tl":
		return "fil"
	default:
		return b
	}
}

// languageNames is every language's code by its English name and its own,
// lowercase.
var languageNames = sync.OnceValue(func() map[string]string {
	out := map[string]string{"norwegian": "no", "frisian": "fy", "flemish": "nl", "tagalog": "fil"}
	english := display.English.Languages()
	add := func(code string) {
		tag, err := language.Parse(code)
		if err != nil || english.Name(tag) == "" {
			return
		}
		for _, n := range []string{english.Name(tag), display.Self.Name(tag)} {
			if n = strings.ToLower(n); n != "" && out[n] == "" {
				out[n] = languageKey(tag)
			}
		}
	}
	for a := 'a'; a <= 'z'; a++ {
		for b := 'a'; b <= 'z'; b++ {
			add(string([]rune{a, b}))
			for c := 'a'; c <= 'z'; c++ {
				add(string([]rune{a, b, c}))
			}
		}
	}
	return out
})

// parseLanguage is the language somebody typed: its name, in English or its
// own, or its code.
func parseLanguage(raw string) (language.Tag, bool) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if key, ok := languageNames()[raw]; ok {
		raw = key
	}
	tag, ok := parseSource(raw)
	if !ok {
		return tag, false
	}
	// Parse takes any well-formed code: one with no name is no language.
	return tag, display.English.Languages().Name(language.Make(baseOf(tag))) != ""
}

// keyName is a filed language's name and flag.
func keyName(key string) (name, emoji string) {
	tag := language.Make(key)
	if key == "no" {
		return "Norwegian", flag(tag)
	}
	return languageName(tag), flag(tag)
}

// maxDialects is how many a panel's picker can show.
const maxDialects = 25

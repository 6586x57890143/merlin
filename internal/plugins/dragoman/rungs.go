package dragoman

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/6586x57890143/merlin/internal/core"
)

// What a message costs to translate, cheapest first:
//
//   - nothing, when the gate (detect.go) calls it English, or the pre-filter
//     knows it for something the model has always left alone
//   - one call to the guild's model, told to think little
//   - a second call with all the thinking it wants and what was wrong with
//     the first, for an answer that fails a check (doubt)
//
// The first call is never the last word on a doubtful answer, so quality
// rests on the second, as it did when there was only that. Everything goes
// to endpoints that keep nothing, which is why OpenRouter's free models are
// no rung here: none that can answer does.
const (
	rungModel   = "model"
	rungCareful = "careful"
	// rungSkipped is not a rung: it counts what the pre-filter kept from one.
	rungSkipped = "skipped"

	// cheapConfidence is asked of the first call: models say 0.9 easily.
	cheapConfidence = 0.9
)

// climb asks the model, and once more if accept finds something wrong, and
// names the rung that answered last, also beside an error. accept is given
// the answer and whether it is the last word, and returns what is wrong with
// it, or "".
func (p *Plugin) climb(ctx context.Context, j job, q core.Prompt, accept func(out string, last bool) (doubt string)) (string, error) {
	cfg, err := p.config(ctx, core.ID(j.guildID))
	if err != nil {
		return "", err
	}
	if cfg.Model != "" && p.engine.HasOpenRouter(ctx, j.guildID) {
		q.Models = []string{cfg.Model}
	}
	q.Brief = true
	a, err := p.ask(ctx, j, rungModel, q)
	if err != nil {
		return "", err
	}
	why := accept(a, false)
	if why == "" {
		return rungModel, nil
	}
	p.log.Debug("dragoman: asking again", "guild", j.guildID, "message", j.messageID, "why", why)
	q.Brief = false
	q.User += fmt.Sprintf("\n\nYou have answered this once already:\n%s\nThat was held back, because %s. "+
		"Work it out again with care. Keep the same answer if it is right.", a, why)
	if a, err = p.ask(ctx, j, rungCareful, q); err != nil {
		return rungModel, err
	}
	accept(a, true)
	return rungCareful, nil
}

func (p *Plugin) ask(ctx context.Context, j job, name string, q core.Prompt) (string, error) {
	a, err := p.engine.Ask(ctx, j.guildID, q)
	p.use(ctx, j.guildID, name, a.Cost, err)
	return a.Text, err
}

// use counts one call. One that was refused before it was made isn't one.
func (p *Plugin) use(ctx context.Context, guildID, name string, cost float64, err error) {
	var u interface{ Unavailable() bool }
	if errors.As(err, &u) && u.Unavailable() && cost == 0 {
		return
	}
	if err := p.store.AddUsage(context.WithoutCancel(ctx), guildID, p.now().Truncate(24*time.Hour), name, cost); err != nil {
		p.log.Error("dragoman: record usage", "guild", guildID, "err", err)
	}
}

var digits = regexp.MustCompile(`\d+`)

// doubt is what looks wrong with a translation that is publishable, or "".
// Every check is one a right answer can fail (14 uur is 2 pm): failing one
// costs a second call, never the translation.
func doubt(t translation, masked string) string {
	plain := strings.TrimSpace(token.ReplaceAllString(masked, " "))
	got := strings.TrimSpace(token.ReplaceAllString(t.masked, " "))
	switch {
	case t.policy != "":
		return "you called it harmful, which has to be certain"
	case t.confidence < cheapConfidence:
		return "you were not sure of it"
	case gauge(got, "") == strong:
		return "what you wrote is not in English"
	case strings.Count(plain, "\n") != strings.Count(got, "\n"):
		return "the lines do not match"
	case utf8.RuneCountInString(plain) > 60 && utf8.RuneCountInString(got)*3 < utf8.RuneCountInString(plain):
		return "it is much shorter than the message"
	}
	for _, n := range digits.FindAllString(plain, -1) {
		if !strings.Contains(got, n) {
			return "the number " + n + " is missing"
		}
	}
	return ""
}

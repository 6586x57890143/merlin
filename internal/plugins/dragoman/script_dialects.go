package dragoman

import "context"

// The dialects script: which English a language is translated into. Off, or
// for a language not in the table, it is Standard American English. The
// table is code, so a server decides only whether it applies.
const scriptDialects = "dialects"

// variety is one kind of English: its name and what a model is told about
// writing it.
type variety struct{ name, how string }

var standard = variety{
	name: "Standard American English",
	how:  "Write natural, idiomatic Standard American English, in the casual register people type in chat.",
}

var dialects = map[string]variety{
	"nl": {
		name: "African American Vernacular English",
		how: "Write fluent, contemporary African American Vernacular English, as a native speaker would type it in chat. " +
			"Use its real grammar where it fits (habitual be, zero copula, completive done, negative concord, ain't, finna) and its ordinary vocabulary. " +
			"Keep what the author said and how they meant it: the same claims, the same mood, no added jokes, boasts, swearing or slang for its own sake. " +
			"It is a dialect, never a caricature: no phonetic mockery, no stereotypes, and never any form of the n-word.",
	},
}

// varietiesOn are the languages that don't become Standard American English
// in this guild. The model names the language and writes the English in one
// call, so it is told all of them. An unreadable switch is off.
func (p *Plugin) varietiesOn(ctx context.Context, guildID string) map[string]variety {
	if on, err := p.scripts.Enabled(ctx, guildID, scriptDialects); err != nil || !on {
		return nil
	}
	return dialects
}

// englishFor is the variety base becomes, given what is on.
func englishFor(varieties map[string]variety, base string) variety {
	if v, ok := varieties[base]; ok {
		return v
	}
	return standard
}

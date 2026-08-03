package voice

import (
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// The lines themselves are data, not code, so they can be edited and
// reviewed as writing. Embedded rather than read from disk at runtime for
// the same reason the brand images are (see core/embeds.go): the running
// binary should not be able to lose track of what it is allowed to say.
//
//go:embed lines/*.yaml
var lineFS embed.FS

// placeholderRe matches the {name} substitution form. Deliberately not
// text/template: a template parse error is a runtime failure mode, and the
// entire point of this package is that a broken line cannot reach a
// channel. A regex over a fixed, tiny grammar has no failure mode to have.
var placeholderRe = regexp.MustCompile(`\{([a-z][a-z0-9_]*)\}`)

// catalog is the validated set of lines, keyed exactly like specs.
type catalog map[Key][]string

// loadCatalog reads and validates the embedded lines. Every problem found
// is reported at once rather than one per run: whoever is fixing a catalog
// is usually fixing several lines, and a validator that surfaces one error
// per build turns that into an afternoon.
func loadCatalog() (catalog, error) {
	raw := map[string][]string{}

	err := fs.WalkDir(lineFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, readErr := lineFS.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("read %s: %w", path, readErr)
		}
		var part map[string][]string
		if unmarshalErr := yaml.Unmarshal(b, &part); unmarshalErr != nil {
			return fmt.Errorf("parse %s: %w", path, unmarshalErr)
		}
		for k, lines := range part {
			if _, dup := raw[k]; dup {
				return fmt.Errorf("key %q is defined in more than one file", k)
			}
			raw[k] = lines
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	cat := make(catalog, len(raw))
	var problems []string

	for key := range specs {
		lines, ok := raw[string(key)]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: declared in specs but has no lines", key))
			continue
		}
		if len(lines) < minLinesPerKey {
			problems = append(problems, fmt.Sprintf("%s: %d lines, want at least %d", key, len(lines), minLinesPerKey))
		}
		// A repeated line is the one authoring mistake that costs variety
		// without looking like anything: the key still passes its line count,
		// the duplicate just gets picked twice as often as everything else,
		// and nobody notices until the same sentence turns up twice in an
		// afternoon in a channel people are watching.
		seen := map[string]int{}
		for i, line := range lines {
			for _, p := range Validate(key, line) {
				problems = append(problems, fmt.Sprintf("%s[%d]: %s", key, i, p))
			}
			if j, dup := seen[line]; dup {
				problems = append(problems, fmt.Sprintf("%s[%d]: identical to line %d, so this key has one permutation fewer than it looks like", key, i, j))
				continue
			}
			seen[line] = i
		}
		cat[key] = lines
	}

	// A key in the YAML with no spec is almost always a typo in the key
	// name, which would otherwise sit there silently as lines that can
	// never be selected.
	for k := range raw {
		if _, ok := specs[Key(k)]; !ok {
			problems = append(problems, fmt.Sprintf("%s: has lines but no spec, so nothing can ever say it", k))
		}
	}

	// The fallbacks are compiled in and are the last line of defence, so
	// they are held to the same contract as the catalog.
	for key, sp := range specs {
		for _, p := range Validate(key, sp.fallback) {
			problems = append(problems, fmt.Sprintf("%s fallback: %s", key, p))
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("voice: catalog is invalid:\n  %s", strings.Join(problems, "\n  "))
	}
	return cat, nil
}

// Validate reports everything wrong with one line for one key, or nil if it
// is fine.
//
// Exported because it is the contract, not an implementation detail. Any
// future source of lines, a generator or a live model, has to pass its
// output through here before the result reaches a channel. Without that,
// "we can add an LLM later" means adding a second, unchecked path into a
// public channel, and the retention disclosure stops being guaranteed by
// anything.
func Validate(key Key, line string) []string {
	sp, ok := specs[key]
	if !ok {
		return []string{fmt.Sprintf("unknown key %q", key)}
	}

	var problems []string
	if strings.TrimSpace(line) == "" {
		return []string{"line is empty"}
	}
	if len(line) > sp.maxLen {
		problems = append(problems, fmt.Sprintf("line is %d bytes, over the %d limit for this surface", len(line), sp.maxLen))
	}

	// Discord renders a line as typed. A leading or trailing space is
	// invisible in a YAML diff and visible in the channel.
	if line != strings.TrimSpace(line) {
		problems = append(problems, "line has leading or trailing whitespace")
	}

	found := map[string]bool{}
	for _, m := range placeholderRe.FindAllStringSubmatch(line, -1) {
		found[m[1]] = true
	}
	for _, want := range sp.required {
		if !found[want] {
			problems = append(problems, fmt.Sprintf("missing required placeholder {%s}", want))
		}
	}
	for name := range found {
		if !slices.Contains(sp.required, name) && !slices.Contains(sp.optional, name) {
			problems = append(problems, fmt.Sprintf("unknown placeholder {%s}", name))
		}
	}

	// A stray brace is how a placeholder typo escapes the check above: a
	// line reading "every {cadence" has no placeholders at all as far as
	// the regex is concerned, so it would pass the required check by
	// looking like it has none, and then post a literal brace.
	if strings.Count(line, "{") != strings.Count(line, "}") {
		problems = append(problems, "unbalanced braces, so a placeholder is malformed")
	}
	if strings.Count(line, "{") != len(found) {
		problems = append(problems, "a brace does not belong to a well-formed {placeholder}")
	}

	// The same characters CI rejects across the repository. Enforced here
	// as well because this is the one place text could arrive from outside
	// the repository entirely, once a generator exists, and a generator is
	// exactly what produces them.
	//
	// Written as code points rather than literals so this file does not
	// contain the characters it rejects, which would otherwise make the
	// repository-wide CI check fail on the very code implementing it.
	for _, bad := range []struct {
		r    rune
		name string
	}{
		{0x2014, "em dash"},
		{0x2026, "ellipsis character"},
		{0x201C, "curly quote"},
		{0x201D, "curly quote"},
	} {
		if strings.ContainsRune(line, bad.r) {
			problems = append(problems, "contains a "+bad.name+", which reads as machine written")
		}
	}
	return problems
}

// render substitutes {name} placeholders and reports whether every one of
// them was filled. An empty value counts as unfilled: "resets every " is a
// broken sentence, not a shorter one.
func render(line string, vars map[string]string) (string, bool) {
	complete := true
	out := placeholderRe.ReplaceAllStringFunc(line, func(m string) string {
		name := m[1 : len(m)-1]
		v, ok := vars[name]
		if !ok || v == "" {
			complete = false
			return m
		}
		return v
	})
	return out, complete
}

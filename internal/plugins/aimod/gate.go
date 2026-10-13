package aimod

import (
	"math/bits"
	"regexp"
	"regexp/syntax"
	"unicode"
	"unicode/utf8"
)

// Rung 1 runs on every message on every server, and nearly every message is
// clean: on 2026-10-13 hardHit took 15us and 62 allocations to say so, almost
// all of it the slur table backtracking over text that holds none of its
// letters. The gate skips a pattern when the text provably cannot match it,
// before the regexp runs, so a clean message pays one pass over its bytes.
//
// "Provably" is the whole design. What a pattern needs is read off its own
// parsed syntax, never written by hand, so a new table entry is gated
// correctly without anybody thinking about it, and the reading only ever
// errs towards running the pattern: anything it cannot be sure of (an
// optional part, a class reaching outside ASCII, a construct it does not
// know) asks for nothing. gate_test.go holds the gated and ungated answers
// equal over every test string in this package and under fuzzing.

// profile is which ASCII bytes a text contains, and whether it contains
// anything else. Built in one pass with no allocation.
type profile struct {
	ascii    [2]uint64
	nonASCII bool
}

func profileOf(s string) profile {
	var p profile
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x80 {
			p.ascii[c>>6] |= 1 << (c & 63)
		} else {
			p.nonASCII = true
		}
	}
	return p
}

// charset is a set of characters a match must contain one of. Non-ASCII
// members are not listed one by one: any non-ASCII byte in the text counts
// as a possible member, which is the sound direction.
type charset struct {
	ascii    [2]uint64
	nonASCII bool
}

func (c *charset) add(r rune) {
	if r < 0x80 {
		c.ascii[r>>6] |= 1 << (r & 63)
	} else {
		c.nonASCII = true
	}
}

func (c *charset) union(o charset) {
	c.ascii[0] |= o.ascii[0]
	c.ascii[1] |= o.ascii[1]
	c.nonASCII = c.nonASCII || o.nonASCII
}

func (c charset) in(p profile) bool {
	return c.ascii[0]&p.ascii[0] != 0 || c.ascii[1]&p.ascii[1] != 0 || (c.nonASCII && p.nonASCII)
}

// size is how selective the set is, for picking the best one to stand for a
// branch of an alternation. A set with non-ASCII members is the least
// selective, since any non-ASCII byte satisfies it.
func (c charset) size() int {
	n := bits.OnesCount64(c.ascii[0]) + bits.OnesCount64(c.ascii[1])
	if c.nonASCII {
		n += 1 << 16
	}
	return n
}

// needs is every charset a match must hit. An empty needs never skips.
type needs []charset

// admits reports whether p could contain a match. Gating off answers yes.
func (n needs) admits(p profile) bool {
	if !rung1Gate {
		return true
	}
	for _, c := range n {
		if !c.in(p) {
			return false
		}
	}
	return true
}

// rung1Gate switches the gate off, for the tests that hold the gated and
// ungated answers equal.
var rung1Gate = true

// needsOf reads what every match of re must contain.
func needsOf(re *regexp.Regexp) needs {
	parsed, err := syntax.Parse(re.String(), syntax.Perl)
	if err != nil {
		// The regexp compiled, so its own syntax parses; this is unreachable,
		// and running the pattern is the safe answer if it ever is not.
		return nil
	}
	return required(parsed)
}

func required(re *syntax.Regexp) needs {
	switch re.Op {
	case syntax.OpLiteral:
		var out needs
		for _, r := range re.Rune {
			var c charset
			c.add(r)
			if re.Flags&syntax.FoldCase != 0 {
				for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
					c.add(f)
				}
			}
			out = append(out, c)
		}
		return out
	case syntax.OpCharClass:
		// The parser has already folded case into the ranges.
		var c charset
		for i := 0; i+1 < len(re.Rune); i += 2 {
			lo, hi := re.Rune[i], re.Rune[i+1]
			for r := lo; r <= hi && r < 0x80; r++ {
				c.add(r)
			}
			if hi >= 0x80 {
				c.nonASCII = true
			}
		}
		return needs{c}
	case syntax.OpCapture:
		return required(re.Sub[0])
	case syntax.OpPlus:
		return required(re.Sub[0])
	case syntax.OpRepeat:
		if re.Min >= 1 {
			return required(re.Sub[0])
		}
		return nil
	case syntax.OpConcat:
		var out needs
		for _, sub := range re.Sub {
			out = append(out, required(sub)...)
		}
		return out
	case syntax.OpAlternate:
		// Every match matches some branch, so it holds a member of that
		// branch's most selective set; the union of those is needed. A
		// branch that needs nothing makes the whole alternation need nothing.
		var c charset
		for _, sub := range re.Sub {
			n := required(sub)
			if len(n) == 0 {
				return nil
			}
			best := n[0]
			for _, s := range n[1:] {
				if s.size() < best.size() {
					best = s
				}
			}
			c.union(best)
		}
		return needs{c}
	}
	// Star, quest, empty, anchors, word boundaries, any-char: a match can
	// hold nothing particular here.
	return nil
}

// shapeNeeds is what nWordShape needs a token to contain, read the other
// way: the characters that squash and nWordNormal turn into the n, the g and
// the r its pattern requires. gate_test.go walks every rune through squash
// and nWordNormal to hold this table to them.
var shapeNeeds = func() needs {
	set := func(s string) charset {
		var c charset
		for _, r := range s {
			c.add(r)
		}
		return c
	}
	return needs{set("nN"), set("gGbBqQxX69"), set("rR")}
}()

// starter finds a pattern's leftmost match by trying it only where a match
// can start. Every slur pattern begins with one of a few letters, so on a
// clean message the anchored copy is tried at a handful of offsets and fails
// within a character or two of each, where the plain search walks every
// byte through the whole automaton.
//
// The answer is FindStringIndex's: the leftmost offset at which the pattern
// matches, and at that offset the match the leftmost-first rules pick, which
// is what the anchored copy returns there. That only holds for a pattern
// whose matches do not depend on what precedes them, so one with any
// empty-width assertion (\b, ^, $) gets no starter and searches as before.
type starter struct {
	plain    *regexp.Regexp
	anchored *regexp.Regexp // nil: search with plain
	first    charset
}

func starterOf(re *regexp.Regexp) starter {
	s := starter{plain: re}
	parsed, err := syntax.Parse(re.String(), syntax.Perl)
	if err != nil || !readable(parsed) {
		return s
	}
	first, empty, ok := firsts(parsed)
	if !ok || empty {
		return s
	}
	s.anchored, s.first = regexp.MustCompile(`\A(?:`+re.String()+`)`), first
	return s
}

func (s starter) find(text string) []int {
	if s.anchored == nil || !rung1Gate {
		return s.plain.FindStringIndex(text)
	}
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c < 0x80 {
			if s.first.ascii[c>>6]&(1<<(c&63)) == 0 {
				continue
			}
		} else if !s.first.nonASCII || !utf8.RuneStart(c) {
			continue
		}
		if loc := s.anchored.FindStringIndex(text[i:]); loc != nil {
			return []int{i + loc[0], i + loc[1]}
		}
	}
	return nil
}

// firsts is the set a match's first character comes from, whether a match
// can be empty, and ok false for a construct it does not read, in which case
// the pattern keeps its plain search. starterOf has already refused any
// pattern that is not readable.
func firsts(re *syntax.Regexp) (first charset, empty, ok bool) {
	switch re.Op {
	case syntax.OpLiteral:
		n := required(&syntax.Regexp{Op: syntax.OpLiteral, Flags: re.Flags, Rune: re.Rune[:1]})
		return n[0], false, true
	case syntax.OpCharClass:
		return required(re)[0], false, true
	case syntax.OpCapture, syntax.OpPlus:
		return firsts(re.Sub[0])
	case syntax.OpStar, syntax.OpQuest:
		f, _, ok := firsts(re.Sub[0])
		return f, true, ok
	case syntax.OpRepeat:
		f, e, ok := firsts(re.Sub[0])
		return f, e || re.Min == 0, ok
	case syntax.OpEmptyMatch:
		return charset{}, true, true
	case syntax.OpConcat:
		empty = true
		for _, sub := range re.Sub {
			f, e, ok := firsts(sub)
			if !ok {
				return charset{}, false, false
			}
			first.union(f)
			if !e {
				empty = false
				break
			}
		}
		return first, empty, true
	case syntax.OpAlternate:
		for _, sub := range re.Sub {
			f, e, ok := firsts(sub)
			if !ok {
				return charset{}, false, false
			}
			first.union(f)
			empty = empty || e
		}
		return first, empty, true
	}
	return charset{}, false, false
}

// readable is false if re holds anything whose match depends on context:
// assertions, and any-char, which firsts does not read.
func readable(re *syntax.Regexp) bool {
	switch re.Op {
	case syntax.OpLiteral, syntax.OpCharClass, syntax.OpEmptyMatch:
		return true
	case syntax.OpCapture, syntax.OpPlus, syntax.OpStar, syntax.OpQuest, syntax.OpRepeat, syntax.OpConcat, syntax.OpAlternate:
		for _, sub := range re.Sub {
			if !readable(sub) {
				return false
			}
		}
		return true
	}
	return false
}

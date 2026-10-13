package aimod

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// corpus is every string literal in this package's tests, which is every
// slur spelling, evasion and innocent near miss anybody has written down,
// plus each one upper-cased and glued into a sentence.
func corpus(t testing.TB) []string {
	t.Helper()
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	fset := token.NewFileSet()
	for _, f := range files {
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if s, err := strconv.Unquote(lit.Value); err == nil {
					add(s)
					add(strings.ToUpper(s))
					add("well " + s + " then")
				}
			}
			return true
		})
	}
	if len(out) < 1000 {
		t.Fatalf("only %d strings: the corpus is not reading the tests", len(out))
	}
	return out
}

// ungated runs f with the gate off and a fixed replacement pick, so its
// answer can be compared with the gated one.
func both(f func()) {
	pickIndex = func(int) int { return 0 }
	defer func() { pickIndex, rung1Gate = defaultPick, true }()
	rung1Gate = false
	f()
	rung1Gate = true
}

var defaultPick = pickIndex

type rung1Answer struct {
	bucket          Bucket
	reason, rewrite string
	hit             bool
	redacted        string
	redactHit       bool
}

func answer(s string) rung1Answer {
	var a rung1Answer
	a.bucket, a.reason, a.rewrite, a.hit = hardHit(s)
	a.redacted, a.redactHit = redactSlurs(s)
	return a
}

// agree reports whether the gate changes rung 1's answer for s.
func agree(s string) (gated, ungated rung1Answer, ok bool) {
	pickIndex = func(int) int { return 0 }
	defer func() { pickIndex = defaultPick }()
	gated = answer(s)
	both(func() { ungated = answer(s) })
	return gated, ungated, gated == ungated
}

func TestGateNeverChangesAnAnswer(t *testing.T) {
	hits := 0
	for _, s := range corpus(t) {
		g, u, ok := agree(s)
		if !ok {
			t.Errorf("gate changed rung 1 on %q:\n gated   %+v\n ungated %+v", s, g, u)
		}
		if u.hit {
			hits++
		}
	}
	// The corpus has to exercise the matching paths, not just the skips.
	if hits < 100 {
		t.Fatalf("only %d hits in the corpus", hits)
	}
}

func FuzzGateNeverChangesAnAnswer(f *testing.F) {
	for _, s := range corpus(f) {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if g, u, ok := agree(s); !ok {
			t.Errorf("gate changed rung 1 on %q:\n gated   %+v\n ungated %+v", s, g, u)
		}
	})
}

// The token loop in redactSlurs stands where shapeToken.ReplaceAllStringFunc
// was, so it must cut exactly the tokens `\S+` does.
var shapeTokenRe = regexp.MustCompile(`\S+`)

func tokensAgree(s string) bool {
	want := shapeTokenRe.FindAllStringIndex(s, -1)
	var got [][]int
	for at, end := nextToken(s, 0); at < len(s); at, end = nextToken(s, end) {
		got = append(got, []int{at, end})
	}
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i][0] != want[i][0] || got[i][1] != want[i][1] {
			return false
		}
	}
	return true
}

func TestTokensMatchTheRegexp(t *testing.T) {
	for _, s := range append(corpus(t), "", " ", "a\vb", "\xff\xfe x", "  lead and trail  ", "tab\there\r\nnl\fff") {
		if !tokensAgree(s) {
			t.Errorf("tokens differ from the regexp on %q", s)
		}
	}
}

func FuzzTokensMatchTheRegexp(f *testing.F) {
	f.Add("a b\tc")
	f.Fuzz(func(t *testing.T, s string) {
		if !tokensAgree(s) {
			t.Errorf("tokens differ from the regexp on %q", s)
		}
	})
}

// Every rune, as fold leaves it in the text and as squash and nWordNormal
// then read it inside a token: whatever turns into an n, a g or an r must be
// in shapeNeeds, or the gate would skip a word nWordShape matches.
func TestShapeNeedsCoverEveryRune(t *testing.T) {
	sets := map[byte]charset{'n': shapeNeeds[0], 'g': shapeNeeds[1], 'r': shapeNeeds[2]}
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !utf8.ValidRune(r) {
			continue
		}
		folded := fold(string(r))
		p := profileOf(folded)
		for _, ch := range []byte(nWordNormal(folded)) {
			if c, need := sets[ch]; need && !c.in(p) {
				t.Errorf("U+%04X reads as %q in a token but the gate does not admit it", r, ch)
			}
		}
	}
}

func TestNeedsOf(t *testing.T) {
	cases := []struct {
		re     string
		admits []string
		skips  []string
	}{
		{`abc`, []string{"xabcx"}, []string{"ab", "ABC"}},
		{`(?i)k`, []string{"K", "k", "\u212a"}, []string{"x"}},
		{`(?i)s`, []string{"\u017f"}, []string{"t"}},
		{`a?b`, []string{"b"}, []string{"a"}},
		{`x*`, []string{""}, nil},
		{`(cat|dog)`, []string{"c", "d"}, []string{"x"}},
		{`(cat|)z`, []string{"z"}, []string{"cat"}},
		{`[^\p{L}]q`, []string{" q"}, []string{"x", "q"}},
		{`n.{2,}r`, []string{"nr"}, []string{"n", "r"}},
		{`é`, []string{"\u00e9", "\u00ff"}, []string{"e"}},
	}
	for _, c := range cases {
		n := needsOf(regexp.MustCompile(c.re))
		for _, s := range c.admits {
			if !n.admits(profileOf(s)) {
				t.Errorf("%s: skipped %q", c.re, s)
			}
		}
		for _, s := range c.skips {
			if n.admits(profileOf(s)) {
				t.Errorf("%s: admitted %q", c.re, s)
			}
		}
	}
}

var cleanMessages = []string{
	"lol yeah i saw that, gonna hop on in like 10 min",
	"did anyone finish the raid last night? we wiped on the second boss like four times",
	"ok",
	"https://www.youtube.com/watch?v=dQw4w9WgXcQ check this out it's actually hilarious",
	"no way 😂😂 that's insane",
}

func BenchmarkHardHitClean(b *testing.B) {
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		hardHit(cleanMessages[i%len(cleanMessages)])
	}
}

func BenchmarkHardHitSlur(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		hardHit("shut up you n1gg3r")
	}
}

package audit

import "testing"

// The entries in the audit channel that prompted this: every detail was one
// run-on line with its quoting and brackets left in, and role lists were raw
// snowflakes. What is asserted is the layout a moderator reads.
func TestReadableLaysPairsOutAsLabelledLines(t *testing.T) {
	cases := map[string]string{
		`user=<@1> duration=10m reason="Slur skirting"`: "**Member** <@1>\n**Duration** 10m\n**Reason** Slur skirting",
		`user=<@1> from=jail restored=[<@&2> <@&3>]`:    "**Member** <@1>\n**From** jail\n**Roles restored** <@&2> <@&3>",
		`user=<@1> from=jail restored=[]`:               "**Member** <@1>\n**From** jail\n**Roles restored** none",
		// Prose stays prose, in order, around the pairs.
		`case #12 user=<@1> ban served`: "case #12\n**Member** <@1>\nban served",
		// An either/or pair with one half empty drops the empty half.
		`role= user=<@1>`: "**Member** <@1>",
		// A quoted value keeps its spaces, escapes and markdown.
		`message="[Jump](https://x/1/2/3)" reason="said \"hi\""`: "**Message** [Jump](https://x/1/2/3)\n**Reason** said \"hi\"",
	}
	for in, want := range cases {
		if got := readable(in); got != want {
			t.Errorf("readable(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

// Anything that is not the pair format must come through untouched, or the
// renderer starts rewriting entries it does not understand.
func TestReadableLeavesOtherValuesAlone(t *testing.T) {
	for _, in := range []string{
		"", "<@99>", "plain words only", "a=b\nalready laid out",
		`reason="unterminated`, "1 + 1 = 2",
	} {
		if got := readable(in); got != in {
			t.Errorf("readable(%q) = %q, want it unchanged", in, got)
		}
	}
}

// The layout the screenshots asked for: the reason leads as a quote beside
// the mood icon, and everything else is a column in a grid after the actor.
func TestPairDetailsRenderAsAGrid(t *testing.T) {
	e := buildEmbed("system", "aimod.rewrite", "",
		`user=<@1> channel=<#2> policy="Hate speech" confidence="x 95%" message="[Deleted](u)" reason="a slur"`)
	if e.Description != "> a slur" {
		t.Errorf("description = %q, want the reason as a quote", e.Description)
	}
	want := []string{"Actor", "Member", "Channel", "Policy", "Confidence", "Message"}
	if len(e.Fields) != len(want) {
		t.Fatalf("fields = %+v", e.Fields)
	}
	for i, f := range e.Fields {
		if f.Name != want[i] || !f.Inline {
			t.Errorf("field %d = %q inline=%v, want inline %q", i, f.Name, f.Inline, want[i])
		}
	}
}

package dragoman

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestMaskTakesOutWhatIsNotLanguage(t *testing.T) {
	const text = "kijk <@123456789012345678> <:lol:123456789012345678> https://x.com/a?b=c en example.nl `code` @everyone <#1> <t:1:R>"
	masked, parts, ok := mask(text)
	if !ok || len(parts) != 8 {
		t.Fatalf("masked %q parts %q", masked, parts)
	}
	if strings.ContainsAny(masked, "<@`") || strings.Contains(masked, "x.com") {
		t.Errorf("masked %q still holds something", masked)
	}
	if out, ok := unmask(masked, parts); !ok || out != text {
		t.Errorf("unmask = %q, %v", out, ok)
	}
	if _, _, ok := mask("a " + tokenOpen + "1" + tokenClose); ok {
		t.Error("a message that already looks masked was masked")
	}
}

func tok(n string) string { return tokenOpen + n + tokenClose }

func TestUnmaskRefusesAnyOtherSetOfTokens(t *testing.T) {
	parts := []string{"https://a.com", "<@1>"}
	for _, s := range []string{
		tok("1"), tok("1") + tok("1") + tok("2"), tok("1") + tok("2") + tok("3"), tok("0") + tok("1") + tok("2"),
		tok("1") + tok("2") + " https://evil.com", tok("1") + tok("2") + " evil.com", tok("1") + tok("2") + " @everyone",
		tok("1") + tok("2") + " discord.gg/evil", tok("1") + tok("2") + " <@99>", tok("1") + tok("2") + tokenOpen,
		tok("1") + tok("2") + " [x](javascript://y)",
	} {
		if out, ok := unmask(s, parts); ok {
			t.Errorf("%q came out as %q", s, out)
		}
	}
	if out, ok := unmask(tok("2")+" see "+tok("1"), parts); !ok || out != "<@1> see https://a.com" {
		t.Errorf("moved tokens = %q, %v", out, ok)
	}
}

const good = `{"translate":true,"source":"nl","confidence":0.95,"harmful":"","text":"the train's always late"}`

func TestJudgePublishesOnlyWhatItCanStandBehind(t *testing.T) {
	const masked = "de trein is altijd te laat"
	got, err := judge(good, masked, nil)
	if err != nil || got.text != "the train's always late" || baseOf(got.source) != "nl" || got.policy != "" {
		t.Fatalf("judge = %+v, %v", got, err)
	}
	for name, out := range map[string]string{
		"not for translating": strings.Replace(good, "true", "false", 1),
		"english":             strings.Replace(good, `"nl"`, `"en-GB"`, 1),
		"no language":         strings.Replace(good, `"nl"`, `"und"`, 1),
		"not a language":      strings.Replace(good, `"nl"`, `"Dutch!!"`, 1),
		"unknown language":    strings.Replace(good, `"nl"`, `"xx"`, 1),
		"unsure":              strings.Replace(good, "0.95", "0.6", 1),
		"empty":               strings.Replace(good, "the train's always late", " ", 1),
		"unchanged":           strings.Replace(good, "the train's always late", "De trein is altijd te laat", 1),
		"a speech":            strings.Replace(good, "the train's always late", strings.Repeat("late ", 100), 1),
		"a link":              strings.Replace(good, "the train's", "see https://evil.com", 1),
	} {
		if got, err := judge(out, masked, nil); !errors.Is(err, errLeave) || got.text != "" || got.policy != "" {
			t.Errorf("%s: %+v, %v", name, got, err)
		}
	}
	if _, err := judge("sorry, I can't", masked, nil); err == nil || errors.Is(err, errLeave) {
		t.Errorf("prose: %v, want a failure rather than a decision", err)
	}
}

func TestJudgeKeepsTheHarm(t *testing.T) {
	soft := strings.Replace(good, `"harmful":""`, `"harmful":"threats"`, 1)
	if got, err := judge(soft, "x y z", nil); err != nil || got.policy != "threats" || got.text == "" {
		t.Errorf("softened = %+v, %v", got, err)
	}
	// Nothing harmless left: no text, but moderation still hears which.
	none := strings.Replace(soft, "the train's always late", "", 1)
	if got, err := judge(none, "x y z", nil); !errors.Is(err, errLeave) || got.policy != "threats" || got.text != "" {
		t.Errorf("nothing left = %+v, %v", got, err)
	}
}

func TestFlags(t *testing.T) {
	for code, want := range map[string]string{
		"nl": "\U0001F1F3\U0001F1F1", "nl-BE": "\U0001F1E7\U0001F1EA", "pt-PT": "\U0001F1F5\U0001F1F9", "es-419": "\U0001F1EA\U0001F1F8",
		"ja": "\U0001F1EF\U0001F1F5", "uk": "\U0001F1FA\U0001F1E6", "zh-Hant": "\U0001F1F9\U0001F1FC",
		"eo": "\U0001F310 Esperanto", "tlh": "\U0001F310 Klingon",
	} {
		tag, ok := parseSource(code)
		if !ok {
			t.Errorf("%s is not a language", code)
			continue
		}
		if got := flag(tag); got != want {
			t.Errorf("flag(%s) = %q, want %q", code, got, want)
		}
	}
	tag, _ := parseSource("nl")
	if got := marker(tag); got != "\n-# translated from \U0001F1F3\U0001F1F1 - edited by merlin" {
		t.Errorf("marker %q", got)
	}
}

func TestPromptSaysWhichEnglish(t *testing.T) {
	plain := systemPrompt("- threats: no threats\n", nil)
	if strings.Contains(plain, "Vernacular") || !strings.Contains(plain, standard.how) || !strings.Contains(plain, "- threats: no threats") {
		t.Error("the plain prompt carries a dialect, or lacks the English or the policies")
	}
	on := systemPrompt("", dialects)
	if !strings.Contains(on, `Only when source is "nl"`) || !strings.Contains(on, dialects["nl"].how) || !strings.Contains(on, standard.how) {
		t.Error("the dialect prompt does not name Dutch")
	}
	if englishFor(dialects, "de") != standard || englishFor(nil, "nl") != standard || englishFor(dialects, "nl") == standard {
		t.Error("the wrong language got the dialect")
	}
}

// What other people said goes along without who they are or where they link.
func TestUserPromptCarriesNoIDs(t *testing.T) {
	got := userPrompt("hoi", "kijk <@123456789012345678>", []string{"B: see https://x.com <#123456789012345678>", "A: " + strings.Repeat("ha", 600)}, "A")
	if strings.Contains(got, "1234567") || strings.Contains(got, "x.com") {
		t.Errorf("prompt %q", got)
	}
	if !strings.HasSuffix(got, "written by A:\nhoi") || len(got) > 1200 {
		t.Errorf("prompt of %d %q", len(got), got)
	}
	if lines := strings.Split(userPrompt("hoi", "", nil, "A"), "\n"); !slices.Equal(lines, []string{"The message to translate, written by A:", "hoi"}) {
		t.Errorf("bare prompt %q", lines)
	}
}

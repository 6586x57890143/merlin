package aimod

import "testing"

func TestFoldCatchesUnicodeEvasions(t *testing.T) {
	for _, s := range []string{
		"ｎｉｇｇｅｒ",                               // fullwidth
		"𝐧𝐢𝐠𝐠𝐞𝐫",                               // math bold
		"𝓷𝓲𝓰𝓰𝓮𝓻",                               // math script
		"ɴɪɢɢᴇʀ",                               // small capitals
		"ⓝⓘⓖⓖⓔⓡ",                               // circled
		"🇳🇮🇬🇬🇪🇷",                               // regional indicators
		"n\u200bi\u200bg\u200bg\u200be\u200br", // zero-width spaces
		"n\u00adi\u00adg\u00adg\u00ade\u00adr", // soft hyphens
		"n\u3164i\u3164g\u3164g\u3164e\u3164r", // Hangul filler
		"ńíggér",                               // accents
		"n̶i̶g̶g̶e̶r̶",                         // combining strikethrough
		"f̷̛a̸g̴g̵o̶t̷",                        // zalgo
		"kіkе",                                 // Cyrillic i and e
		"ƒaggot",                               // hooked f
		"tгаnny",                               // Cyrillic r and a
		"\u2063nigger",                         // invisible separator up front
		"you ｆａｇｇｏｔ lol",
	} {
		if _, _, _, hit := hardHit(s); !hit {
			t.Errorf("hardHit(%q) missed", s)
		}
	}
}

func TestFoldLeavesInnocentTextAlone(t *testing.T) {
	for _, s := range []string{
		"café au lait 👨‍👩‍👧 ok",
		"Привет, как дела?",
		"Καλημέρα σε όλους",
		"そうですね",
		"1️⃣ first place 🏴󠁧󠁢󠁳󠁣󠁴󠁿",
	} {
		if _, _, _, hit := hardHit(s); hit {
			t.Errorf("hardHit(%q) hit innocent text", s)
		}
		if out, hit := redactSlurs(s); hit || out != s {
			t.Errorf("redactSlurs(%q) = %q, %v; want it untouched", s, out, hit)
		}
	}
	// A hit keeps the emoji ZWJ sequence joined.
	out, hit := redactSlurs("ｎｉｇｇｅｒ 👨‍👩‍👧")
	if !hit || out[len(out)-len("👨‍👩‍👧"):] != "👨‍👩‍👧" {
		t.Errorf("redactSlurs broke the emoji: %q", out)
	}
}

func TestMustScanFolds(t *testing.T) {
	for _, s := range []string{"ｍｉｎｏｒ", "kіds", "l\u200boli"} {
		if !mustScan(s) {
			t.Errorf("mustScan(%q) = false", s)
		}
	}
}

func TestWordListFolds(t *testing.T) {
	w, err := normalizeWord("Ｗöｒｋ")
	if err != nil || w != "work" {
		t.Fatalf("normalizeWord folded to %q, %v; want work", w, err)
	}
	list := []BannedWord{{Word: w, Replacement: "play"}}
	for _, s := range []string{"ｗｏｒｋ", "wоrk", "w\u200bo\u200br\u200bk", "ẃörk̶"} {
		if _, hit := redactWords(list, s); !hit {
			t.Errorf("redactWords(%q) missed", s)
		}
	}
	if out, hit := redactWords(list, "café"); hit || out != "café" {
		t.Errorf("redactWords touched a miss: %q", out)
	}
}

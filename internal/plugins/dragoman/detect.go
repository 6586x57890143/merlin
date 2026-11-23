package dragoman

import (
	"slices"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/rangetable"
)

// The free gate: which messages are worth asking a model about.
//
// Nearly every message on an English server is English, so this has to say
// no to all of those without spending anything. It answers "might this not
// be English", never "what is this": the model names the language and can
// still say leave it alone. So it is tuned to miss a one-word message in
// Dutch rather than to pay for "wat" and "lol", and a member who was just
// speaking Dutch needs less to go on (sticky).

// cjk is the scripts Chinese and Japanese mix in one word.
var cjk = rangetable.Merge(unicode.Han, unicode.Hiragana, unicode.Katakana)

// foreign are scripts English is not written in. Fancy text (mathematical
// letters, circled letters) belongs to none of them.
var foreign = []*unicode.RangeTable{
	cjk, unicode.Cyrillic, unicode.Greek, unicode.Arabic, unicode.Hebrew, unicode.Hangul, unicode.Devanagari, unicode.Bengali, unicode.Tamil, unicode.Telugu,
	unicode.Gujarati, unicode.Gurmukhi, unicode.Malayalam, unicode.Kannada, unicode.Sinhala, unicode.Thai,
	unicode.Lao, unicode.Khmer, unicode.Myanmar, unicode.Georgian, unicode.Armenian, unicode.Ethiopic,
}

// functionWords are the commonest small words of each language written in
// Latin letters, less every one that is also an English word, a name, or
// ordinary chat slang: those say nothing. Two different ones are enough.
var functionWords = wordTable(map[string]string{
	"nl": `het een niet ik jij je dat maar ook zijn heb hebt heeft wat voor naar nog wel geen hoe waarom gewoon
		deze dit als uit zo mijn jouw wij jullie worden wordt moet echt veel toch weer gaat gaan klopt ja nee
		bent aan hier daar niks iets omdat met doen weet kunnen moeten vandaag morgen lekker`,
	"de": `ich nicht und der das ist ein eine mit auf für dich mich wir ihr sie es aber auch noch schon wenn
		warum kein keine doch habe hast sind bist wie oder nur immer jetzt heute ja nein dass daß über haben
		wird kann muss mir dir weil sehr hier`,
	"fr": `je ne pas les des une dans que qui avec mais vous nous ils elle c'est j'ai tu ça très aussi suis
		sont même quoi pourquoi où oui être avoir cette leur moi toi rien quand comme parce bien faire`,
	"es": `las que es para una como pero muy porque esta está qué tú también cuando todo eso ese hay
		soy estoy tengo quiero donde sí nada más te lo se mucho bien ahora aquí puede`,
	"pt": `não você que uma para muito está isso meu minha ele ela também quando porque foi tem estou nós são
		vou pra aqui mas um com por mais bem agora fazer tudo`,
	"it": `che sono una gli della anche perché questo questa cosa è ho hai sei lui lei più molto bene quando
		dove ti si un mi ci fare adesso tutto niente`,
	"pl": `nie się że jak ale tak czy mam masz dla tylko już jestem bardzo dlaczego po od mnie jest co
		będzie teraz gdzie`,
	"sv": `jag inte och det är att som för med på du en vi om vad varför mycket också bara nu ska`,
	"da": `jeg ikke og det er som på du en vi om hva hvorfor hvad også mye meget nå nu skal bare fordi`,
	"fi": `minä sinä ei ja että mutta niin kun se oli olen mitä miksi hän ovat vain kyllä nyt mä sä joo`,
	"tr": `bir ve bu için çok değil ama gibi nasıl neden şey daha evet hayır yok sen şimdi`,
	"id": `yang tidak ini itu saya kamu aku untuk dengan ada apa tapi juga sudah bisa nggak gak kenapa lagi
		banget mau`,
	"tl": `ng mga hindi ako ikaw siya naman lang kasi pero bakit ano opo yung niya nila`,
	"ro": `și este sunt pentru că mai cu ce foarte acum asta vreau eu nu dar cum din`,
	"hu": `nem hogy és egy az csak mert itt vagy ez azt én miért igen jó nagyon most`,
	"cs": `není jsem že ale tak jak proč jen už jsou bych mám máš něco protože som nie prečo čo jsi`,
	"hr": `nije što kako ali samo zašto ili imam znam ovo koji sada da sve nisam`,
	"vi": `không là của và một những được cho tôi bạn người này có rất`,
	"af": `ek nie het wat ons jy sal baie hierdie vir dit`,
	"sq": `nuk është një dhe për kam çfarë unë`,
	"lt": `yra kad aš tai kaip bet dabar labai`,
	"et": `ei ja see ma sa mis kas aga olen väga`,
	"ca": `amb però què això aquest són molt una`,
	"sw": `hii sana nini kuwa hapana yangu kwa ndiyo`,
	"cy": `mae dw i'n ddim beth sut rydw`,
	"ga": `tá níl agus bhí conas cad`,
})

// englishWords outvote the above. The ones that are small words somewhere
// else too ("is", "in", "was", "of") are left out.
var englishWords = set(`the and you to that it for this with are be have not but what my just don't i'm it's can
	like your they she if get yes up when who there would about know how why because them him her his our then
	than from has had did does doing been being were will should could these those where which while got get
	want think really thing people time good new now make some any only very even back after into over never
	always something nothing anyone everyone lol lmao bro yeah gonna wanna ain't can't won't didn't doesn't
	isn't that's what's he's she's you're they're we're i've i'll let's please thanks`)

func set(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.Fields(s) {
		out[w] = true
	}
	return out
}

// wordTable turns the lists inside out: word to the languages it is in.
func wordTable(lists map[string]string) map[string][]string {
	out := map[string][]string{}
	for lang, list := range lists {
		for w := range set(list) {
			out[w] = append(out[w], lang)
		}
	}
	return out
}

// candidate reports whether text might be worth translating. sticky is the
// language its author was last confirmed speaking, or "".
func candidate(text, sticky string) bool {
	if strings.ContainsAny(text, "<`@:./") {
		text = carriers.ReplaceAllString(text, " ")
	}
	if foreignScript(text) {
		return true
	}
	var english int
	hits := map[string]int{}
	seen := map[string]bool{}
	text = strings.ToLower(strings.ReplaceAll(text, "’", "'"))
	for w := range strings.FieldsFuncSeq(text, func(r rune) bool { return !unicode.IsLetter(r) && r != '\'' }) {
		w = strings.Trim(w, "'")
		if w == "" || seen[w] {
			continue
		}
		seen[w] = true
		if englishWords[w] {
			english++
			continue
		}
		for _, lang := range functionWords[w] {
			hits[lang]++
		}
	}
	if sticky != "" && hits[sticky] > english {
		return true
	}
	best := 0
	for _, n := range hits {
		best = max(best, n)
	}
	return best >= 2 && best > english
}

// foreignScript reports whether most of text's letters are in a script
// English is not written in: and three in a row are of one script and not
// all the same. An emoticon borrows a letter here and there, and a run of one
// letter is a laugh.
func foreignScript(text string) bool {
	var letters, other int
	var word, mixed bool
	last, lastScript, streak := rune(0), -1, 0
	for _, r := range text {
		script := -1
		if unicode.IsLetter(r) || (r > 0x7f && unicode.IsMark(r)) {
			letters++
			script = slices.IndexFunc(foreign, func(t *unicode.RangeTable) bool { return r > 0x7f && unicode.Is(t, r) })
		}
		if script >= 0 {
			other++
			if script != lastScript {
				streak, mixed = 0, false
			}
			streak++
			mixed = mixed || (streak > 1 && r != last)
			word = word || (streak >= 3 && mixed)
		}
		last, lastScript = r, script
	}
	return word && other*2 > letters
}

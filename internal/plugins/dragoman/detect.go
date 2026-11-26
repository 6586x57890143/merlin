package dragoman

import (
	_ "embed"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

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
//
// Three tests, any of which is enough: a foreign script, the small words of
// one language (which catch short messages), or words that mostly are not
// English at all (which catch a sentence with no small words in it).

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
// ordinary chat slang: those say nothing. Two different ones are enough. These
// were picked by hand; words.txt adds the commonest 300 of each language.
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
	"fil": `ng mga hindi ako ikaw siya naman lang kasi pero bakit ano opo yung niya nila`,
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
}, commonWords)

// commonWords is a line for each language: its code and its commonest words,
// counted in Tatoeba's sentences (tatoeba.org, CC BY 2.0), less English ones
// (known, or as common in English sentences and tweets) and names.
//
//go:embed words.txt
var commonWords string

// borrowed are phrases English has taken whole, which no list of words can
// tell from the language they came from.
var borrowed = regexp.MustCompile(`(?i)\b(c'est la vie|que sera,? sera|hasta la vista|te amo|mi amor|du jour|mon ami|mi casa|su casa|` +
	`gracias amigo|muchas gracias|buenos? d[ií]as?|buenas noches|merci beaucoup|danke sch(?:ö|oe)n|guten tag|gute nacht|auf wiedersehen|` +
	`bon app[eé]tit|bon voyage|au revoir|je ne sais quoi|d[eé]j[aà] vu|raison d'être|la la land|la vida loca|por favor|no bueno|ad hoc|per se|et al|et cetera)\b`)

// englishWords outvote the above. The ones that are small words somewhere
// else too ("is", "in", "was", "of") are left out.
var englishWords = set(`the and you to that it for this with are be have not but what my just don't i'm it's can
	like your they she if get yes up when who there would about know how why because them him her his our then
	than from has had did does doing been being were will should could these those where which while got get
	want think really thing people time good new now make some any only very even back after into over never
	always something nothing anyone everyone lol lmao bro yeah gonna wanna ain't can't won't didn't doesn't
	isn't that's what's he's she's you're they're we're i've i'll let's please thanks`)

// vocabulary is the 40,000 commonest words of English, from SCOWL (its
// licence is english.LICENSE), and slang is what people type that no word
// list has. Regenerated from the wordlist-english npm package, sizes 10 to
// 35: lowercase, letters only, and of the two-letter entries only the words.
//
//go:embed english.txt
var vocabularyList string

var (
	vocabulary = set(vocabularyList)
	slang      = set(`lol lmao lmfao rofl bruh bro bros sis fam yo ya yall ayo nah naw yea yeah yep yup nope ok okay kk idk idc
		idgaf tbh ngl imo imho fr frfr ong smh tf wtf wth stfu omg omfg btw brb afk gg ggs ez wp glhf irl dm dms lmk hmu
		wyd wya wdym hbu tysm ty thx pls plz ppl smth smthn sth nvm rn atm asap fyi jk gn gm hru ik ikr ion ig iirc iykyk
		istg tho thru cuz cus coz bc bcuz gonna wanna gotta gimme lemme kinda sorta dunno tryna finna boutta imma ima aint
		ur u r n y k im ive id ill dont doesnt didnt cant wont isnt wasnt arent werent couldnt wouldnt shouldnt hasnt
		havent thats whats hes shes theyre youre weve theres heres lets based cringe mid sus cap nocap bet goat goated
		rizz gyatt skibidi sigma ohio slay yeet simp stan vibe vibes vibing delulu bussin drip lit af asf fuckin fucking
		fucked fucker motherfucker shit shitty bullshit bitch bitches asshole dumbass jackass badass ass damn goddamn
		dude dudes guys homie homies chill ded oof yikes pog poggers kek kekw copium hopium meme memes noob newb op nerf
		buff buffed lag laggy hacker cheater app apps wifi online email emails blog vlog streamer twitch discord reddit
		youtube google tiktok insta instagram snapchat twitter spotify netflix xbox ps steam minecraft valorant fortnite
		roblox iphone android pc gpu cpu ram ai llm gpt bot bots admin admins mod mods ping pinged server servers emoji
		emojis gif gifs pfp nitro vc ava avi selfie selfies cringey weirdo lame legit lowkey highkey deadass facts cope
		ratio mald malding seethe hella mf mfs mfer mfers tn tmr tmrw yday pic pics vid vids sec secs mins hrs aight alr
		alright bday prolly def defo obv obvs totes whatcha watcha gotcha betcha innit bloke mate mates lads wanker cunt
		bollocks oi bruv gov naur heh hehe haha hahaha hah lolol uh uhh um umm hmm hm mhm eh meh ugh huh yay wow whoa woah
		welp ew eww aw aww hey heyo hiya sup wassup whaddup howdy yessir nuh uhuh ts pmo sm`)
)

// known reports whether w, in lowercase, is English enough: a word, slang, a
// word stretched out ("sooooo"), or one ending in what chat adds to words.
func known(w string) bool {
	if vocabulary[w] || slang[w] {
		return true
	}
	short := unstretch(w)
	if vocabulary[short] || slang[short] {
		return true
	}
	if base, _, ok := strings.Cut(w, "'"); ok {
		return base == "" || known(base) // it's, goin', y'all
	}
	for _, end := range []string{"s", "in", "ing", "ed", "er", "y"} {
		if base, ok := strings.CutSuffix(w, end); ok && len(base) > 2 && (vocabulary[base] || vocabulary[base+"e"] || slang[base]) {
			return true
		}
	}
	return false
}

func set(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.Fields(s) {
		out[w] = true
	}
	return out
}

// unstretch cuts a letter typed three times or more down to one.
func unstretch(w string) string {
	var b strings.Builder
	r := []rune(w)
	for i := 0; i < len(r); {
		j := i
		for j < len(r) && r[j] == r[i] {
			j++
		}
		n := j - i
		if n >= 3 {
			n = 1
		}
		for range n {
			b.WriteRune(r[i])
		}
		i = j
	}
	return b.String()
}

// wordTable turns the lists inside out: word to the languages it is in.
func wordTable(lists map[string]string, common string) map[string][]string {
	for line := range strings.Lines(common) {
		lang, list, _ := strings.Cut(line, " ")
		lists[lang] += " " + list
	}
	out := map[string][]string{}
	for lang, list := range lists {
		for w := range set(list) {
			out[w] = append(out[w], lang)
		}
	}
	return out
}

// evidence is how much there is that a text isn't English. Weak is worth
// asking about, and worth believing when the answer is no.
type evidence int

const (
	none evidence = iota
	weak
	strong
)

// gauge is how likely text is worth translating. sticky is the language its
// author was last confirmed speaking, or "".
func gauge(text, sticky string) evidence {
	text = strings.ReplaceAll(text, "\u2019", "'")
	e := weigh(text, sticky)
	// Only once there is something to explain away: this is slow, and nearly
	// every message is English.
	if e != none && borrowed.MatchString(text) {
		return weigh(borrowed.ReplaceAllString(text, " "), sticky)
	}
	return e
}

func weigh(text, sticky string) evidence {
	if strings.ContainsAny(text, "<`@:./") {
		text = carriers.ReplaceAllString(text, " ")
	}
	if foreignScript(text) {
		return strong
	}
	var english, words, unknown int
	hits := map[string]int{}
	seen := map[string]bool{}
	shouting := strings.ToUpper(text) == text
	first := true
	for typed := range strings.FieldsFuncSeq(text, func(r rune) bool { return !unicode.IsLetter(r) && r != '\'' }) {
		w := strings.ToLower(strings.Trim(typed, "'"))
		if w == "" || seen[w] {
			continue
		}
		seen[w] = true
		// A capital inside a sentence is a name, which says nothing about
		// the language around it.
		name := !first && !shouting && unicode.IsUpper([]rune(typed)[0])
		acronym := !shouting && len(w) > 1 && strings.ToUpper(typed) == typed
		first = false
		if !name && !acronym && utf8.RuneCountInString(w) > 1 {
			words++
			if !known(w) {
				unknown++
			}
		}
		if englishWords[w] {
			english++
			continue
		}
		if name {
			continue
		}
		for _, lang := range functionWords[w] {
			hits[lang]++
		}
	}
	best := 0
	for _, n := range hits {
		best = max(best, n)
	}
	switch {
	case best > english && (best >= 3 || (best == 2 && unknown*2 > words)):
		return strong
	// Three words that are not English, out of at most five, or every word
	// of a short message, one of them common somewhere.
	case unknown >= 3 && unknown*5 >= words*3,
		best >= 1 && english == 0 && unknown >= 2 && unknown*3 >= words*2,
		sticky != "" && (hits[sticky] > english || (unknown >= 2 && unknown*2 > words)),
		// Mid conversation, a word no list knows and nothing English: a
		// one-word answer ("Zwartjoekel") is in the language it answers.
		sticky != "" && english == 0 && words > 0 && unknown == words,
		best >= 2 && best > english:
		return weak
	}
	return none
}

// leaning is the language text looks most like, or "" where it doesn't
// look foreign: the conversation a reply joins, for gauge's sticky.
func leaning(text string) string {
	if gauge(text, "") == none {
		return ""
	}
	hits := map[string]int{}
	for w := range strings.FieldsFuncSeq(strings.ToLower(carriers.ReplaceAllString(text, " ")), func(r rune) bool { return !unicode.IsLetter(r) && r != '\'' }) {
		for _, lang := range functionWords[strings.Trim(w, "'")] {
			hits[lang]++
		}
	}
	best := ""
	for lang, n := range hits {
		if n > hits[best] || n == hits[best] && lang < best {
			best = lang
		}
	}
	return best
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

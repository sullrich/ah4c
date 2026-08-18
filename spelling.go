package main

import "strings"

// Which English the captions are spelled in, for models that were not asked.
//
// The models that do take a locale are told one. Canary is not: its own command
// line offers "en", "de", "es" and "fr" and no variants, so it writes whichever
// spelling its training data leaned toward and there is nothing to pass it. It
// wrote "tyre".
//
// So this is a substitution over the finished text and nothing cleverer. Rules
// were the obvious alternative and they are wrong more often than they are
// right: -ise to -ize turns "surprise" into "surprize" and "exercise" into
// "exercize", and -our to -or turns "four" into "for". A list is only as good as
// its entries and it never invents one.
//
// Whole words only, so "tyres" needs its own entry and "satyr" is untouched.

// britishSpellings maps a British spelling to its American form. Lowercase
// keys; the case of what was written is put back afterward.
var britishSpellings = func() map[string]string {
	m := map[string]string{}
	// The -re words.
	for _, w := range []string{"centre", "centres", "centred", "theatre", "theatres",
		"metre", "metres", "litre", "litres", "fibre", "fibres", "calibre", "sabre",
		"sombre", "spectre", "lustre", "meagre"} {
		m[w] = strings.TrimSuffix(w, "re") + "er"
	}
	// The -our words, and their obvious inflections.
	for _, w := range []string{"colour", "favour", "flavour", "honour", "humour",
		"labour", "neighbour", "rumour", "behaviour", "harbour", "odour", "vapour",
		"armour", "parlour", "saviour", "endeavour", "splendour", "candour", "clamour",
		"valour", "rigour", "vigour"} {
		base := strings.TrimSuffix(w, "our") + "or"
		m[w] = base
		m[w+"s"] = base + "s"
		m[w+"ed"] = base + "ed"
		m[w+"ing"] = base + "ing"
	}
	// The -ise words that really are -ize in American English. Only these:
	// "surprise", "advertise", "exercise", "compromise" and their like are
	// spelled the same on both sides and are deliberately absent.
	for _, w := range []string{"realise", "organise", "recognise", "apologise",
		"criticise", "emphasise", "specialise", "normalise", "authorise", "memorise",
		"minimise", "maximise", "prioritise", "summarise", "categorise", "modernise",
		"publicise", "sympathise", "utilise", "civilise", "colonise", "harmonise",
		"analyse", "paralyse", "catalyse"} {
		base := w
		switch {
		case strings.HasSuffix(w, "yse"):
			base = strings.TrimSuffix(w, "yse") + "yze"
		default:
			base = strings.TrimSuffix(w, "ise") + "ize"
		}
		m[w] = base
		m[strings.TrimSuffix(w, "e")+"ed"] = strings.TrimSuffix(base, "e") + "ed"
		m[strings.TrimSuffix(w, "e")+"ing"] = strings.TrimSuffix(base, "e") + "ing"
		m[w+"s"] = base + "s"
	}
	// The doubled consonant before a suffix.
	for b, a := range map[string]string{
		"travelled": "traveled", "travelling": "traveling", "traveller": "traveler",
		"cancelled": "canceled", "cancelling": "canceling",
		"labelled": "labeled", "labelling": "labeling",
		"modelled": "modeled", "modelling": "modeling",
		"marvelled": "marveled", "signalled": "signaled", "totalled": "totaled",
		"fuelled": "fueled", "duelled": "dueled", "levelled": "leveled",
		"marvellous": "marvelous", "jeweller": "jeweler", "jewellery": "jewelry",
	} {
		m[b] = a
	}
	// The rest, one at a time.
	for b, a := range map[string]string{
		"tyre": "tire", "tyres": "tires",
		"grey": "gray", "greyer": "grayer", "greyish": "grayish",
		"defence": "defense", "offence": "offense", "pretence": "pretense",
		"licence": "license", "practise": "practice", "practised": "practiced",
		"programme": "program", "programmes": "programs",
		"catalogue": "catalog", "catalogues": "catalogs",
		"analogue": "analog", "dialogue": "dialog",
		"aluminium": "aluminum", "cheque": "check", "cheques": "checks",
		"storey": "story", "storeys": "stories",
		"mould": "mold", "moulded": "molded", "moulding": "molding",
		"smoulder": "smolder", "smouldering": "smoldering",
		"moustache": "mustache", "pyjamas": "pajamas",
		"sceptical": "skeptical", "sceptic": "skeptic", "scepticism": "skepticism",
		"aeroplane": "airplane", "kerb": "curb", "plough": "plow", "ploughed": "plowed",
		"draught": "draft", "draughts": "drafts",
		"whilst": "while", "amongst": "among",
		"judgement": "judgment", "judgements": "judgments",
		"ageing": "aging", "enrol": "enroll", "fulfil": "fulfill",
		"instalment": "installment", "skilful": "skillful", "wilful": "willful",
		"manoeuvre": "maneuver", "manoeuvres": "maneuvers",
		"gaol": "jail", "kilometre": "kilometer", "kilometres": "kilometers",
		"tonne": "metric ton", "towards": "toward",
	} {
		m[b] = a
	}
	return m
}()

// oneWay is the American spelling of a pair that must not be turned back.
//
// The table reads cleanly in one direction and not in the other, because
// several American spellings are two British words at once. "story" is a tale
// on both sides of the Atlantic and a floor only in Britain, so turning every
// "story" into "storey" would rewrite the news. "check" is a cheque only when
// it is money. "program" is what a computer runs in both. "draft" is a draught
// only when it is beer or a breeze. "curb" is a kerb only when it is a street.
//
// Going the other way there is no such doubt: a British "storey" is always a
// floor, a "cheque" always money. So the British direction is the same table
// inverted with these left out, rather than a second list to keep in step.
var oneWay = map[string]bool{
	"story": true, "stories": true, "check": true, "checks": true,
	"license": true, "practice": true, "practiced": true,
	"program": true, "programs": true, "draft": true, "drafts": true,
	"curb": true, "jail": true, "metric ton": true, "toward": true,
	"while": true, "among": true, "tire": true, "tires": true,
}

// americanSpellings is the table read the other way, for somebody who wants
// what was said spelled the way they spell it.
var americanSpellings = func() map[string]string {
	m := map[string]string{}
	for br, us := range britishSpellings {
		if oneWay[us] {
			continue
		}
		// First entry wins, so a shared American form keeps one British
		// spelling rather than whichever the map iterated to last.
		if _, seen := m[us]; !seen {
			m[us] = br
		}
	}
	return m
}()

// Spelling choices, as the page stores them.
const (
	spellAsRecognized = ""
	spellAmerican     = "us"
	spellBritish      = "gb"
)

// respell rewrites the spellings in recognized text to the wanted variety.
//
// Its own function with its own test because it is a rule that changes what
// goes on the screen, and a substitution table that fires on the wrong word is
// the kind of fault nobody notices until it is in a recording.
//
// Anything the table does not know is left exactly as the model wrote it. That
// is the whole contract: this corrects a spelling or it does nothing, and it
// never guesses at a word it has not been told about.
func respell(text, want string) string {
	var table map[string]string
	switch want {
	case spellAmerican:
		table = britishSpellings
	case spellBritish:
		table = americanSpellings
	default:
		return text
	}
	if text == "" {
		return text
	}
	words := strings.Split(text, " ")
	changed := false
	for i, w := range words {
		lead, core, tail := splitAffixes(w)
		if core == "" {
			continue
		}
		to, ok := table[strings.ToLower(core)]
		if !ok {
			continue
		}
		words[i] = lead + matchCase(core, to) + tail
		changed = true
	}
	if !changed {
		return text
	}
	return strings.Join(words, " ")
}

// splitAffixes takes the punctuation off both ends of a word so the middle can
// be looked up, and hands back the pieces to put it together again.
func splitAffixes(w string) (lead, core, tail string) {
	const marks = `"'“”‘’()[]{}.,!?;:—–…`
	core = strings.Trim(w, marks)
	if core == "" {
		return "", "", ""
	}
	i := strings.Index(w, core)
	return w[:i], core, w[i+len(core):]
}

// matchCase writes the replacement the way the original was written: all
// capitals stay all capitals, a leading capital stays a leading capital, and
// anything else is left lowercase.
func matchCase(from, to string) string {
	switch {
	case from == strings.ToUpper(from) && from != strings.ToLower(from):
		return strings.ToUpper(to)
	case from != "" && strings.ToUpper(from[:1]) == from[:1]:
		return strings.ToUpper(to[:1]) + to[1:]
	}
	return to
}

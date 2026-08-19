package main

// The CEA-608 encoder: turning recognized text into the byte pairs that ride
// in the caption channel.
//
// Both presentation styles live here — the roll-up that types onto the screen
// and the box that appears whole — along with the pacing that decides when a
// pair leaves and where a caption breaks.

import (
	"math"
	"strings"
	"sync"
	"time"
	"unicode"
)

// CEA-608 encoding
// ---------------------------------------------------------------------------

// CEA-608 carries two bytes per video frame on field 1. At 29.97 fps that is
// about 60 characters a second, comfortably ahead of the ~13 characters a
// second that ordinary speech produces, so the queue below drains faster than
// the recognizer fills it.

const (
	cc608Null = 0x00 // padding, before parity

	ccCtrlCC1 = 0x14 // control code channel prefix for CC1
	ccRU2     = 0x25 // roll up 2 rows
	ccRU3     = 0x26 // roll up 3 rows
	ccRU4     = 0x27 // roll up 4 rows
	ccCR      = 0x2D // carriage return
	ccEDM     = 0x2C // erase displayed memory
	ccEOC     = 0x2F // end of caption (pop-on flip)
	ccRCL     = 0x20 // resume caption loading
	ccENM     = 0x2E // erase non-displayed memory
)

// odd608 sets bit 7 so the byte carries odd parity, which is what a 608 decoder
// expects on the wire.
func odd608(b byte) byte {
	v := b & 0x7F
	ones := 0
	for i := 0; i < 7; i++ {
		if v&(1<<i) != 0 {
			ones++
		}
	}
	if ones%2 == 0 {
		return v | 0x80
	}
	return v
}

// cc608Char maps a rune onto the CEA-608 basic character set.
//
// That set is ASCII with a handful of positions given over to accented letters:
// á é í ó ú ç ñ Ñ are carried natively and are emitted as themselves. The rest
// of Europe's letters have no code point here, and dropping one leaves a hole
// in the middle of a word — "café" arrived as "caf ", which reads as a typo
// rather than as a missing glyph — so those are folded to the nearest letter a
// viewer would recognize.
//
// The ASCII characters occupying the accented positions have to be blanked
// rather than passed through, or an asterisk would be shown as an á.
func cc608Char(r rune) byte {
	if b, ok := cc608Native[r]; ok {
		return b
	}
	switch {
	case r >= 0x20 && r <= 0x7F:
		switch r {
		case '*', '\\', '^', '_', '`', '{', '|', '}', '~', 0x7F:
			// These positions carry á é í ó ú ç ÷ Ñ ñ and a solid block.
			return ' '
		}
		return byte(r)
	}
	if b, ok := cc608Fold[r]; ok {
		return b
	}
	return ' '
}

// cc608Native are the letters the basic character set carries in place of
// certain ASCII codes.
var cc608Native = map[rune]byte{
	'á': 0x2A, 'é': 0x5C, 'í': 0x5E, 'ó': 0x5F, 'ú': 0x60,
	'ç': 0x7B, '÷': 0x7C, 'Ñ': 0x7D, 'ñ': 0x7E,
}

// cc608Fold folds the letters the European languages need onto the basic set.
// It is not a transliteration scheme, just the nearest letter a viewer would
// recognize, which is what a caption needs.
var cc608Fold = map[rune]byte{
	'à': 'a', 'â': 'a', 'ä': 'a', 'ã': 'a', 'å': 'a', 'ā': 'a', 'ă': 'a', 'ą': 'a',
	'Á': 'A', 'À': 'A', 'Â': 'A', 'Ä': 'A', 'Ã': 'A', 'Å': 'A', 'Ā': 'A', 'Ă': 'A', 'Ą': 'A',
	'è': 'e', 'ê': 'e', 'ë': 'e', 'ē': 'e', 'ĕ': 'e', 'ė': 'e', 'ę': 'e', 'ě': 'e',
	'É': 'E', 'È': 'E', 'Ê': 'E', 'Ë': 'E', 'Ē': 'E', 'Ė': 'E', 'Ę': 'E', 'Ě': 'E',
	'ì': 'i', 'î': 'i', 'ï': 'i', 'ī': 'i', 'į': 'i', 'ı': 'i',
	'Í': 'I', 'Ì': 'I', 'Î': 'I', 'Ï': 'I', 'Ī': 'I', 'Į': 'I', 'İ': 'I',
	'ò': 'o', 'ô': 'o', 'ö': 'o', 'õ': 'o', 'ø': 'o', 'ō': 'o', 'ő': 'o',
	'Ó': 'O', 'Ò': 'O', 'Ô': 'O', 'Ö': 'O', 'Õ': 'O', 'Ø': 'O', 'Ō': 'O', 'Ő': 'O',
	'ù': 'u', 'û': 'u', 'ü': 'u', 'ū': 'u', 'ů': 'u', 'ű': 'u', 'ų': 'u',
	'Ú': 'U', 'Ù': 'U', 'Û': 'U', 'Ü': 'U', 'Ū': 'U', 'Ů': 'U', 'Ű': 'U', 'Ų': 'U',
	'ý': 'y', 'ÿ': 'y', 'Ý': 'Y', 'Ŷ': 'Y', 'ŷ': 'y',
	'ń': 'n', 'ň': 'n', 'ņ': 'n', 'Ń': 'N', 'Ň': 'N', 'Ņ': 'N',
	'ć': 'c', 'č': 'c', 'ĉ': 'c', 'Ç': 'C', 'Ć': 'C', 'Č': 'C', 'Ĉ': 'C',
	'š': 's', 'ś': 's', 'ş': 's', 'ŝ': 's', 'Š': 'S', 'Ś': 'S', 'Ş': 'S', 'Ŝ': 'S',
	'ž': 'z', 'ź': 'z', 'ż': 'z', 'Ž': 'Z', 'Ź': 'Z', 'Ż': 'Z',
	'ł': 'l', 'ľ': 'l', 'ĺ': 'l', 'ļ': 'l', 'Ł': 'L', 'Ľ': 'L', 'Ĺ': 'L', 'Ļ': 'L',
	'ť': 't', 'ţ': 't', 'ŧ': 't', 'Ť': 'T', 'Ţ': 'T', 'Ŧ': 'T',
	'ď': 'd', 'đ': 'd', 'ð': 'd', 'Ď': 'D', 'Đ': 'D', 'Ð': 'D',
	'ř': 'r', 'ŕ': 'r', 'Ř': 'R', 'Ŕ': 'R',
	'ğ': 'g', 'ģ': 'g', 'ġ': 'g', 'Ğ': 'G', 'Ģ': 'G', 'Ġ': 'G',
	'ķ': 'k', 'Ķ': 'K', 'ħ': 'h', 'Ħ': 'H',
	'þ': 'p', 'Þ': 'P', 'ŭ': 'u', 'Ŭ': 'U',
	'‘': '\'', '’': '\'', '‚': '\'', '‹': '<', '›': '>',
	'“': '"', '”': '"', '„': '"', '«': '"', '»': '"',
	'—': '-', '–': '-', '‑': '-', '−': '-',
	'…': '.', '·': '.', '•': '-',
	'\u00a0': ' ', '\u202f': ' ', '\u2009': ' ',
}

// cc608Expand spells out the characters that carry meaning and have no letter
// to fold onto, so a price or a fraction is not silently blanked.
var cc608Expand = map[rune]string{
	'€': "EUR", '£': "GBP", '¥': "YEN", '¢': "c", '¤': "",
	'æ': "ae", 'Æ': "AE", 'œ': "oe", 'Œ': "OE", 'ß': "ss",
	'½': "1/2", '¼': "1/4", '¾': "3/4", '°': " degrees",
	'±': "+/-", '×': "x", '™': "(TM)", '©': "(C)", '®': "(R)",
	'µ': "u", '§': "S", '¶': "P", '†': "+", '‡': "++",
}

// cc608ExpandText spells out the characters that have no single letter to fold
// onto, before the text is laid out on the caption grid.
func cc608ExpandText(text string) string {
	if !strings.ContainsFunc(text, func(r rune) bool { _, ok := cc608Expand[r]; return ok }) {
		return text
	}
	var b strings.Builder
	b.Grow(len(text) + 8)
	for _, r := range text {
		if s, ok := cc608Expand[r]; ok {
			b.WriteString(s)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// cea608 turns lines of recognized text into the byte pairs that ride in the
// caption stream, one pair per video frame.
type cea608 struct {
	mu    sync.Mutex
	queue [][2]byte
	// lastText is when text was last queued. A caption left on screen after
	// everything upstream has stopped is worse than no caption: it is a
	// sentence from four minutes ago presented as if it were current. A
	// broadcast encoder erases after a while and so does this.
	lastText time.Time
	// lastCR is when the display last rolled, and crCopies how many repeats of
	// that carriage return are still owed; see next() for what they pace.
	lastCR   time.Time
	crCopies int
	// popon writes the caption where it cannot be seen and then shows it whole,
	// instead of typing it onto the screen as it arrives. held is the words of
	// the caption being assembled, which have not been sent anywhere yet.
	// lastBlock is when a caption was last shown and blockCopies how many
	// repeats of that command are still owed, the same bookkeeping the roll
	// keeps for its carriage return.
	popon bool
	// boxRows is how many rows a box caption may fill.
	//
	// Two, and it is not a setting. A caption of more than two lines is not a
	// box — every published style guide caps it there, and the reason is that
	// three lines cover enough of the picture to be worth avoiding. It decides
	// how much of a sentence fits before the rest has to become a second
	// caption, so it is tempting to raise when a box runs behind, and raising
	// it buys the time back by breaking the style.
	boxRows int
	held    []string
	// popPending is how many finished captions are queued but not yet shown,
	// and popDwells how long each of them will need once it is. curDwell is the
	// one belonging to the caption on screen now, which is the time the next
	// caption has to wait.
	popPending int
	popDwells  []popTiming
	curDwell   popTiming
	lastBlock  time.Time
	// pairs counts the byte pairs handed out, which is the presentation
	// timeline in the only unit this encoder has. See streamNow.
	streamTime  time.Duration
	blockCopies int
	rows        byte // ccRU2 / ccRU3 / ccRU4
	started     bool
	col         int
	maxCol      int
	// textCol is how wide a row of words may be, which is not how wide the
	// grid is. See ccTextCol.
	textCol int
	upper   bool
	// toldRate is the picture rate the injector read out of the stream, used
	// until this channel has clocked itself.
	toldRate float64
	// minRollGap is the least time between two rolls, from the page's roll
	// speed setting.
	minRollGap time.Duration
	// credit is the pacing allowance, in characters, accrued per picture, and
	// pace the rate it accrues at. maxLag is how much unread text may wait
	// before the meter stands aside.
	credit float64
	pace   float64
	maxLag float64
	// pendingBreak is a carriage return the last phrase finished with and this
	// one has yet to spend. See pushText.
	pendingBreak bool
}

// streamNow is the time on the presentation timeline: what the viewer's clock
// says, not what ours does.
//
// Every dwell in here was measured against time.Now, and that is the wrong
// clock. Pairs do not leave at a steady rate in wall time — they leave when the
// encoder hands over a picture, and a stream arrives in bursts: a socket
// delivers two seconds of video in a moment and then nothing for a moment. So a
// caption told to stay four seconds stayed four seconds of *our* time, which
// was however many seconds of video the burst happened to contain. The same
// setting produced a different result on every stretch of every stream, and a
// player buffering differently saw something different again.
//
// A pair is one picture's worth of line 21 and line 21 runs at a fixed rate, so
// counting pairs is counting presentation time exactly. Nothing here reads a
// real clock any more; the whole encoder runs on this one, and a burst that
// delivers a hundred pairs advances it by a hundred pairs' worth of video
// whether that took a second or an instant.
//
// This is only correct because the channel now runs at the rate the format
// defines. While a sixty picture stream was carrying twice as many pairs, a
// pair was half a picture and this clock would have run at double speed.
func (c *cea608) streamNow() time.Time {
	return time.Unix(0, int64(c.streamTime))
}

// advanceStream moves the presentation clock to where the stream says it is.
//
// Counting pairs and dividing by a rate was right only while the rate was
// fixed, and it is not: a player that transcodes or remuxes does not hand over
// a locked sixty pictures a second, it hands over whatever it produced, and the
// interval wanders. A clock derived from an assumed rate then drifts against
// the video for as long as the stream runs, which is a caption timed correctly
// at the start of a program and wrong by the end of it.
//
// The stream carries the answer. A presentation timestamp is the picture's
// place on the timeline the viewer sees, so the clock is set from that and
// nothing is assumed — variable rate, transcoded, remuxed or locked, the dwells
// are measured against the same timeline the player renders on.
func (c *cea608) advanceStream(d time.Duration) {
	if d <= 0 {
		return
	}
	c.mu.Lock()
	c.streamTime += d
	c.mu.Unlock()
}

// pairRate is how many byte pairs a second this channel carries.
//
// It settles within a second or two of the stream starting and is re-measured
// on a rolling window, so a stream that changes rate mid-flight is followed
// rather than remembered. Before there is enough to go on it answers with the
// format's base rate, which is right for the common case and never far wrong.
func (c *cea608) pairRate() float64 {
	if c.toldRate > 0 {
		return c.toldRate
	}
	return cc608NominalRate
}

// setPictureRate takes the rate the injector derived from the stream's own
// timestamps, for the stretch before this channel has clocked itself. The
// measured rate wins once there is one: what is wanted is the rate pairs are
// actually leaving at, and the picture rate is the best available guess at it
// rather than a substitute for it.
func (c *cea608) setPictureRate(fps float64) {
	if fps <= 0 {
		return
	}
	c.mu.Lock()
	c.toldRate = fps
	c.mu.Unlock()
}

// The drain measurement is gone, and it was measuring the wrong thing.
//
// It counted pairs against the wall clock, because the rate they left at was
// not known and had to be inferred. It is known now: the injector puts one pair
// in every picture the format allows and tells the encoder what that works out
// at, which is authoritative rather than inferred. And the figure it produced
// was the delivery rate, not the presentation rate — a stream arriving in
// bursts measures fast and then slow, and every dwell computed from it moved
// with the network.

// captionLag is how much unread text the meter may hold for this model.
func captionLag(m captionModel, cfg captionConfig) float64 {
	if m.Streaming {
		return ccLagStreaming
	}
	if w := phraseWindowFor(quirksFor(m), cfg); w > 0 {
		return w
	}
	return ccLagFallback
}

func newCEA608(style string, upper bool, onScreen float64, wpm int, maxLag float64) *cea608 {
	rows := byte(ccRU3)
	popon, boxRows := false, 0
	switch style {
	case "rollup2":
		rows = ccRU2
	case "rollup4":
		rows = ccRU4
	case "box2":
		popon, boxRows = true, 2
	}
	n := 3
	switch rows {
	case ccRU2:
		n = 2
	case ccRU4:
		n = 4
	}
	gap := rollGapFor(onScreen, n)
	if popon {
		// A roll divides the wanted time on screen by the rows, because a line
		// survives that many rolls. A box does not divide it: the caption goes
		// up whole and comes down whole, so the whole of it is what the setting
		// asks for.
		//
		// Floored at the guidance for the number of rows it puts up rather than
		// for one, because it puts them up together.
		gap = rollGapFor(onScreen, 1)
		if min := ccMinOnScreen(boxRows); gap < min {
			gap = min
		}
	}
	return &cea608{rows: rows, popon: popon, boxRows: boxRows, maxCol: 32, textCol: ccTextCol, upper: upper,
		pace: paceFor(wpm), maxLag: maxLag, minRollGap: gap}
}

func (c *cea608) ctrl(code byte) {
	// Control codes are sent twice; a decoder that catches the pair twice acts
	// on it once, and the repeat is what survives a dropped frame.
	c.queue = append(c.queue, [2]byte{odd608(ccCtrlCC1), odd608(code)}, [2]byte{odd608(ccCtrlCC1), odd608(code)})
}

// begin puts the decoder into roll-up mode on the bottom row.
//
// Box style has no mode to set here: every caption states its own, and the
// display is cleared instead. Starting is not always starting from nothing —
// the backlog cull above throws away the queue and starts again, and whatever
// the decoder was showing when that happened is still on the screen. A roll
// scrolls it away in its own time; a box has to erase it. Erasing a blank
// display costs a control code and does nothing, which is what it does at a
// genuine stream start.
func (c *cea608) begin() {
	if c.popon {
		c.ctrl(ccEDM)
		c.started = true
		c.col = 0
		return
	}
	c.mode()
	c.ctrl(ccCR)
	c.started = true
	c.col = 0
}

// mode states the roll-up style and the row to write on.
func (c *cea608) mode() {
	c.ctrl(c.rows)
	// Preamble address code for row 15, column 0, white non-italic: 0x14 0x70.
	c.queue = append(c.queue, [2]byte{odd608(ccCtrlCC1), odd608(0x70)}, [2]byte{odd608(ccCtrlCC1), odd608(0x70)})
}

// newRow ends the current row and restates the mode.
//
// A decoder holds the caption style and the row as state, and it only learns
// them from these commands. Sending them once at the start of a stream is
// enough for somebody watching from the start and no use to anybody else: seek
// into a recording, or switch captions on an hour in, and the decoder has
// nothing to go on until the next one arrives. Restating them on every row
// gives a receiver joining at any point somewhere to latch on within a second,
// which is what a broadcast encoder does and why its captions survive a
// channel change.
func (c *cea608) newRow() {
	c.ctrl(ccCR)
	c.mode()
	c.col = 0
}

// ---------------------------------------------------------------------------
// Where a caption breaks
//
// A caption of two rows has to decide two things: which words go in it, and
// where the first row ends. Both were accidents of a greedy wrapper — fill row
// one to thirty-two columns, put the rest on row two, and start a new caption
// whenever that ran out of room. So captions ended mid-clause and rows split
// articles from their nouns, which is the thing every published guide names
// first.
//
// The guidance is unusually consistent about this and it is not house style:
// the ITC guidance the BBC's rules are copied from is inherited regulator
// guidance, and it was written for a thirty-two character line, which is the
// line this code has. It keeps the linguistic rule for live subtitling
// explicitly — "where possible, avoid non-linguistic line breaks (splitting
// verbs etc)" — with "where possible" as the only concession.
//
// None of it needs a parser. The rules are about closed classes of words, which
// is a list, and about punctuation and capitalization, which are already there.
// ---------------------------------------------------------------------------

// wordClass is the part a word plays, as far as a break is concerned. Only the
// closed classes are named; everything else is content and can end a row.
type wordClass int

const (
	clsContent wordClass = iota
	clsTitle
	clsAux
	clsArt
	clsPrep
	clsConj
	clsSubord
	clsPron
	clsNeg
	clsQuant
	clsNumWord
	clsUnit
)

// ccWordClass is every word that is not free to end a row, and what it is.
//
// A word appears once. Several belong to two classes in the grammar — "that" is
// a determiner and a relativizer, "for" is a preposition and a conjunction —
// and the entry here is the one that decides breaks better: a break before
// "that" is usually a clause boundary, a break before "for" usually is not.
var ccWordClass = func() map[string]wordClass {
	m := map[string]wordClass{}
	add := func(c wordClass, words string) {
		for _, w := range strings.Fields(words) {
			if _, seen := m[w]; !seen {
				m[w] = c
			}
		}
	}
	add(clsTitle, `mr mrs ms miss mx dr prof professor rev sen senator rep gov governor
		pres president sir dame lady lord judge justice officer sgt sergeant capt captain
		lt col gen general adm st saint mt mount fr father sister brother coach chief
		mayor king queen prince princess pope agent detective sheriff ambassador
		secretary chairman chairwoman uncle aunt`)
	add(clsAux, `am is are was were be been being have has had having do does did doing
		will would shall should can could may might must ought need dare gonna wanna
		gotta ain't`)
	add(clsNeg, `not never no`)
	add(clsSubord, `that although because if once though unless when whenever whereas
		where wherever whether while why how who whom which as since than until`)
	add(clsArt, `a an the this these those my your his her its our their whose`)
	add(clsConj, `and but or nor so yet plus`)
	add(clsPrep, `of in on at to for with from by about into onto over under above below
		across against along among amid around behind beneath beside besides between
		beyond during except inside near outside past through throughout till toward
		towards underneath unto up upon within without like off out down per via versus
		atop despite regarding concerning after before`)
	add(clsQuant, `some any every each all both either neither another other much many
		few several more most less least half`)
	add(clsNumWord, `one two three four five six seven eight nine ten eleven twelve
		thirteen fourteen fifteen sixteen seventeen eighteen nineteen twenty thirty
		forty fifty sixty seventy eighty ninety hundred thousand million billion
		trillion dozen`)
	add(clsPron, `i you he she it we they there here`)
	add(clsUnit, `percent cent dollars cents pounds euros pence degrees miles feet inches
		yards meters metres kilometers kilometres kilograms kilos grams tons hours
		minutes seconds days weeks months years o'clock am pm mph kph mg kg km lb lbs oz`)
	return m
}()

// ccAbbrev is the words whose full stop ends an abbreviation and not a
// sentence. Without it "Dr. Wilson" reads as two sentences and the break lands
// between the title and the name, which is the one split every guide forbids.
var ccAbbrev = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`mr mrs ms dr prof rev sen rep gov pres st mt inc
		ltd corp co jr sr vs etc no vol fig approx dept univ ave blvd`) {
		m[w] = true
	}
	return m
}()

// ccWord is one word of a caption with everything a break rule asks about it.
type ccWord struct {
	text         string
	class        wordClass
	endsSentence bool
	endsClause   bool
	isCap        bool
	isDigit      bool
}

// ccTrimEdges strips the quotes and brackets around a word, which are not part
// of it for any purpose here.
func ccTrimEdges(w string) string {
	return strings.Trim(w, `"'“”‘’()[]{}`)
}

// analyze608 reads the words a caption is made of.
//
// Given the text before it is put into capitals, and it has to be: two of the
// rules below are about which words are capitalized, and after ToUpper every
// word is. This was the easiest thing in the whole rule set to get quietly
// wrong — the tests would pass, the rules would simply never fire.
func analyze608(words []string) []ccWord {
	out := make([]ccWord, 0, len(words))
	startOfSentence := true
	for _, raw := range words {
		// Trimmed for the rules and never for the text: a word is shown as it
		// was recognized, quotation marks and all. Classifying the trimmed form
		// and then displaying it would take the quotes off the caption.
		w := ccTrimEdges(raw)
		bare := strings.TrimRight(w, `.!?…,;:—–`)
		lower := strings.ToLower(strings.TrimRight(bare, "."))
		a := ccWord{text: raw, class: ccWordClass[lower]}
		if last := strings.TrimRight(w, `"'“”’)`); last != "" {
			switch last[len(last)-1] {
			case '.', '!', '?':
				a.endsSentence = !ccAbbrev[lower]
			case ',', ';', ':':
				a.endsClause = true
			}
			if strings.HasSuffix(last, "—") || strings.HasSuffix(last, "–") {
				a.endsClause = true
			}
		}
		for _, r := range bare {
			if r >= '0' && r <= '9' {
				a.isDigit = true
				break
			}
		}
		if r := []rune(bare); len(r) > 0 && unicode.IsUpper(r[0]) && !startOfSentence {
			a.isCap = true
		}
		startOfSentence = a.endsSentence
		out = append(out, a)
	}
	return out
}

// ccStranded is the classes that must not be left at the end of a row. It is
// the union of the article, preposition, conjunction, auxiliary and title
// rules, which is the one rule every guide states in some form — and the only
// one that still works when the model is producing no punctuation at all.
func ccStranded(c wordClass) bool {
	switch c {
	case clsArt, clsPrep, clsConj, clsSubord, clsAux, clsNeg, clsQuant, clsTitle:
		return true
	}
	return false
}

// ccSplitsName reports that a break would cut something that is one thing: a
// person's name, a title from its name, a number from its unit, or a
// hyphenated compound.
func ccSplitsName(l, r ccWord) bool {
	switch {
	case l.isCap && r.isCap && !l.endsSentence:
		return true
	case l.class == clsTitle && r.isCap:
		return true
	case l.isDigit && (r.isDigit || r.class == clsUnit || r.class == clsNumWord):
		return true
	case l.class == clsNumWord && (r.class == clsNumWord || r.class == clsUnit || r.isDigit):
		return true
	case strings.HasSuffix(l.text, "-"):
		return true
	}
	return false
}

// breakTier scores a break between two words. Lower is better, and the order
// the tests run in is the order the guidance ranks them.
//
// Its own function with its own test because it is the rule, and a rule buried
// in a loop is one nobody can check. The tiers, in the words of the guides
// they come from: break at punctuation; never after a conjunction; never inside
// a prepositional phrase; never between an article and its noun; a break
// between two content words is fine; a pronoun may end a row if nothing better
// offers; and last, the break that strands a function word, which is what every
// guide names first and this code used to do at random.
func breakTier(l, r ccWord) int {
	switch {
	case l.endsSentence || l.endsClause:
		return 1
	case r.class == clsConj || r.class == clsSubord:
		return 2
	case r.class == clsPrep:
		return 3
	case r.class == clsArt || r.class == clsQuant:
		return 4
	case ccSplitsName(l, r):
		// Ahead of the two tiers below on purpose: both halves of a name are
		// content words, so a name split would otherwise score as a good break.
		return 7
	case l.class == clsContent && r.class == clsContent:
		return 5
	case l.class == clsPron:
		return 6
	case ccStranded(l.class):
		return 8
	}
	return 5
}

// ccWordsWidth is how many columns a run of words occupies, spaces included.
func ccWordsWidth(w []ccWord) int {
	n := 0
	for i, x := range w {
		if i > 0 {
			n++
		}
		n += len([]rune(x.text))
	}
	return n
}

// breakLine608 chooses where the first row of a caption ends, returning the
// index the second row starts at. Zero means the words do not fit on two rows,
// which is a question about how much goes in the caption and not about where it
// breaks.
func breakLine608(w []ccWord, cols int) int {
	if len(w) < 2 {
		return 0
	}
	type cand struct{ at, tier, skew int }
	var all []cand
	for i := 1; i < len(w); i++ {
		top, bottom := ccWordsWidth(w[:i]), ccWordsWidth(w[i:])
		if top > cols || bottom > cols {
			continue
		}
		skew := top - bottom
		if skew < 0 {
			skew = -skew
		}
		all = append(all, cand{i, breakTier(w[i-1], w[i]), skew})
	}
	if len(all) == 0 {
		return 0
	}
	// One or two words alone above a full row reads as a stray, so those are
	// set aside — unless they are all there is, because failing to break is
	// not one of the outcomes.
	kept := all[:0:0]
	for _, c := range all {
		if c.at <= 2 && len(w)-c.at > 4 {
			continue
		}
		kept = append(kept, c)
	}
	if len(kept) == 0 {
		kept = all
	}
	best := kept[0]
	for _, c := range kept[1:] {
		// Shape decides between equals and never overrules the rule: the
		// evidence has viewers choosing on where the sentence divides rather
		// than on how the caption looks. A tie goes to the fuller first row.
		if c.tier < best.tier || (c.tier == best.tier && c.skew <= best.skew) {
			best = c
		}
	}
	return best.at
}

// fits608 is how many of these words will go on rows rows of cols columns.
func fits608(w []ccWord, cols, rows int) int {
	n, used, line := 0, 0, 1
	for i, x := range w {
		width := len([]rune(x.text))
		next := used + width
		if used > 0 {
			next++
		}
		if next > cols {
			if line == rows {
				return n
			}
			line++
			used = width
			if width > cols {
				return maxInt(n, i+1)
			}
		} else {
			used = next
		}
		n = i + 1
	}
	return n
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// segment608 divides a phrase into captions before any of it is broken into
// rows, which is the order the guidance puts them in and the opposite of what
// this code did. A caption used to end wherever the wrapper ran out of room —
// so a sentence boundary landing one word later was simply missed, and the
// caption ended mid-clause instead.
//
// Nothing is dropped: a word too wide for a row still goes, on a row of its
// own, because a caption with a word missing is worse than a caption that looks
// wrong.
func segment608(w []ccWord, cols, rows int) [][]ccWord {
	var out [][]ccWord
	for len(w) > 0 {
		n := fits608(w, cols, rows)
		if n <= 0 {
			n = 1
		}
		cut := n
		// A caption ends where a sentence ends, whether or not the next one
		// would have fitted beside it. Two sentences sharing a caption is not
		// a caption of two sentences — it is the first one waiting for the
		// second to be spoken before either can be read, which is delay bought
		// for nothing. The search covers the whole caption because a full stop
		// is the strongest boundary there is.
		//
		// Except where it would leave a caption too small to keep up. "Yes." is
		// a sentence and it is not a caption: a box put it up on its own, alone
		// in the middle of the screen, and held the screen a whole second to do
		// it — which is a second the words behind it did not have. Half a row is
		// the same break-even minCaptionChars works out from the reading speed,
		// at the speed the page defaults to. Short sentences ride together the
		// way they do on a broadcast caption.
		for i := n; i >= 1; i-- {
			if w[i-1].endsSentence && i < len(w) && ccWordsWidth(w[:i]) >= cols/2 {
				cut = i
				break
			}
		}
		// A comma is a weaker cue and only worth taking near the back, where it
		// saves a mid-clause ending. Taken early it trades that for a caption
		// with a row nearly empty and gains nothing.
		if cut == n && n < len(w) {
			floor := n * 2 / 3
			if floor < 1 {
				floor = 1
			}
			for i := n; i >= floor; i-- {
				if w[i-1].endsClause {
					cut = i
					break
				}
			}
		}
		out = append(out, w[:cut])
		w = w[cut:]
	}
	return out
}

// rows608 is the finished rows of one caption.
func rows608(w []ccWord, cols int) []string {
	text := func(part []ccWord) string {
		parts := make([]string, len(part))
		for i, x := range part {
			parts[i] = x.text
		}
		return strings.Join(parts, " ")
	}
	if len(w) == 0 {
		return nil
	}
	if ccWordsWidth(w) <= cols {
		return []string{text(w)}
	}
	if at := breakLine608(w, cols); at > 0 {
		return []string{text(w[:at]), text(w[at:])}
	}
	// Wider than two rows, which segment608 should have prevented. Fall back to
	// filling rather than losing the words.
	var out []string
	cur := []ccWord{}
	for _, x := range w {
		if len(cur) > 0 && ccWordsWidth(append(append([]ccWord{}, cur...), x)) > cols {
			out = append(out, text(cur))
			cur = cur[:0]
		}
		cur = append(cur, x)
	}
	if len(cur) > 0 {
		out = append(out, text(cur))
	}
	return out
}

// ccTextCol is the widest row of words this writes, against a grid that is 32
// columns wide.
//
// The grid is the standard's and does not change. What changes is how much of
// it is filled, and filling it to the edge is what nobody else does: a
// broadcast captioner leaves the last few columns alone, which is part of why
// over-the-air captions look right and these did not.
//
// It matters because not every player renders on the grid. A television's
// decoder sizes its font so that 32 columns fit, by definition — the grid is
// the layout. A browser does not: the Channels web player hands each row to
// video.js as its own subtitle cue, and video.js picks a font size from the
// height of the video with no knowledge of columns at all. A row written to
// the full 32 then does not fit the width it is given, and the last word wraps
// onto a line of its own — two rows of captions arriving as three lines of
// text, the third holding one word.
//
// Six columns, because that is about a word and a space, and one word is what
// was seen to overflow. Observed rather than derived: nothing here can measure
// a font it does not choose, so if a row still wraps this is the number to
// lower, and lowering it costs only that captions are cut into more of them.
const ccTextCol = 26

// wrap608 lays words out into caption rows of at most maxCol characters.
//
// A separate function from the roll-up's wrapping because the two need
// different things at different times. A roll-up wraps as it writes, because it
// is writing to the screen and the screen is what it is. A box has to know the
// shape of the whole caption before it sends any of it: the rows are addressed
// by number, so which row a word lands on has to be decided first.
//
// A word longer than a row is broken rather than dropped. Nothing in English
// runs to thirty-two characters, but a caption is not always English and a
// silently missing word is worse than an ugly one.
func wrap608(words []string, maxCol int) []string {
	var lines []string
	cur := ""
	for _, w := range words {
		for len(w) > maxCol {
			if cur != "" {
				lines = append(lines, cur)
				cur = ""
			}
			lines = append(lines, w[:maxCol])
			w = w[maxCol:]
		}
		switch {
		case cur == "":
			cur = w
		case len(cur)+1+len(w) <= maxCol:
			cur += " " + w
		default:
			lines = append(lines, cur)
			cur = w
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

// popDwell is how long one box caption has to stay up before the next may take
// its place.
//
// A roll-up asks this question once and answers it with a constant, because
// every roll moves the same one row. A box replaces everything on the screen at
// a stroke, and how long that needs depends entirely on how much is on it: two
// full rows are four seconds of reading and "Yes." is not.
//
// Setting one number for both is what made this style lag. Time on screen was
// taken as the minimum for every caption, so a two word answer held the screen
// as long as a full sentence, and everything said while it sat there queued up
// behind it. At four seconds that is a throughput ceiling as well: fifteen
// captions a minute, against speech that produces rather more.
//
// So it is measured, the way subtitle timing has always been measured — the
// characters divided by the reading speed. The page's time on screen becomes
// the ceiling rather than the value, which is what somebody choosing a longer
// one is really asking for. The floor is the published guidance for that many
// rows and it is applied last, because a caption gone before it could be read
// is the one failure worth being slow to avoid.
func popDwell(chars, rows int, pace float64, want time.Duration) popTiming {
	d := want
	if pace > 0 {
		d = time.Duration(float64(chars) / pace * float64(time.Second))
	}
	if d > want {
		d = want
	}
	// The floor is how long the caption needs at the fastest anyone should ever
	// be asked to read, not merely the shortest a caption may legally be. Those
	// are different numbers and only the second was here: two full rows have a
	// guidance minimum of a second and a half, which is four hundred words a
	// minute, and draining a backlog down to that puts captions on the screen
	// nobody can read. Whichever is longer wins.
	floor := ccMinOnScreen(rows)
	if pace > 0 {
		if fastest := time.Duration(float64(chars) / ccMaxPace * float64(time.Second)); fastest > floor {
			floor = fastest
		}
	}
	if d < floor {
		d = floor
	}
	return popTiming{want: d, floor: floor}
}

// popTiming is how long one caption asks for and how long it must have.
//
// The two are different numbers because falling behind is answered by giving
// captions less time, and there is a point past which less time is no time.
// A roll-up can be hurried down to nothing safely: the line it rolls away is
// still on the screen, one row up, for another whole cycle. A box has no such
// slack — hurrying a caption means replacing it, and a caption replaced before
// anybody could read it was never shown at all.
//
// So the backlog is drained down to the floor and no further. Being late is
// recoverable and being unreadable is not.
type popTiming struct {
	want  time.Duration
	floor time.Duration
	// last marks the final page of a phrase. A sentence too long for two rows
	// becomes several captions, and those are not a backlog — they are one
	// thing being said. Counting them as one is what stops a sentence from
	// hurrying itself.
	last bool
}

// The preamble address code table, from 47 CFR 15.119.
//
// A row is named by both bytes and by neither alone. The first byte picks a
// pair of rows — and the pairing is not in row order, which is why this is a
// table and not arithmetic. The second byte picks which of the pair, 0x50 for
// the lower and 0x70 for the upper, and carries the indent above that: each
// step of four columns adds two.
//
// Row 11 is the odd one out and it is not a mistake in the transcription. It
// exists only as 0x10 with a 0x5x second byte; there is no 0x10 0x7x preamble
// at all. A decoder handed one drops it and the text that follows lands
// wherever the cursor happened to be.
//
// Every indent code is white by the standard's own note — a preamble gives
// color or indent and never both — so this table needs no color dimension.
var (
	ccPACRow  = [15]byte{0x11, 0x11, 0x12, 0x12, 0x15, 0x15, 0x16, 0x16, 0x17, 0x17, 0x10, 0x13, 0x13, ccCtrlCC1, ccCtrlCC1}
	ccPACBase = [15]byte{0x50, 0x70, 0x50, 0x70, 0x50, 0x70, 0x50, 0x70, 0x50, 0x70, 0x50, 0x50, 0x70, 0x50, 0x70}
)

// ccTabCC1 is the first byte of a tab offset, and it is not the byte every
// other control code here starts with.
//
// 0x14 0x21 is backspace. A tab offset emitted through the usual control path
// would delete the character to the left of the cursor instead of moving it,
// which is why this has its own emitter rather than another call to ctrl.
const ccTabCC1 = 0x17

// pac addresses a row and an indent, so the next characters land there.
//
// Sent twice, like every other control pair: a decoder that catches the repeat
// acts once, and the repeat is what survives a dropped frame.
func (c *cea608) pac(row, indent int) {
	if row < 1 || row > 15 {
		row = 15
	}
	if indent < 0 {
		indent = 0
	}
	if indent > 28 {
		indent = 28
	}
	pair := [2]byte{odd608(ccPACRow[row-1]), odd608(ccPACBase[row-1] + byte(indent/4*2))}
	c.queue = append(c.queue, pair, pair)
}

// tab moves the cursor one, two or three columns right of where the preamble
// left it, which is how a caption reaches a column that is not a multiple of
// four. A preamble alone cannot: its indents are 0, 4, 8 and so on.
//
// The three offsets are three distinct codes rather than one code sent n times,
// and that is what makes them safe to double. A decoder is required to ignore a
// control pair that repeats the one before it, so a column padded by sending
// the same code twice arrives moved once. Sending TO2 twice moves two columns,
// as intended, because the repeat is a repeat and not a second instruction.
func (c *cea608) tab(n int) {
	if n < 1 || n > 3 {
		return
	}
	pair := [2]byte{odd608(ccTabCC1), odd608(byte(0x20 + n))}
	c.queue = append(c.queue, pair, pair)
}

// centerStart is where a caption starts on the 32 column grid, as a preamble
// indent and a tab offset after it.
//
// 608 has no notion of alignment. There is no center attribute and nothing in
// the standard says where a caption belongs across the screen: a row and a
// column is all a caption has, and centering is arithmetic the encoder does
// before it sends anything. Flush at column zero is what comes out of not
// doing it, which is what this code was doing.
//
// Measured rather than assumed: across 747 pop-on captions in two published
// caption files, every one of them starts at (32 - longest line) / 2, and every
// row of a caption starts at the same column as every other row. Both without
// exception. So the block is centered on its longest line and the shorter rows
// hang off it — the lines are not centered one by one, which fits only about
// half of the real rows.
func centerStart(lines []string, cols int) (indent, tab int) {
	longest := 0
	for _, l := range lines {
		if n := len([]rune(l)); n > longest {
			longest = n
		}
	}
	start := (cols - longest) / 2
	if start < 0 {
		start = 0
	}
	return start / 4 * 4, start % 4
}

// showPopon writes one caption where it cannot be seen and then shows it whole.
//
// This is the difference between the two styles and it is the whole of it. A
// roll-up writes to the screen, so the viewer watches the words arrive one at a
// time and watches the line above scroll away. A box writes to the decoder's
// other memory, which is not on screen, and then swaps the two: the caption
// appears finished, all of it at once, and stays until the next swap replaces
// it. It is what broadcast captioning does on anything not being typed live.
//
// Four commands say it. RCL puts the decoder in this mode and points writing at
// the memory nobody can see; ENM clears that memory first, so a caption cannot
// inherit the end of an older one; the rows are addressed and written; EOC
// swaps. Nothing is visible between the first three and the fourth, which is
// the point — and it is why the pacing meter has no business here, because
// there is nothing to pace when there is nothing to watch. What the meter does
// for a roll-up, the dwell on EOC does for this: it decides how long a finished
// caption stays up, which is the only timing a viewer of this style can see.
func (c *cea608) showPopon(lines []string) {
	if len(lines) == 0 {
		return
	}
	// Deferred, never dropped.
	//
	// This said lines = lines[:c.boxRows], which is a silent truncation of
	// speech that was recognized — the caller pages and so it never fired, and
	// a guard that discards words the moment the caller stops being careful is
	// the shape of fault this file has a rule about. The remainder goes up as
	// the caption after this one instead.
	var rest []string
	if len(lines) > c.boxRows {
		rest, lines = lines[c.boxRows:], lines[:c.boxRows]
	}
	c.ctrl(ccRCL)
	c.ctrl(ccENM)
	// Centered as a block, on the longest of its rows, and every row starts at
	// the same column. See centerStart.
	indent, tab := centerStart(lines, c.maxCol)
	// A box sits on the bottom of the screen and grows upward from row 15, so
	// however many rows it has, the last of them is the last line.
	row := 16 - len(lines)
	for _, line := range lines {
		c.pac(row, indent)
		// After the preamble and before any text: a preamble sets the column
		// back to its own indent, so a tab sent before one is discarded, and a
		// character sent between them eats the column the tab was skipping.
		c.tab(tab)
		c.col = 0
		for _, r := range line {
			c.writeRune(r)
		}
		row++
	}
	c.ctrl(ccEOC)
	// Sending a caption is what starting means here, and it has to be said on
	// this path as well as in begin.
	//
	// The display pulls held text straight out of next, which never went
	// through begin — so after a caption had been taken down, started stayed
	// false while a new caption was on screen, and the next phrase to arrive
	// called begin and erased it. A caption shown and then wiped by the arrival
	// of the words meant to follow it: the screen going blank on a pause and
	// staying blank until the phrase after next.
	c.started = true
	defer func() {
		if len(rest) > 0 {
			c.showPopon(rest)
		}
	}()
	chars := 0
	for _, line := range lines {
		chars += len([]rune(line))
	}
	c.popDwells = append(c.popDwells, popDwell(chars, len(lines), c.pace, c.minRollGap))
	c.col = 0
}

// ccPopLinger is how long a box caption may stay past its own reading time when
// nothing has arrived to replace it.
//
// Nothing rules on this number. It was one second, which is shorter than the
// pauses people leave in ordinary speech: a speaker drawing breath between two
// sentences blanked the screen, and it stayed blank until the next phrase had
// been spoken, cut, and transcribed — several seconds of nothing for a gap of
// one. The caption was not stale; the talking had not stopped.
//
// Three seconds clears the pauses inside speech and still takes the caption
// down when the speech itself has ended. A caption left up through a genuine
// silence is a sentence from a while ago presented as though it were current,
// which is what this exists to prevent, and the twenty second staleness sweep
// remains behind it for anything this misses.
const ccPopLinger = 3 * time.Second

// maxInFlight is how many phrases one stream may have at the shared recognizer
// at once.
//
// It was one, which throttled a stream to a phrase per service cycle while the
// recognizer had capacity to spare. Then it was two, which was the same fault
// with a bigger number: a stream still stopped listening for its own audio
// while two phrases were out, however idle the thing it was waiting on.
//
// A phrase in flight is being worked on. A phrase held back is not — it is
// waiting for permission, and the recognizer that would have taken it is what
// it is waiting for. So this is the number that decides whether the batch call
// has anything to batch, and with a stream held to two the log showed 1.2
// phrases per dispatch: a batch entry point built to amortize the decode across
// many utterances, handed one at a time.
//
// Four, which lets a stream keep its buffered phrases moving rather than
// queued, and gives seven captioned tuners enough outstanding work to fill a
// dispatch. It costs nothing when the recognizer is busy, because then the
// window stays full either way and the bound that matters is the recognizer's.
const maxInFlight = 4

// ccMaxBacklogSec is the most unshown caption data we will hold, measured as
// the time it would take to air. Reaching it means recognition has outrun the
// display, and what is held past this point can only ever be shown late.
//
// In seconds rather than byte pairs for the same reason as the roll pressure:
// the pair count that used to stand here was a hundred and fifty, annotated as
// five seconds at thirty pictures a second, which made it two and a half on a
// sixty-picture stream. The ceiling moved with the format instead of staying
// where it was meant to be.
const ccMaxBacklogSec = 5.0

// push queues a complete phrase and closes the line after it.
func (c *cea608) push(text string) { c.pushText(text, true) }

// pushText queues text, wrapping it to the 32 column caption grid, and ends the
// line only if breakAfter is set.
//
// A streaming model finalizes a few words at a time, and each of those is a
// continuation of the sentence being spoken rather than a line of its own, so
// the line is closed on the end of an utterance instead of on every arrival.
func (c *cea608) pushText(text string, breakAfter bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	text = cc608ExpandText(text)
	// A box keeps the capitals it was given until the rows are written.
	//
	// Two of the line break rules ask which words are capitalized — a name is
	// not split from the rest of it, a title is not split from its name — and
	// after ToUpper every word is capitalized, so both rules answer yes to
	// everything and neither does anything. This was the easiest thing in the
	// whole rule set to get quietly wrong: it compiles, it passes, and the
	// rules simply never fire.
	cased := text
	if c.upper {
		text = strings.ToUpper(text)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Captions have to track what is being said now. If a burst of recognition
	// has queued more than the channel can carry, showing all of it would put
	// the text permanently behind the picture, so drop what has not aired and
	// start the roll-up again on the current phrase.
	if float64(len(c.queue)) > ccMaxBacklogSec*c.pairRate() {
		c.queue = c.queue[:0]
		c.started = false
		c.col = 0
		c.popPending, c.popDwells = 0, c.popDwells[:0]
	}
	if !c.started {
		c.begin()
	}
	if c.popon {
		// Held only until there is a caption to show, and not a word longer.
		//
		// This waited for breakAfter, and breakAfter does not mean what it
		// needs to mean here. It is the roll-up's flag for "end the line", and
		// the phrase path sets it only where the speaker actually paused or the
		// model closed a sentence off — a fragment cut at a word gap flows on
		// instead, which is right for a roll-up and is what fills its rows.
		//
		// A box read that as "the caption is not finished" and held everything.
		// On continuous speech, which is most of television, the pause never
		// comes: the words sat until a second caption's worth had piled up
		// behind them, so every caption reached the screen around eight seconds
		// of speech after it started. That is the whole of the delay this style
		// had over the roll-up, and none of it was the style.
		//
		// A caption is complete when it is full, or when a sentence ends inside
		// it, and neither of those needs the speaker to stop.
		c.lastText = c.streamNow()
		c.held = append(c.held, strings.Fields(cased)...)
		c.flushPopon(breakAfter)
		return
	}
	// The break owed by the previous phrase is taken now, with this phrase's
	// words behind it, and not when that phrase ended.
	//
	// Rolling at the end of a phrase rolls to a blank row. The line just read
	// moves up — off the screen entirely at two rows — and the viewer is left
	// looking at nothing for as long as it takes the speaker to say the next
	// thing, which is most of three seconds. That is the hold: not the pace of
	// the roll, but a roll performed before there was anything to roll to.
	// A broadcast encoder rolls when the new line arrives, because the roll is
	// how the new line gets its row, and that is all this is.
	if c.pendingBreak {
		c.pendingBreak = false
		c.newRow()
	}
	c.lastText = c.streamNow()
	for _, w := range strings.Fields(text) {
		runes := []rune(w)
		if c.col > 0 && c.col+1+len(runes) > c.maxCol {
			c.newRow()
		}
		if c.col > 0 {
			c.writeRune(' ')
		}
		for _, r := range runes {
			if c.col >= c.maxCol {
				c.newRow()
			}
			c.writeRune(r)
		}
	}
	if breakAfter {
		// Owed, not taken: the next phrase collects it on the way in.
		c.pendingBreak = true
	}
}

// flushPopon sends everything held as one caption, or as several when it does
// not fit on two rows.
//
// A sentence longer than sixty-four characters becomes two captions shown one
// after the other, which is how a broadcast encoder handles the same thing.
// Each of them is a caption in its own right and waits out its own time on
// screen, because a page nobody could read is not a page that was shown.
//
// Called with the lock held.
func (c *cea608) flushPopon(final bool) {
	held := analyze608(c.held)
	if len(held) == 0 {
		return
	}
	// Divided into captions first and into rows second, which is the order the
	// guidance puts them in. The other way round — rows filled greedily, then
	// cut into captions every two rows — makes where a caption ends a side
	// effect of where the wrapper ran out of room.
	caps := segment608(held, c.textCol, c.boxRows)
	// The last of them may still be growing, so it is only shown when it is
	// finished: when the speaker has stopped, when it fills the caption, or
	// when a sentence ends in it. Otherwise it waits for the next few words.
	// Everything before it is finished by definition — something came after.
	shown := len(caps)
	if !final && shown > 0 {
		last := caps[shown-1]
		full := len(rows608(last, c.textCol)) >= c.boxRows
		if !full && !last[len(last)-1].endsSentence {
			shown--
		}
	}
	if shown <= 0 {
		return
	}
	var kept []string
	for _, w := range caps[shown:] {
		for _, x := range w {
			kept = append(kept, x.text)
		}
	}
	c.held = append(c.held[:0], kept...)
	var captions int
	for _, caption := range caps[:shown] {
		rows := rows608(caption, c.textCol)
		if c.upper {
			for i := range rows {
				rows[i] = strings.ToUpper(rows[i])
			}
		}
		c.showPopon(rows)
		captions++
	}
	if captions == 0 {
		return
	}
	// One phrase, one thing waiting — not one per page.
	//
	// The pages of a long sentence are queued together, so counting pages made
	// every long sentence look like a backlog to itself: the moment the second
	// page was queued the first was declared late and cut to its floor. Half a
	// sentence flashing past while the rest of it waited is not a display
	// catching up, it is a display rushing something nothing was behind.
	c.popDwells[len(c.popDwells)-1].last = true
	c.popPending++
}

// writeRune appends a character, pairing it with the previous one where it can.
// 608 always moves two bytes at a time, so a lone character is padded.
func (c *cea608) writeRune(r rune) {
	b := cc608Char(r)
	if n := len(c.queue); n > 0 && c.queue[n-1][1] == odd608(cc608Null) && c.queue[n-1][0] != odd608(ccCtrlCC1) {
		c.queue[n-1][1] = odd608(b)
	} else {
		c.queue = append(c.queue, [2]byte{odd608(b), odd608(cc608Null)})
	}
	c.col++
}

// clear wipes the display, used when the stream goes quiet for a while.
// ccStaleAfter is how long a caption stays on screen with nothing behind it.
//
// Long enough to sit through a musical interlude or a quiet scene without the
// text flickering away, short enough that a viewer is never reading a sentence
// that stopped being true minutes ago. It also means a failure upstream now
// looks like a failure — a blank line — instead of looking like a caption.
const ccStaleAfter = 20 * time.Second

// The roll speeds offered on the page: the least time between two rolls of the
// display.
//
// The text itself is paced by the channel — so many characters a second, no
// faster — but a carriage return is only two byte pairs, so a burst of
// recognition can roll the display several times in well under a second and a
// line leaves the screen before anyone has read it. This floor keeps a finished
// line put for a beat, which is what broadcast roll-up looks like.
//
// How long a beat should be is a matter of taste and eyesight, and of how many
// rows are up: at two rows a roll is the only thing standing between a line and
// the edge of the screen, while at four it has three more rows to travel. So it
// is a setting rather than a number, and the default is the broadcast one it
// has always been.
const ()

// waiting reports whether anything is queued behind the carriage return at the
// head — that is, whether holding the roll is holding words back.
//
// This is the whole of the rule the pacing needs, and every threshold tried
// here was an approximation of it. A count of pairs was a number about one
// model on one stream, and half a second of channel time was a number about
// one taste; both asked "is a lot waiting", when the question is "is anything
// waiting". A finished line may sit and be read when there is nothing to say.
// The moment there are words recognized and not yet on screen, every
// millisecond of dwell is a millisecond they are late by, and no amount of
// composure is worth that — least of all in the middle of a sentence, where a
// wrapped line holds back the end of the phrase it belongs to.
//
// Two, because a control code occupies the queue twice: the pair and the copy
// a decoder is only guaranteed to recognize when it arrives back to back.
// ccCharsPerPair is what a queued pair is worth in reading time. writeRune
// packs two characters into one wherever it can, and measurement across real
// captions puts the average at 1.97, so two is the figure to reason with.
const ccCharsPerPair = 2.0

// ccMaxLagSec is how much unread text may wait before the meter stands aside.
//
// The meter smooths a phrase onto the screen instead of dumping it, which costs
// nothing in the long run: the words arrive at the speed they were spoken, so a
// pace set to reading speed matches them over any window longer than a phrase.
// What it cannot do is run slower than the speaker indefinitely. If it did, the
// queue would grow without bound and the captions would fall further behind the
// picture every minute.
//
// So the meter yields once more than this much reading time is waiting. Six
// seconds is a phrase and a half at the longest sentence length offered: enough
// to smooth one phrase completely, and short enough that two piling up drains
// at the channel's own speed rather than settling into a permanent lag.
//
// It was five seconds of *channel* time, which sounds similar and is not: the
// channel carries sixty characters a second and nobody reads at a quarter of
// that, so five seconds of channel time is twenty seconds of reading. A backlog
// could sit just under that threshold for ever — captions a quarter of a minute
// late, with nothing in the design to drain them.
func (c *cea608) waiting() bool {
	// A box is behind when there is a caption waiting to be shown, and that is
	// the only reading of it that means anything here.
	//
	// It asked for more than one, which meant it was never behind. Captions and
	// speech arrive at about the same rate, so the queue holds one and the test
	// never fired — and a box that is never behind never catches up. Whatever
	// lag it started the program with, it kept: the cadence looked right and
	// the whole stream sat a few seconds late for ever, which is the difference
	// between running slow and running offset.
	//
	// It is the caption's floor that keeps this safe rather than the count.
	// Being behind shortens what is on screen to the time it takes to read at
	// the fastest speed anyone should be asked to read at, and never below, so
	// draining costs comfort and never legibility.
	//
	// The character count below is no use here either: it asks how much reading
	// time is queued, which assumes the queue is text on its way to the screen.
	// A box's queue is a whole caption that then appears at once, so one
	// ordinary two row caption measures as five seconds of reading.
	if c.popon {
		// Words held are words waiting, exactly as much as a caption already
		// queued is. Only the queued ones counted, and the display makes a
		// caption out of held words only once the screen is free — so the
		// common case had nothing queued, the caption on screen was never
		// declared late, and it kept its full comfortable reading time while
		// the next sentence sat behind it. The drain existed and never ran.
		return c.popPending > 0 || len(c.held) > 0
	}
	if c.maxLag <= 0 {
		return len(c.queue) > 2
	}
	if c.pace <= 0 {
		return len(c.queue) > 2
	}
	return float64(len(c.queue))*ccCharsPerPair/c.pace > c.maxLag
}

// The tolerance is the shape of the model, not a constant.
//
// A phrase model hands over a whole sentence at once, so the meter must be
// allowed to hold roughly one phrase in order to spread it — that is the entire
// job. A streaming model hands over words as they are spoken, so nothing needs
// spreading and every queued character is pure lag on a model chosen precisely
// for not having any. Giving both the same six seconds put Nemotron six seconds
// behind the picture to smooth a burst it never produces.
// Zero turns the meter off, which is what a streaming model wants.
//
// The meter exists to spread a burst. A phrase model hands over a whole
// sentence at once and the words have to be let onto the screen at reading
// speed or they land in a heap; that is the entire job. A streaming model hands
// over words as it hears them, already at the speed they were spoken, so there
// is nothing to spread and every character the meter holds is delay added to a
// model chosen for not having any.
//
// The dwell still applies either way: a finished line rests before it rolls
// whichever model wrote it.
const (
	ccLagStreaming = 0
	ccLagFallback  = 4.0
)

// cc608NominalRate is the pair rate assumed until the channel has been running
// long enough to measure its own. Field 1 of CEA-608 carries one pair per
// picture at the base rate of the format.
const cc608NominalRate = 29.97

// next returns the pair of bytes to attach to the next video frame.
// cc608Pace is how fast characters are let onto the screen, per second.
//
// The caption channel carries one character pair per picture — sixty characters
// a second on a sixty hertz stream, four times the rate anybody speaks. So a
// phrase emptied
// onto the display in a quarter of the time it took to say, the display then sat
// idle until the next phrase arrived, and the carriage return dwell added
// another pause on top of that. Text flew, then stopped, then flew. It is the
// channel's rate rather than the model's, which is why it looked the same
// whichever model was running.
//
// Fifteen is where the published guidance overlaps. The BBC puts subtitle
// speed at 160 to 180 words a minute, the DCMP's captioning key at 130 to 160,
// and the industry figure for characters a second is 20 at the most with 12 to
// 18 comfortable. Fifteen characters a second is about 155 words a minute, which
// is inside both word ranges and in the middle of the character one.
//
//	https://www.clevercast.com/bbc-subtitling-guidelines/
//	https://dcmp.org/learn/601-captioning-key---presentation-rate
//
// The reason it works is simpler than the standards: it is how fast people
// speak, which is the rate the words arrive at. Pacing the display to it makes
// the screen fill at the speed the words were said, which is what makes real
// roll-up readable — the dwell was never doing that job.
//
// It cannot fall behind on its own: the words arrive at speaking speed, so a
// pace set to speaking speed matches them over any window longer than one
// phrase. What it cannot absorb is a genuine backlog, and that is what the
// catch-up below is for.
// ccMinOnScreen is how long a line must be readable before it leaves, from the
// captioning guidance: a minimum of one second for a single line, one and a half
// for two, two for three. A roll-up does not put lines up together — each row is
// added and the oldest scrolls away — so what a row gets is the gap between
// rolls multiplied by the number of rows above it.
//
// So the floor is on the product rather than on the gap, and it is a floor and
// not a setting. Roll speed is taste; a line leaving before it can be read is
// not a taste, it is a caption nobody got to have.
func ccMinOnScreen(rows int) time.Duration {
	switch {
	case rows <= 1:
		return time.Second
	case rows == 2:
		return 1500 * time.Millisecond
	default:
		return 2 * time.Second
	}
}

// captionOnScreen is what the page offers for the least time a line stays
// readable, in seconds. The guidance minimum for three rows is two; the rest is
// room for anybody who wants longer.
var captionOnScreen = []float64{2, 3, 4, 5, 6, 8}

// rollGapFor turns a wanted time on screen into the gap between rolls.
//
// A roll-up does not put lines up together — each row is added and the oldest
// scrolls away — so a row is readable for the gap between rolls multiplied by
// the number of rows above it. Asking for six seconds at three rows is a two
// second gap.
//
// Floored at the guidance whatever is asked for: a minimum of one second for a
// single line, one and a half for two, two for three. Time on screen is taste
// above that floor and not below it, because a line leaving before it can be
// read is not a preference, it is a caption nobody got to have.
func rollGapFor(want float64, rows int) time.Duration {
	if rows < 1 {
		rows = 1
	}
	gap := time.Duration(want * float64(time.Second) / float64(rows))
	if min := ccMinOnScreen(rows) / time.Duration(rows); gap < min {
		gap = min
	}
	return gap
}

const cc608Pace = 15.0

// captionSpeeds is what the page offers, in words a minute. The published
// guidance puts subtitle speed between 120 and 160 depending on audience and
// medium — the BBC at the top of it, the DCMP lower — so the range is offered
// whole rather than a number chosen out of it on somebody's behalf.
var captionSpeeds = []int{120, 130, 140, 150, 160}

// ccCharsPerWord converts words a minute into characters a second.
//
// Derived rather than published, and the derivation matters because there are
// two conventions and they differ by sixteen percent. The five character word
// is the typing standard; captioning counts real words — the DCMP is explicit
// that "each word is counted, as opposed to basing the calculation on the
// number of characters". English prose averages 4.79 letters a word across
// Norvig's corpus of 743 billion, which is 5.79 with the space.
//
// The check that settles it: seventeen characters a second divided by 5.79 is
// 176 words a minute, and the BBC's own research put "175WPM about right". The
// typing constant gives 204, outside every published band.
//
//	https://norvig.com/mayzner.html
const ccCharsPerWord = 5.8

func paceFor(wpm int) float64 {
	for _, w := range captionSpeeds {
		if wpm == w {
			return float64(wpm) * ccCharsPerWord / 60
		}
	}
	return cc608Pace
}

// ccMaxPace is the fastest text may ever be put on the screen, in characters a
// second, whatever the backlog.
//
// This was the one number in the display that had no ceiling at all. The meter
// stood aside when it fell behind and the words went out at whatever the
// channel could carry — about sixty characters a second, four times reading
// speed — on the reasoning that being current matters more than reading evenly.
//
// That trade has been measured in the field and it came back badly. Ofcom
// sampled live subtitling across four exercises and found peaks of 290, 350 and
// 460 words a minute in around a quarter of news and entertainment programs,
// which it describes as unreadable for most viewers. Re-scoring the same
// subtitles with rapid text counted as a fault moved the share falling below
// acceptable from 23% to 68%, and the share rated very good to excellent from
// 24% to under 3%. Same words, same accuracy, one added criterion.
//
// Its recommendation is a hard cap of no more than 180 to 200 words a minute,
// and to recover latency by shortening the gap between captions rather than by
// speeding up the text. Two hundred is taken here because this is only the
// ceiling while draining — the reading speed on the page is what runs the rest
// of the time, and the box style already recovers the way Ofcom describes, by
// cutting the gap and never the readability.
//
//	https://www.ofcom.org.uk/siteassets/resources/documents/tv-radio-and-on-demand/broadcast-codes/other-codes/ofcoms-guidelines-on-providing-tv-and-on-demand-access-services.pdf
const ccMaxPace = 200 * ccCharsPerWord / 60

// burst is how much unspent allowance the meter may hold, in characters.
//
// A second's worth, and that was the whole of why the ceiling made this feel
// slower. The ceiling is right — it is what stops a display running at three
// hundred words a minute through a program — but it was being applied to every
// moment alike, including the one moment where speed costs nothing.
//
// A phrase model hands over a whole sentence at once and then says nothing for
// several seconds. Nobody is reading during the silence. Letting the allowance
// accrue through it, and spending it when the sentence lands, puts that
// sentence on the screen at once and leaves the rate over any longer window
// exactly where it was — which is the number the reading research is about.
// Capped at a second, the allowance was thrown away during every pause and the
// sentence was then metered out from nothing.
//
// The size is a phrase, because a phrase is what arrives at once. Continuous
// speech spends the allowance as fast as it accrues and settles at the pace,
// which is the case the ceiling exists for and is unchanged.
func (c *cea608) burst() float64 {
	if c.maxLag <= 0 {
		return c.pace
	}
	if n := c.pace * c.maxLag; n > c.pace {
		return n
	}
	return c.pace
}

// meterPace is the rate the meter runs at: the reading speed, or the catch-up
// ceiling when there is a backlog to drain. Callers hold the lock.
func (c *cea608) meterPace() float64 {
	if c.waiting() && c.pace < ccMaxPace {
		return ccMaxPace
	}
	return c.pace
}

func (c *cea608) next() [2]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	// One pair leaves per call, and that is the tick this encoder's clock runs
	// on. Counted before anything else, so every dwell below is measured on the
	// presentation timeline and not on ours.
	// Metered at speaking speed, and at the catch-up ceiling when there is a
	// backlog — never at whatever the channel happens to be worth.
	//
	// Characters only. A control code is sent twice and a decoder is only
	// guaranteed to drop the repeat when the two arrive back to back — so
	// withholding between them turns one carriage return into two, which rolls
	// twice and leaves a blank row between every pair of lines. The meter is
	// about how fast words appear; it has no business inside a control pair.
	pace := c.meterPace()
	if rate := c.pairRate(); rate > 0 {
		c.credit += pace / rate
		if max := c.burst(); c.credit > max {
			c.credit = max
		}
	}
	// Charged per character, not per pair.
	//
	// writeRune packs two characters into one pair wherever it can, and this
	// spent one credit per pair — so a rate set to fifteen characters a second
	// put thirty on the screen, and the words-a-minute figure on the page meant
	// half what it said. Reported as captions still being too fast, which they
	// were, by a factor of two.
	//
	// A pair whose second byte is padding carries one character; any other
	// carries two. Control codes are not charged at all: they are not words,
	// and withholding inside a doubled pair splits it and rolls twice.
	if c.maxLag > 0 && !c.popon && len(c.queue) > 0 && !c.headIsControl() {
		cost := 1.0
		if c.queue[0][1] != odd608(cc608Null) {
			cost = 2
		}
		if c.credit < cost {
			return [2]byte{odd608(cc608Null), odd608(cc608Null)}
		}
		c.credit -= cost
	}
	if len(c.queue) == 0 {
		// A box shows what it has the moment the screen is free.
		//
		// This is the difference between running slow and running offset, and
		// it is where the offset was. A caption was only sent once it was full
		// or a sentence had ended in it, so the box always held about a
		// caption's worth of speech — two rows is four seconds of it — and
		// everything reached the screen four seconds after it was said. The
		// same four seconds whatever the model: a streaming model that commits
		// a word a second still waited for sixty more characters before any of
		// them went up.
		//
		// So the writer no longer decides when a caption is sent. The display
		// asks: the moment there is nothing left on the wire, whatever has been
		// held is written out. That is self-correcting in both directions —
		// when it is keeping up the captions are small and current, and when it
		// falls behind they fill, because more has arrived by the time the wire
		// clears.
		//
		// Written out, and not shown: the two are a second and a half apart and
		// that gap was being paid twice. A full two row caption is forty-six
		// byte pairs, and forty-six pairs at the rate line 21 carries them is a
		// second and a half of transmission before the swap at the end of them
		// can flip it onto the screen. Waiting for the caption on screen to
		// finish its time and only then starting to transmit put that second
		// and a half after the dwell instead of inside it.
		//
		// A pop-on caption is meant to be loaded while the previous one is
		// still up — that is what the second memory is for. So the bytes go now
		// and the swap waits: the dwell is enforced on the swap in next, and
		// everything ahead of it drains during the caption it is replacing.
		if c.popon && c.worthShowing() {
			c.flushPopon(true)
			if len(c.queue) > 0 {
				p := c.queue[0]
				c.queue = c.queue[1:]
				return p
			}
		}
		// A box caption comes down when its time is up.
		//
		// A roll-up leaves its lines where they are because the next roll will
		// take them, and until then they are the most recent thing said. A box
		// has no next roll: the caption sits there until something replaces it,
		// so through a pause in the talking it sits there for the length of the
		// pause — a sentence from half a minute ago presented as though it were
		// current, and then a burst when the talking starts again. That is the
		// half of the pacing that hangs.
		//
		// So it is taken down once it has been readable for as long as it asked
		// for, with a moment's grace in case the next caption is a breath away.
		// Broadcast pop-on has an out time for every caption; this is that.
		if c.popon && c.started && len(c.held) == 0 && !c.lastBlock.IsZero() &&
			c.streamNow().Sub(c.lastBlock) > c.curDwell.want+ccPopLinger {
			c.ctrl(ccEDM)
			c.started = false
			c.col = 0
			c.lastBlock = time.Time{}
			c.lastText = time.Time{}
			p := c.queue[0]
			c.queue = c.queue[1:]
			return p
		}
		if c.started && !c.lastText.IsZero() && c.streamNow().Sub(c.lastText) > ccStaleAfter {
			c.ctrl(ccEDM)
			c.started = false
			c.col = 0
			c.lastText = time.Time{}
			if len(c.queue) > 0 {
				p := c.queue[0]
				c.queue = c.queue[1:]
				return p
			}
		}
		return [2]byte{odd608(cc608Null), odd608(cc608Null)}
	}
	// The moment a caption changes waits for the dwell, so that what is on the
	// screen has been there long enough to read. Which code that is depends on
	// the style: a roll-up changes the screen with a carriage return, a box
	// with the swap that shows what it has loaded. Either one's doubled copy is
	// exempt — control codes go out twice back to back, and a decoder is only
	// guaranteed to drop the repeat when it arrives as one.
	//
	// Everything a box sends before that swap goes out at the channel's own
	// speed and is held by nothing, which is the point of the style: the rows
	// are being written where they cannot be seen, so there is no reason to
	// spread them out and every reason not to. The caption is loaded and ready,
	// and the only thing waiting is the instant it appears.
	if p := c.queue[0]; p[0] == odd608(ccCtrlCC1) {
		switch {
		case c.popon && p[1] == odd608(ccEOC):
			switch {
			case c.blockCopies > 0:
				c.blockCopies--
			case c.streamNow().Sub(c.lastBlock) < c.popGap():
				return [2]byte{odd608(cc608Null), odd608(cc608Null)}
			default:
				c.lastBlock = c.streamNow()
				c.blockCopies = 1
				// The caption going up now decides how long the one after it
				// waits, because that is how long this one is on the screen.
				if len(c.popDwells) > 0 {
					c.curDwell, c.popDwells = c.popDwells[0], c.popDwells[1:]
					if c.curDwell.last && c.popPending > 0 {
						c.popPending--
					}
				}
			}
		case !c.popon && p[1] == odd608(ccCR):
			switch {
			case c.crCopies > 0:
				c.crCopies--
			case c.streamNow().Sub(c.lastCR) < c.minRollGap && !c.waiting():
				return [2]byte{odd608(cc608Null), odd608(cc608Null)}
			default:
				c.lastCR = c.streamNow()
				c.crCopies = 1
			}
		}
	}
	p := c.queue[0]
	c.queue = c.queue[1:]
	return p
}

// ccPopGather is how long a fragment waits for the rest of its sentence before
// it goes up on its own. Short, because the size bar below is what does the
// work now; this only catches the fragment nothing more is coming for.
const ccPopGather = 250 * time.Millisecond

// worthShowing reports whether what is held is a caption yet.
//
// The display shows what it has the moment the screen is free, which is what
// keeps a box current. Taken literally that put single words on the screen,
// alone, one after another — a phrase model hands over a fragment, the screen
// happens to be free, and three words become a caption of their own.
//
// So a fragment too short to be a caption waits a moment for the rest of its
// sentence. A caption's worth goes at once; anything ending a sentence is a
// caption whatever its length, because nothing more is coming for it; and
// anything that has waited out the gather goes rather than being held for words
// that may never arrive. Callers hold the lock.
// minCaptionChars is the fewest characters a caption may carry, derived rather
// than chosen.
//
// A caption is on screen for at least the guidance minimum, a second for one
// row, whatever else is true — so a caption of ten characters occupies a second
// to show ten characters, which is ten a second, against speech that produces
// fifteen and a half. The display is then losing ground on every caption and
// nothing downstream can win it back: that is a box falling further behind the
// longer it runs, and it is arithmetic rather than tuning.
//
// The break-even point is the minimum time on screen multiplied by the reading
// speed, and that is this. Above it the display gains on speech, below it the
// display cannot keep up however good the recognizer is. It moves with the
// reading speed on the page, because both sides of the sum do.
//
// The first two attempts at this bar were a whole row and then a third of one,
// picked for how they looked rather than worked out. A third of a row is ten
// characters, which is the losing side of this sum.
func (c *cea608) minCaptionChars() int {
	if c.pace <= 0 {
		return c.textCol / 2
	}
	// Rounded up, not to nearest: rounding down lands a character short of
	// break-even, which is the one side of this that does not work.
	n := int(math.Ceil(ccMinOnScreen(1).Seconds() * c.pace))
	if n > c.textCol {
		n = c.textCol
	}
	return n
}

func (c *cea608) worthShowing() bool {
	if len(c.held) == 0 {
		return false
	}
	if ccWordsWidth(analyze608(c.held)) >= c.minCaptionChars() {
		return true
	}
	if last := c.held[len(c.held)-1]; endsSentence(last) {
		return true
	}
	return c.streamNow().Sub(c.lastText) >= ccPopGather
}

// popGap is how long the caption on screen must stay before the next replaces
// it: what it asks for, or the guidance floor when captions are queued behind
// it. Callers hold the lock.
func (c *cea608) popGap() time.Duration {
	if c.waiting() {
		return c.curDwell.floor
	}
	return c.curDwell.want
}

// headIsControl reports whether the next thing out is a control code rather
// than text. Callers hold the lock.
func (c *cea608) headIsControl() bool {
	return len(c.queue) > 0 && c.queue[0][0] == odd608(ccCtrlCC1)
}

// reset throws away everything the encoder is holding, so it begins where the
// viewer does.
//
// Not a cull: the backlog cull drops what can no longer be shown in time and
// carries on. This drops what should never have been queued at all, at the one
// moment that can be known — the first picture the viewer is going to see.
func (c *cea608) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queue = c.queue[:0]
	c.held = c.held[:0]
	c.popDwells = c.popDwells[:0]
	c.popPending = 0
	c.started = false
	c.col = 0
	c.pendingBreak = false
	c.lastText = time.Time{}
	c.lastBlock = time.Time{}
	c.lastCR = time.Time{}
}

func (c *cea608) backlog() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.queue)
}

// ---------------------------------------------------------------------------

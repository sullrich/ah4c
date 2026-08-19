package main

import (
	"strings"
	"testing"
	"time"
)

// ccEvent is one thing the encoder put on the wire, on the presentation clock.
type ccEvent struct {
	at   time.Duration
	kind string // "EDM", "EOC", "RCL", "text"
	text string
}

// driveBox runs a box encoder for the given time at the line 21 rate, handing
// it text at the moments the feed asks for, and returns what went out with the
// doubled control codes collapsed to one event each.
func driveBox(t *testing.T, seconds float64, feed map[time.Duration]string) []ccEvent {
	t.Helper()
	c := newCEA608("box2", true, 4, 160, ccLagFallback)
	c.setPictureRate(cc608NominalRate)
	rate := float64(cc608NominalRate)
	tick := time.Duration(float64(time.Second) / rate)
	var out []ccEvent
	var prev [2]byte
	var text strings.Builder
	flushText := func(at time.Duration) {
		if text.Len() > 0 {
			out = append(out, ccEvent{at: at, kind: "text", text: text.String()})
			text.Reset()
		}
	}
	edm := [2]byte{odd608(ccCtrlCC1), odd608(ccEDM)}
	eoc := [2]byte{odd608(ccCtrlCC1), odd608(ccEOC)}
	rcl := [2]byte{odd608(ccCtrlCC1), odd608(ccRCL)}
	for at := time.Duration(0); at < time.Duration(seconds*float64(time.Second)); at += tick {
		for when, s := range feed {
			if when >= at && when < at+tick {
				c.pushText(s, true)
			}
		}
		c.advanceStream(tick)
		p := c.next()
		if p == prev && p[0] == odd608(ccCtrlCC1) {
			prev = [2]byte{}
			continue // the repeat of a doubled control code
		}
		prev = p
		switch {
		case p == edm:
			flushText(at)
			out = append(out, ccEvent{at: at, kind: "EDM"})
		case p == eoc:
			flushText(at)
			out = append(out, ccEvent{at: at, kind: "EOC"})
		case p == rcl:
			flushText(at)
			out = append(out, ccEvent{at: at, kind: "RCL"})
		case p[0]&0x7F >= 0x10 && p[0]&0x7F <= 0x17 && p[1]&0x7F >= 0x40:
			// a preamble starts a row: keep the rows apart in the text
			if text.Len() > 0 {
				text.WriteByte(' ')
			}
		case p[0] == odd608(ccCtrlCC1), p[0] == odd608(ccTabCC1):
			// other control: ENM, tabs
		default:
			for _, b := range p {
				if b&0x7F >= 0x20 {
					text.WriteByte(b & 0x7F)
				}
			}
		}
	}
	return out
}

// Every swap is preceded by an erase, with nothing between them: the order a
// broadcast caption file writes a caption change in, and the edge a cue-based
// decoder stamps the new caption from.
func TestBoxSwapIsErasedFirst(t *testing.T) {
	feed := map[time.Duration]string{
		1 * time.Second:  "The first caption goes up here.",
		6 * time.Second:  "And the second one replaces it.",
		11 * time.Second: "Then a third, to be sure.",
	}
	out := driveBox(t, 20, feed)
	eocs := 0
	for i, e := range out {
		if e.kind != "EOC" {
			continue
		}
		eocs++
		if i == 0 || out[i-1].kind != "EDM" {
			t.Fatalf("EOC at %v is not immediately preceded by EDM: %+v", e.at, out[max(0, i-3):i+1])
		}
		if gap := e.at - out[i-1].at; gap > 3*time.Second/30 {
			t.Fatalf("EDM to EOC took %v, expected the next pair", gap)
		}
	}
	if eocs != 3 {
		t.Fatalf("expected 3 captions, saw %d in %+v", eocs, out)
	}
}

// A box that is behind writes the next caption as late as it can be loaded in
// time, so the words arriving while the caption on screen still has time left
// join it rather than waiting a whole caption longer — and the swap itself is
// not delayed by the waiting.
func TestBoxFillsInsteadOfFlashingWhenBehind(t *testing.T) {
	feed := map[time.Duration]string{
		1 * time.Second: "We have the first caption up now.",
		// The next caption's words arrive in two pieces shortly after the
		// first goes up, the way a phrase model hands over a sentence.
		2600 * time.Millisecond: "Then the second,",
		3000 * time.Millisecond: "arriving a moment later.",
	}
	out := driveBox(t, 12, feed)
	var swaps []time.Duration
	var captions []string
	var cur []string
	for _, e := range out {
		switch e.kind {
		case "text":
			cur = append(cur, e.text)
		case "EOC":
			swaps = append(swaps, e.at)
			captions = append(captions, strings.Join(cur, " "))
			cur = nil
		}
	}
	if len(captions) != 2 {
		t.Fatalf("expected the late words to join the second caption and make 2 captions, got %d: %q", len(captions), captions)
	}
	if !strings.Contains(captions[1], "THEN THE SECOND") || !strings.Contains(captions[1], "ARRIVING A MOMENT LATER") {
		t.Fatalf("second caption should carry both pieces, got %q", captions[1])
	}
	// The first caption had the floor of a behind box: it must not have come
	// down before the guidance minimum for its rows, and the waiting must not
	// have held the swap past the floor by more than the estimate allows.
	held := swaps[1] - swaps[0]
	if held < ccMinOnScreen(1) {
		t.Fatalf("first caption was up only %v", held)
	}
	if held > 3*time.Second {
		t.Fatalf("first caption was held %v, the waiting delayed the swap", held)
	}
}

// A box that is keeping up is unchanged: with nothing on the screen, words go
// up the moment they are worth a caption.
func TestBoxShowsAtOnceWhenScreenIsFree(t *testing.T) {
	feed := map[time.Duration]string{
		1 * time.Second: "Nothing is on the screen yet.",
	}
	out := driveBox(t, 5, feed)
	for _, e := range out {
		if e.kind == "EOC" {
			if e.at > 2500*time.Millisecond {
				t.Fatalf("caption took until %v to go up with a free screen", e.at)
			}
			return
		}
	}
	t.Fatal("caption never went up")
}

// popLoad counts what a caption costs on the wire: the loading codes, a
// preamble and tab per row, the characters two to a pair, the erase and swap.
func TestPopLoad(t *testing.T) {
	c := newCEA608("box2", true, 4, 160, ccLagFallback)
	c.setPictureRate(cc608NominalRate)
	words := analyze608(strings.Fields("TWELVE CHARS"))
	// one row of 12 characters: 8 + 4 + 6 = 18 pairs
	rate := float64(cc608NominalRate)
	want := time.Duration(18 / rate * float64(time.Second))
	if got := c.popLoad(words); got < want-time.Millisecond || got > want+time.Millisecond {
		t.Fatalf("popLoad = %v, want %v", got, want)
	}
}

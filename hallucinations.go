package main

// What a speech model writes when nothing was said.
//
// Not one model's problem and so not in one model's file. Handed silence or a
// music bed, an encoder-decoder does not fall silent — it writes the thing it
// saw most often at the end of the videos it was trained on, and those videos
// end with somebody thanking an audience. The list below is the community
// hallucination dataset, collected against Whisper, and it catches the same
// phrases whichever model reaches for them.
//
// A model opts in by naming stockHallucination as its Suppress. It is safe to
// opt in to: it matches a whole caption against a fixed list, so the most it
// can discard is a caption that was entirely one of those. It cannot eat a
// sentence.

import "strings"

// stockHallucination reports a phrase that is this model answering a question
// nobody asked.
//
// Two kinds. The stock phrases it reaches for when handed silence, which come
// from the hours of video it was trained on whose closing seconds are somebody
// thanking an audience. And the sound it names when handed music, which comes
// from the same place: captioned video writes a stretch of music in brackets,
// so a stretch of music is what it writes.
//
// The apology is the third: handed a stretch it cannot make words out of, the
// model says it is sorry.
//
// What the documentation actually says, because the rest of this comment used to
// invent a reason and state it confidently. The model card names the behavior and
// names one fix, and it is not this one:
//
//	"Like most AED speech models, Cohere Transcribe is eager to transcribe,
//	even non-speech sounds. The model thus benefits from prepending a noise
//	gate or VAD (voice activity detection) model in order to prevent
//	low-volume, floor noise from turning into hallucinations."
//
// So the documented mitigation is the noise gate in captions.go, and this list is
// only the backstop behind it — for audio that clears the gate honestly because
// it has real energy, which is what a commercial bed or a crowd is. The card
// publishes no stock phrases and neither does anyone else; every entry below is
// here because it was seen coming out of this model, and nothing is here because
// it seemed likely. Sources are in the commit that added each one.
//
// All three kinds are matched against the whole phrase and never part of one.
// Somebody saying thank you mid-sentence keeps it, an actor apologizing in a
// scene keeps it, and a real sentence containing an aside in brackets keeps the
// aside. What is being caught is a caption that is entirely a stage direction,
// not a caption with one in it.
func stockHallucination(text string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	if isSoundEventTag(t) {
		return true
	}
	t = strings.Trim(t, " .,!?-—’'\"")
	// Typographic apostrophes fold to the plain one before the lookup. The model
	// writes "I’m" and a map key spelled "i'm" never matches it — every entry
	// here with an apostrophe in it was dead on arrival until this line existed.
	t = strings.ReplaceAll(t, "’", "'")
	// One lookup. There was a second against t+"." as well, which could never
	// find anything the first had not: the trim above strips the period before
	// either of them runs, so a dotted key is only ever reachable by putting the
	// dot back on. The two dotted entries it existed for had undotted twins.
	return stockPhrases[t]
}

// The list comes from the community dataset, filtered by one rule.
//
// This is how it is actually done, and the method has two halves. A voice
// activity gate stops non-speech reaching the model, and a phrase list catches
// what gets through; measured on Common Voice, the gate alone took about 1.5%
// absolute off word error rate, the list alone about 2.5%, and the two together
// about 3.5%. Cohere's card recommends the gate half and says nothing about
// this one.
//
//	Investigation of Whisper ASR Hallucinations Induced by Non-Speech Audio
//	https://arxiv.org/pdf/2501.11378
//
// The list itself is not guesswork either. It was built by running the model
// over a noise-only corpus and counting what came out, which is 7,889 phrases
// across every language and 348 in English:
//
//	https://huggingface.co/datasets/sachaarbonel/whisper-hallucinations
//
// What that data shows is why hand-picking was always going to go wrong. The
// most frequent English hallucinations are "bye", "you", "thank you" and "the" —
// all of them ordinary speech, and all of them things this list has had in it at
// one time or another. Blocking those deletes conversation, and no amount of
// arguing about it settles anything, because they really are both.
//
// So the rule is structural rather than statistical: a phrase belongs here only
// if a person on a television channel could not have said it. That admits two
// families and no others.
//
// Captioning and transcription credits, which come from the subtitle files the
// model was trained on. A broadcaster does not read its own caption vendor out
// loud.
//
// Video sign-offs, which come from the same place: an appeal to subscribe, a
// thank-you for watching, a promise to see you in the next one. Broadcast has
// its own version of these and it is not this one.
//
// Deliberately excluded, though the dataset lists all of them: every
// interjection (isSoundEventTag has the bracketed ones and the rest are real
// speech), every courtesy — thank you, thanks, bye, okay, I'm sorry — and
// anything a channel might genuinely say, which is why "we'll be right back" is
// not here even though the model hallucinates it. Frequency was not used as a
// criterion at all. The most frequent entries are the most dangerous ones.
var stockPhrases = map[string]bool{
	// Caption and transcript credits.
	"subtitles by the amara.org community":                    true,
	"subtitles by the amara org community":                    true,
	"amara.org":                                               true,
	"subtitles by steamteamextra":                             true,
	"captioned by cotter captioning services":                 true,
	"captions by gettranscribed com":                          true,
	"captions by gettranscribed.com":                          true,
	"captions by nicosubs":                                    true,
	"closed captioning by kris brandhagen com":                true,
	"closed captioning by kris brandhagen.com":                true,
	"closed captioning provided by muhsen":                    true,
	"closed captioning provided by the imperial news network": true,
	"transcribed by eso translated by":                        true,
	"transcription by eso translation by":                     true,
	"transcript emily beynon":                                 true,
	"tanya cushman reviewer":                                  true,
	"tanya cushman reviewer's":                                true,
	"rev com":                                                 true,
	"rev.com":                                                 true,
	"otter ai":                                                true,
	"otter.ai":                                                true,

	// Video sign-offs and channel patter.
	"thanks for watching":                           true,
	"thank you for watching":                        true,
	"thanks for watching please subscribe":          true,
	"thank you for watching please subscribe":       true,
	"please subscribe and like thanks for watching": true,
	"please subscribe to my channel thank you":      true,
	"please subscribe":                              true,
	"please subscribe to my channel":                true,
	"like and subscribe":                            true,
	"don't forget to subscribe":                     true,
	"and subscribe to my channel":                   true,
	"welcome to my channel":                         true,
	"hi guys welcome back to my channel":            true,
	"hello everyone welcome to my channel":          true,
	"and i'll see you in the next one":              true,
	"and i'll see you in the next video":            true,
	"and i'll see you next time":                    true,
	"so i'll see you next time":                     true,
	"the next video":                                true,

	// The courtesies, which are both.
	//
	// These fail the rule above — people say them on television constantly —
	// and they are in here anyway, because the dataset ranks them among the
	// most frequent hallucinations there are and two of them turned up over a
	// music bed within minutes of taking them out. Both errors are real and
	// the choice is which one to make; this is the one that happens less.
	//
	// What it costs is a caption whose entire content is the courtesy. Anything
	// around it survives — "thank you, Jim, back to you" is untouched, because
	// the match is against the whole phrase and never part of one. A standalone
	// "Thank you." is close to the least informative caption there is, and a
	// standalone "Thank you." over music is noise in the middle of a programme.
	//
	// The line is drawn at courtesies and no further. "bye", "you", "okay" and
	// "the" score higher in the dataset than any of these and are not here:
	// they carry nothing that marks them as stock, and a filter that eats them
	// eats conversation rather than pleasantries.
	"thank you":           true,
	"thanks":              true,
	"thank you very much": true,
	"i'm sorry":           true,
	"i am sorry":          true,
}

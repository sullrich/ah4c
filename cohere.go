package main

import "strings"

// Cohere Transcribe 03-2026: the model, and everything it asks of the code
// around it.
//
// This is a whole file for one model because this model has earned one. It is
// the most accurate open recognizer there is and it has opinions — about how
// much audio it wants at a time, about what happens when it is handed a
// stretch that has no speech in it, about what it says when it would rather
// say nothing. Each of those was found the hard way and each has its reason
// written beside it.
//
// It is a separate file so that the day it is replaced, this file is deleted.
// Two references break when it goes — its line in captionModelCatalog and its
// case in quirksFor — and the compiler names both, which is a better guarantee
// than a comment asking somebody to remember.
//
// Nothing here is imported by anything except through those two. The
// recognizer, the caption encoders, the injector and the tune gate have no
// idea which model they are serving, and that is the property worth keeping.

var cohereTranscribe = captionModel{
	Key:  "cohere-transcribe",
	Name: "Cohere Transcribe 03-2026",
	Role: "Best for one to three streams on CPU, or any number on a GPU",
	Desc: "The most accurate open speech model there is, and the top of the public leaderboard. It reads a phrase at a time and what it writes reads like the closed captions on a broadcast channel. One copy is shared by every tuner, and the delay setting below decides how closely it follows the picture.",
	// It reads a whole phrase and then writes it, so the delay setting
	// governs how far behind it runs; the batch service amortizes its
	// per-call cost across every tuner.
	Latency:   "Two to four seconds behind the picture, like live broadcast captioning",
	Accuracy:  "Best available",
	Benchmark: "1.3% of words come out wrong",
	Hardware:  "One to three streams on the processor is fine; anything more, use a GPU — integrated graphics are plenty. Guidance, not a gate: the log will tell you honestly whether it keeps up.",
	Runtime:   rtTranscribe,
	Repo:      "handy-computer/cohere-transcribe-03-2026-gguf",
	// Q4_K_M, after Q8_0 was tried on this machine and measured.
	//
	// The published table says the quantization does not matter for accuracy:
	// BF16 and F16 at 1.26% word error, Q8_0 and Q6_K at 1.27%, Q5_K_M and
	// Q4_K_M at 1.25%. Four bit is tied for the best there and the whole spread
	// is two hundredths of a point.
	//
	// It does matter, and the table cannot see it, because every one of those
	// figures comes from a reference run rather than from Vulkan. Q8_0 was
	// noticeably more accurate here — brand names and proper nouns that came
	// back wrong at four bits came back right — which says the shaders differ
	// in arithmetic as well as in speed, and that nobody has published a number
	// for the backend most people will actually run this on.
	//
	// It is not shipped, because it cost more than it bought. Eight bits
	// measured 4.2 times real time against 7.4 at four, which on a recognizer
	// that carries about as many streams as its multiplier is the difference
	// between four captioned tuners and seven. At five tuners the queue stopped
	// draining, streams ran thirteen seconds behind and phrases were skipped to
	// stay current — and words skipped for staleness are worse than words
	// spelled wrong. Q6_K was tried in between and was not good.
	//
	// So: four bits for the throughput, extra recognizers for the streams
	// beyond one recognizer's share, and the accuracy made up elsewhere. The
	// full precision attention cache below is the first of that.
	File:        "cohere-transcribe-03-2026-Q4_K_M.gguf",
	SizeMB:      1550,
	Punctuation: true,
	Languages:   []string{"auto", "de", "en", "es", "fr", "it", "nl", "pl", "pt"},
}

// ---------------------------------------------------------------------------
// Cohere Transcribe 03-2026
// ---------------------------------------------------------------------------

// Three things, and the model's own card asks for two of them.
//
// Delete this block and its entry in quirksFor and nothing else has to change:
// the recognizer, the caption encoders, the injector and the tune gate have no
// idea which model they are serving.
var cohereQuirks = modelQuirks{
	// Four seconds. Three was tried and the accuracy went with it, worst over
	// advertisements — the thinnest speech on television, and so the least
	// able to spare a second of context. The card points the same way: it
	// gives no minimum duration and its only length guidance is about long
	// audio, thirty-five second splits and chunks of twenty to sixty. This is
	// a model built to be handed plenty at once.
	PhraseWindow: 4.0,

	// "Cohere Transcribe is eager to transcribe, even non-speech sounds. The
	// model thus benefits from prepending a noise gate or VAD in order to
	// prevent low-volume, floor noise from turning into hallucinations." Its
	// words, and the reason the gate exists. There is nothing on the engine's
	// side to use instead: its run parameters carry a task, a language,
	// punctuation and inverse text normalization, and no threshold of any kind.
	NoiseGate: true,

	// And for what gets past the gate, because applause and a music bed vary
	// enough to look like speech.
	Suppress: cohereSuppresses,

	// Full precision for the attention cache, which is the one accuracy knob
	// the engine actually exposes and the only one it documents in those
	// terms. Its header: F32 is "full-precision KV. Maximum accuracy, highest
	// bandwidth", against the default of F16, "minimal precision loss (~3
	// decimal digits). Best for bandwidth-bound backends (integrated GPUs,
	// CPU)".
	//
	// The default is the right general advice and the wrong trade here. This
	// model is chosen for being the most accurate there is, on a machine that
	// runs it seven times faster than real time — so it is spending its
	// precision to buy speed it already has. Three decimal digits is not
	// nothing when a word turns on the difference between two vowels.
	//
	// It costs bandwidth on an integrated GPU, where bandwidth is the scarce
	// thing, and the attention cache is only part of the work — the encoder
	// dominates, a second against a fifth for the decode. If the real-time
	// factor in the log falls somewhere it matters, this is the line to
	// reconsider, and the streaming models do not use it at all.
	KVType: txKVF32,

	// Punctuation and inverse text normalization asked for, and this model does
	// not take either.
	//
	// Both are documented as per-call toggles, and inverse text normalization
	// is the one that would matter here: it turns spoken numbers, dates and
	// currencies into their written form, and television is made of prices,
	// years and numbers to call. It was worth asking for.
	//
	// The engine's answer is that cohere_asr exposes no runtime control over
	// either, and its output already carries punctuation. So these are kept as
	// the intent rather than removed — a later build of this family may expose
	// them, and then it costs nothing to have said so — but they are now put
	// through the capability probe the header names, which asks the loaded
	// weights and drops anything they will not take. Without that the engine
	// warned per call: two lines of log for every phrase of every stream, for
	// ever.
	PNC: txPNCOn,
	ITN: txITNOn,

	// As many recognizers as the page asks for, up to eight.
	//
	// This model saturates one. Measured on an integrated graphics chip with
	// five streams captioning: four phrases to a dispatch, two and a fifth
	// seconds of compute for nine seconds of audio, and streams reporting seven
	// and eight seconds behind with nothing dropped — a queue that no longer
	// drains as fast as it fills. Its encoder runs serially across a batch, so
	// batching relieves the decode and not the encode, and the only way to have
	// two encoder passes in flight is to have two copies of the weights.
	//
	// Eight is a ceiling rather than a recommendation, and it is set by what a
	// machine can hold rather than by what is likely to help: each recognizer
	// is another full copy of the weights, two and a half gigabytes at eight
	// bits, so eight of them is twenty gigabytes of it. Whoever is running this
	// knows their own machine better than a number in a file does, so the page
	// offers every one of them and says what each costs.
	//
	// What is worth knowing rather than enforcing: the graphics gate admits two
	// transcriptions at once and holds the rest, so past two on a GPU the extra
	// copies mostly wait. On the processor they do not — threads are shared out
	// there and it really does run things at the same time — which is where a
	// larger number earns itself.
}

// cohereSuppresses reports a phrase that is this model answering a question
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
func cohereSuppresses(text string) bool {
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
	return cohereStockPhrases[t]
}

var cohereStockPhrases = map[string]bool{
	"thank you": true, "thanks": true,
	"thanks for watching": true, "thank you for watching": true,
	"you": true, "bye": true, "okay": true,
	"please subscribe": true, "subtitles by the amara.org community": true,
	// The apology. Reported against this model on this machine, 2026-08-17.
	// "i am sorry", "i'm so sorry" and a bare "sorry" were in here briefly and
	// have been taken out: nobody saw those, I guessed at them, and a bare
	// "sorry" is an entirely ordinary line of dialogue to throw away on a guess.
	"i'm sorry": true,
}

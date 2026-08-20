package main

// Parakeet Unified EN 0.6B: the model, and everything it asks of the code
// around it.
//
// The most accurate thing here that still shares one copy between every tuner.
//
// 1.60% of words wrong against Cohere's 1.25% and Canary's 1.93%, from a model
// a little over half Cohere's size. It is offline here, which is the half of it
// that matters for a machine captioning several tuners at once: an offline
// model is loaded once and every stream is served from that one copy, batched
// together, while a streaming model is loaded per stream and cannot be shared
// at all. The engine's threading contract is what decides that and it says so
// — one compute in flight per model, load one model per worker for parallel
// work — and a per-session architecture that would lift it is named as planned
// rather than present.
//
// It is a unified model and does stream, which is the interesting thing about
// it, and that is a choice on the page now rather than an unused capability.
//
// The engine runs this family's buffered streaming at (70, 13, 13) — 5.6s of
// left context, a 1.04s chunk, 1.04s of right context — and its own docs call
// that the highest-accuracy tuple and the row the model card reports streaming
// accuracy for. Latency there is the chunk plus the right context, so 2.08
// seconds, at 1.44% of words wrong against 1.62% for these offline weights.
// Real time is, if anything, the more accurate of the two modes here.
//
// What it does not buy is much of the wait. Two seconds of lookahead is close
// to what waiting for a sentence costs anyway, so the difference a viewer sees
// is mostly that words arrive as they are spoken instead of a whole caption
// landing at once. Tighter tuples exist and the port publishes them — 1.64% at
// 320ms, 1.40% at 1.12s — but the engine is not asked for one, because picking
// a config it does not default to means owning that choice on every machine
// this runs on and nothing here has measured one.
//
// What it costs is a copy per tuner, in memory and in the graphics chip, and
// the work of re-encoding: that window is recomputed for every chunk, so each
// second of audio is encoded several times over. It was reported here at about
// twice the offline mode's processing. That is a trade a machine captioning ten
// tuners cannot make and a machine captioning two may well want, so it is the
// transcription mode in the configuration: chosen on the page, a sentence at a
// time by default, and decided in modelStreams.
//
// Delete this file and two references break, both named by the compiler: its
// line in captionModelCatalog and its case in quirksFor.

var parakeetUnified = captionModel{
	Key:  "parakeet-unified",
	Name: "Parakeet Unified EN 0.6B",
	Role: "Recommended",
	Desc: "Within half a point of the most accurate model here, and one copy serves every tuner. English only. Runs in real time too, if you ask for it.",
	// A phrase model as it is used by default: nothing is transcribed until the
	// sentence is complete. Set to real time it commits words as they settle,
	// about two seconds behind the sound rather than a sentence behind it.
	Latency:   "A couple of seconds behind; in real time the words arrive as they are spoken",
	Accuracy:  "Excellent",
	Benchmark: "1.6% of words come out wrong, 1.4% in real time",
	Hardware:  "One copy serves every tuner. Heavier on the graphics chip than Canary, far lighter than Cohere. Real time loads a copy per tuner at roughly double the work.",
	Runtime:   rtTranscribe,
	Repo:      "handy-computer/parakeet-unified-en-0.6b-gguf",
	// Q4_K_M, and accuracy did not choose it because accuracy has nothing to
	// say: 1.59% at full precision, 1.60% at Q8_0, 1.58% at Q5_K_M and 1.62%
	// here. Four hundredths of a point across a file that more than halves.
	//
	// Speed chose it, on the only evidence anyone has for this class of chip.
	// Cohere's author measured that model at eight times real time on
	// integrated graphics at Q4_K_M where Q5_K_M managed a third of that — the
	// shaders differ per quantization and the difference was not small. Nothing
	// equivalent is published for this model, so the nearest thing to evidence
	// is the same quantization on the same engine on the same kind of chip,
	// which is how Nemotron's was chosen too.
	//
	// It is also the question this model is being added to answer. Whether it
	// carries ten captioned tuners is decided on the graphics chip and not on
	// the disk, so of two files that transcribe the same the faster one is the
	// only one worth shipping — and it happens to be the smaller.
	//
	// If it disappoints, Q8_0 is the one preset the engine does not mix and the
	// first thing to try. The log prints the real-time factor to settle it.
	File:      "parakeet-unified-en-0.6b-Q4_K_M.gguf",
	SizeMB:    477,
	Streaming: false,
	// Offline by default, and the streaming mode is real: the engine runs this
	// model either way — "buffered streaming is supported on
	// parakeet-unified-en-0.6b", and on nothing else in its family — so the
	// page offers the choice. See modelStreams.
	EitherMode:  true,
	Punctuation: true,
	// Cased and punctuated by the model, and English only: its own command line
	// passes "--language en" and the model does nothing else.
	NoLanguage: true,
	Languages:  []string{"en"},
}

// The same handling Canary asks for, and for the same reasons.
//
// A noise gate is deliberately absent: it went on Canary by reading a sentence
// about AED models on another model's card, came off when words went missing,
// and the missing words turned out to be something else entirely. Nothing in
// this model's card says it transcribes non-speech, so it does not get one
// until it is seen doing it.
//
// The phrase list stays, because it cannot eat a sentence — it matches a whole
// caption against a fixed list of things a model writes when handed silence.
//
// Context in front of every phrase, so the phrase can be short without the
// model being handed a fragment. The same figure and the same reasoning as
// Canary, and it holds here for the same measured reason: a conformer encoder
// costs about the same for a long phrase as a short one, so context is paid for
// in bytes rather than in time.
var parakeetUnifiedQuirks = modelQuirks{
	Suppress: stockHallucination,
	// No carried context, and the full phrase window. Tried the other way and
	// took it back: with the carry on, the trimmer sometimes matched further
	// than the repeat actually went and took real words off the front of a
	// phrase, so captions skipped forward in the sentence — from the sofa it
	// reads as the captions running ahead of the speaker.
	//
	// A caption two seconds late is worse to look at than one that is early is
	// to read. But a caption missing its first words is not late or early, it
	// is wrong, and no amount of timing makes it right again.
	//
	// The trimmer that tolerates the model disagreeing with itself is still
	// there and does no harm — with nothing carried it is never asked. What is
	// missing before this can be tried again is knowing how far the repeat
	// really goes, and matching words is a guess at that. Word timings from the
	// engine would answer it exactly.
	PhraseWindow: modelDefaults.PhraseWindow,
	// 1040 ms a feed when the streaming mode is chosen, which is the engine's
	// own default for this family and not a guess: it runs buffered streaming
	// at (70, 13, 13) — 5.6s of left context, a 1.04s chunk, 1.04s of right
	// context, the tuple the model card's streaming accuracy is reported at —
	// and its command line ships --stream-buf-chunk-ms 1040. Feeding less does
	// not produce a word sooner, because the model cannot commit ahead of its
	// right context; smaller pieces only re-encode the same window more often.
	// The phrase path never reads this.
	StreamChunkSec: 1.04,
}

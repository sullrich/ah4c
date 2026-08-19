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
// It is a unified model and does stream, which is not used here and is the
// interesting thing about it. In streaming mode the port publishes 1.64% at a
// third of a second of lookahead and 1.40% at a second, with no phrase to wait
// for at all — better than this offline figure and far closer to the sound.
// What it costs is a copy per tuner, in memory and in the graphics chip, and
// that is the trade this program cannot make on a machine captioning ten of
// them. If the engine ever shares a model across concurrent streams, this entry
// is the one to revisit first.
//
// Delete this file and two references break, both named by the compiler: its
// line in captionModelCatalog and its case in quirksFor.

var parakeetUnified = captionModel{
	Key:  "parakeet-unified",
	Name: "Parakeet Unified EN 0.6B",
	Role: "Recommended",
	Desc: "Within half a point of the most accurate model here, and it shares one copy across every tuner rather than loading one each. English only. Start here.",
	// A phrase model as it is used here: nothing is transcribed until the
	// sentence is complete.
	Latency:   "A couple of seconds behind",
	Accuracy:  "Excellent",
	Benchmark: "1.6% of words come out wrong",
	Hardware:  "Shares one copy across every tuner being captioned, so the memory does not grow with the tuner count. Heavier on the graphics chip than Canary and far lighter than Cohere.",
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
	File:        "parakeet-unified-en-0.6b-Q4_K_M.gguf",
	SizeMB:      477,
	Streaming:   true,
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
	// A fifth of a second a feed. The model keeps a running encoder state, so a
	// feed adds frames to it rather than re-running anything, and feeding more
	// often is the difference between a word appearing when it settles and up
	// to a second later.
	StreamChunkSec: 0.2,
	PhraseWindow:   modelDefaults.PhraseWindow,
	Suppress:       stockHallucination,
	// No carried context, deliberately. It was confined to Canary and then
	// handed to this model when it was added, which put every phrase's opening
	// words on screen a second time.
	//
	// The carry only works if the model renders the same audio the same way
	// twice: trimOverlap takes the repeat off the front by matching words, and
	// Canary is an encoder-decoder that re-transcribes the whole clip and comes
	// back with the same words. This one does not — it commits tokens as it
	// goes, so the second rendering of the carried seconds differs in casing,
	// in punctuation or in where a word was split, the match fails, and the
	// context is shown rather than removed.
	//
	// What the carry buys is accuracy on a short phrase. What it cost here was
	// every caption repeating itself, which is worth more than the accuracy.
}

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
	// Q8_0, and the reason is speed rather than size.
	//
	// One copy serves every tuner, so the file size is not the constraint it
	// would be on a model loaded per stream. What is left is how fast the
	// quantization decodes, which is published for none of these and has been
	// measured once on this class of chip — for a different model, where the
	// difference between two K presets was threefold. Q8_0 is the one preset
	// the engine does not mix: every K preset falls back to Q8_0 for tensors
	// whose dimensions do not divide the block size.
	//
	// Accuracy does not choose it either. This model measures 1.59% at F16,
	// 1.60% at Q8_0 and 1.58% at Q5_K_M — flat, and the K preset is nominally
	// the best of them. If the real-time factor in the log disappoints, Q5_K_M
	// is the first thing to try and it costs nothing in accuracy to try it.
	File:        "parakeet-unified-en-0.6b-Q8_0.gguf",
	SizeMB:      731,
	Streaming:   false,
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
	PhraseWindow: modelDefaults.PhraseWindow,
	Suppress:     stockHallucination,
	ContextSec:   vadMinPhrase,
}

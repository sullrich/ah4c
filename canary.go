package main

// Canary 180M Flash: the model, and everything it asks of the code around it.
//
// The default, and the one to reach for unless something specific says
// otherwise.
//
// Cohere is the most accurate thing here and its encoder is the size of that
// claim: a billion parameters and more, and the engine runs the encoder once
// per phrase and serially, by design — batching a heavy conformer over ragged
// lengths regresses, so it does not. On integrated graphics that works out at
// about half a second of encoder for every phrase whatever its length, which is
// five captioned tuners of ordinary television and no more.
//
// This is a hundred and eighty-two million parameters for 1.93% of words wrong
// against Cohere's 1.25%. Two thirds of a point, for an encoder a fraction of
// the size — and the encoder is where nearly all of the time goes, so that is
// the trade being made and it is the only one on offer that is not a change of
// shape. It is offline like Cohere, so one copy serves every tuner and phrases
// batch; it is not a streaming model that would load a copy per tuner.
//
// Four languages against Cohere's eight, and no more than that: it does English,
// German, Spanish and French, and nothing else. Somebody captioning Polish
// wants the other one and no amount of throughput changes that.
//
// Delete this file and two references break, both named by the compiler: its
// line in captionModelCatalog and its case in quirksFor.

var canaryFlash = captionModel{
	Key:  "canary-180m",
	Name: "Canary 180M Flash",
	Role: "Recommended",
	Desc: "Nearly the accuracy of the best one at a sixth of the cost, so an ordinary machine captions several tuners at once rather than four or five. English, German, Spanish and French. Start here.",
	// A phrase model: nothing is transcribed until the sentence is complete.
	Latency:   "A couple of seconds behind",
	Accuracy:  "Excellent",
	Benchmark: "1.9% of words come out wrong",
	Hardware:  "Shares one copy across every tuner being captioned, so the memory does not grow with the tuner count. The lightest of the accurate models on the graphics chip.",
	Runtime:   rtTranscribe,
	Repo:      "handy-computer/canary-180m-flash-gguf",
	// Q8_0, and the accuracy is not what chose it.
	//
	// This model measures 1.93% at Q8_0, 1.93% at Q6_K, 1.90% at Q5_K_M and
	// 1.93% at Q4_K_M — flat across the whole range, so the file size is the
	// only thing the choice could be about, and at two hundred megabytes shared
	// between every tuner the file size is not worth choosing on.
	//
	// What is worth choosing on is speed, and speed per quantization is not
	// published for any of these. It has been measured once on this class of
	// chip, for a different model: Cohere's author found Q4_K_M running at eight
	// times real time on integrated graphics where Q5_K_M managed a third of
	// that, because the shaders differ per quant. So the honest position is that
	// nobody knows for this one, and Q8_0 is the one preset the engine does not
	// mix — every K preset falls back to Q8_0 for tensors whose dimensions do
	// not divide the block size, so Q6_K and below are part Q8_0 anyway.
	//
	// If it disappoints, the quantization is the first thing to try and the log
	// prints the real-time factor to settle it with.
	File:        "canary-180m-flash-Q8_0.gguf",
	SizeMB:      208,
	Streaming:   false,
	Punctuation: true,
	// Bare codes and not locales, which is what its own command line documents:
	// "-l <code> — source language code (en, de, es, fr)". Four, and only the
	// four; the model does translation as well and this program does not ask
	// for it.
	Languages: []string{"en", "de", "es", "fr"},
}

// The noise gate, and why a model that has not been caught doing it gets one.
//
// Cohere's card is the source and it does not describe only Cohere: "Like most
// AED speech models, Cohere Transcribe is eager to transcribe, even non-speech
// sounds. The model thus benefits from prepending a noise gate or VAD model in
// order to prevent low-volume, floor noise from turning into hallucinations."
//
// This is an AED speech model — a FastConformer encoder with a transformer
// decoder — so the sentence covers it. The gate costs a stretch of steady noise
// that had no speech in it, which is nothing, and the failure it prevents is a
// commercial's music bed transcribed as words.
//
// The phrase list behind it is shared rather than copied, because it was never
// a Cohere list: it is the community hallucination dataset, collected against
// Whisper, and what it catches is the stock phrase an AED reaches for when
// handed silence. Whichever model is doing it, it is the same phrase.
var canaryQuirks = modelQuirks{
	PhraseWindow: modelDefaults.PhraseWindow,
	NoiseGate:    true,
	Suppress:     cohereSuppresses,
}

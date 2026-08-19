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
	// Not "Recommended": the page works that out for the machine it is running
	// on and puts a badge on whichever model it picks. Saying it here as well
	// put the word beside two different models at once.
	Role: "Lightest of the accurate ones",
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

// No noise gate, and the reason is that it was put here on an inference.
//
// Cohere's card says "Like most AED speech models, Cohere Transcribe is eager
// to transcribe, even non-speech sounds", and this is an AED speech model, so
// the gate was switched on by reading that sentence as covering it. That is not
// the same as this model having been caught doing it, and its own card says
// nothing of the kind.
//
// What the gate costs when it is wrong is not small. It discards a whole phrase
// — up to a second and a fifth of speech — on a shape measurement, and a phrase
// discarded is a gap where somebody was talking. Reported as exactly that:
// words missing, the display stopping for a second at a time.
//
// So it is off until this model is seen hallucinating on silence, rather than
// on until it is seen not to. The evidence for switching it on would be the
// stock phrases turning up over quiet passages, and the line below catches
// those anyway.
//
// The phrase list stays, and it is safe in a way the gate is not: it matches a
// whole phrase against a fixed list of things models say when handed silence,
// so the most it can discard is a caption that was entirely one of those. It
// cannot eat a sentence. And it was never a Cohere list — it is the community
// hallucination dataset, collected against Whisper, which is to say it is a
// list of what AED models write when there is nothing to write.
//
// No sentence length choice, and it is not an oversight.
//
// One was added here by copying Cohere's list across, on the reasoning that
// there was no reason to withhold the choice. There was: the list is what makes
// a saved sentence length apply. phraseWindowFor takes the configured value
// only when the model's own list contains it, so a model with no list ignores
// whatever is saved and runs its own window — and the moment this one had a
// list, a length chosen for Cohere started driving a model it was never chosen
// for. It showed up as the accuracy falling apart.
//
// Cohere offers the choice because its card discusses the trade and its numbers
// were measured across it. Nothing of the kind is published for this one, so
// what would be on offer is a range of untested operating points with the
// tested one buried among them.
var canaryQuirks = modelQuirks{
	PhraseWindow: modelDefaults.PhraseWindow,
	Suppress:     stockHallucination,
	// Context in front of every phrase, so the phrase can be short.
	//
	// Cutting early is what puts captions close to the sound, and what stops it
	// is that a model handed a fragment guesses: "I would in a heartbeat" came
	// back as "I wouldn't a hot beat" from a phrase that began inside it. That
	// is an argument about how much this model sees, not about how often the
	// segmenter cuts, and the two were the same thing only because each phrase
	// was handed over alone.
	//
	// It is close to free here and that is why it is here and not everywhere:
	// this family's encoder costs about the same for a long phrase as a short
	// one, which its own log prints. On a family where that has not been
	// measured, context would be paid for in time and this should stay zero.
	//
	// The figure is the shortest phrase a model of this kind works from, which
	// is what the phrase floor used to be. Moving it from the phrase to the
	// context is the whole idea: what the model sees does not shrink when the
	// phrase does.
	ContextSec: vadMinPhrase,
}

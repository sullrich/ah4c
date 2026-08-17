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
	// Q8_0, and it is worth being exact about why, because the published
	// numbers do not say what one would expect.
	//
	// The model's own table gives word error by quantization: BF16 and F16 at
	// 1.26%, Q8_0 and Q6_K at 1.27%, Q5_K_M and Q4_K_M at 1.25%. The four bit
	// quant is not the least accurate of them — on that measurement it is tied
	// for the best, and the whole spread is two hundredths of a point, which is
	// noise. Nobody should choose between these on that table.
	//
	// What the table does not cover is the backend. It was measured on
	// LibriSpeech with greedy decoding, and every one of those figures comes
	// from a reference run rather than from Vulkan. The engine's own author
	// found the quants behave differently on a graphics chip — that is why this
	// was Q4_K_M in the first place, after Q5_K_M measured a third of the speed
	// on integrated graphics — and if shader quality varies by quant for speed
	// there is no reason it cannot vary for arithmetic too. Nobody has
	// published an accuracy measurement for this model on Vulkan at all.
	//
	// So this is a test of that, not an upgrade justified by the vendor's
	// numbers. Q8_0 has the simplest dequantization of the set, which is the
	// least room for a shader to be subtly wrong. It costs 2.41 GB on disk
	// against 1.56, and about the same again in memory — once, since a phrase
	// model is shared by every tuner. If the substitutions on proper nouns stop,
	// the hypothesis was right. If they do not, this reverts to one filename
	// and a number, and the answer is that the model does not know the word.
	File:        "cohere-transcribe-03-2026-Q8_0.gguf",
	SizeMB:      2410,
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

	// Up to three recognizers, if the page asks for them.
	//
	// This model saturates one. Measured on an integrated graphics chip with
	// five streams captioning: four phrases to a dispatch, two and a fifth
	// seconds of compute for nine seconds of audio, and streams reporting seven
	// and eight seconds behind with nothing dropped — a queue that no longer
	// drains as fast as it fills. Its encoder runs serially across a batch, so
	// batching relieves the decode and not the encode, and the only way to have
	// two encoder passes in flight is to have two copies of the weights.
	//
	// Three is a ceiling rather than a recommendation. Each one costs another
	// full copy — two and a half gigabytes for the eight bit weights — and the
	// graphics gate lets two decodes through at a time, so a third only helps
	// where the processor is doing the arithmetic.
	MaxWorkers: 3,
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
// Both are matched against the whole phrase and never part of one. Somebody
// saying thank you mid-sentence keeps it, and a real sentence containing an
// aside in brackets keeps the aside. What is being caught is a caption that is
// entirely a stage direction, not a caption with one in it.
func cohereSuppresses(text string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	if isSoundEventTag(t) {
		return true
	}
	t = strings.Trim(t, " .,!?-—’'\"")
	return cohereStockPhrases[t] || cohereStockPhrases[t+"."]
}

var cohereStockPhrases = map[string]bool{
	"thank you": true, "thanks": true, "thank you.": true,
	"thanks for watching": true, "thank you for watching": true,
	"you": true, "bye": true, "bye.": true, "okay": true,
	"please subscribe": true, "subtitles by the amara.org community": true,
}

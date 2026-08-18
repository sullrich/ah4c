package main

// Nemotron 3.5 ASR Streaming 0.6B: the model, and everything it asks of the
// code around it.
//
// The multilingual one, and the quick one.
//
// Cohere Transcribe reads eight languages and this reads thirty-two, which
// makes it the only real answer for most of the world rather than a compromise
// for people in a hurry. It also transcribes as the audio arrives instead of
// waiting for a sentence to finish, so words land about a second behind the
// speaker instead of four.
//
// It was measured against the phrase model on a graphics chip and keeps up with
// it, so the second it saves costs nothing in throughput. What it costs is
// context: it can never see what is said next, which is where its accuracy goes
// against Cohere in English — and memory, because a streaming model cannot be
// shared between tuners and loads a copy for each.
//
// The engine handles it as a parakeet family model, which is worth knowing
// because that is the name that appears in the log when it picks a backend.
//
// Delete this file and two references break, both named by the compiler: its
// line in captionModelCatalog and its case in quirksFor.

var nemotronStreaming = captionModel{
	Key:  "nemotron-streaming",
	Name: "Nemotron 3.5 ASR Streaming 0.6B",
	Role: "Best for low latency",
	Desc: "Written as the audio arrives rather than a phrase at a time, so words appear about a second behind the speaker instead of four. Reads thirty-two languages, which is more than anything else here.",
	// Streaming is a different shape of model, not a faster one: it keeps a
	// running encoder state and commits words as they settle, so latency is
	// a property of the architecture rather than a setting. The phrase
	// window does not apply to it.
	Latency:   "About a second behind the picture",
	Accuracy:  "Very good",
	Benchmark: "Between the other two in English",
	// The memory is the thing to know about this one, and it is not the
	// download.
	//
	// A phrase model is loaded once and shared: ten tuners captioning at
	// once put one copy of Cohere in memory between them. A streaming model
	// cannot be shared, because the running state that makes it streaming
	// belongs to one stream — so every captioned tuner loads its own copy,
	// and the memory is the size of the model times the number of tuners
	// being captioned. Three streams is comfortable on most machines. Ten
	// is seven gigabytes and worth deciding on deliberately.
	Hardware:    "About 500 MB of memory for every tuner being captioned, because a streaming model cannot be shared between them. Comfortable for a few streams; worth adding up before running many.",
	Runtime:     rtTranscribe,
	Repo:        "handy-computer/nemotron-3.5-asr-streaming-0.6b-gguf",
	File:        "nemotron-3.5-asr-streaming-0.6b-Q4_K_M.gguf",
	SizeMB:      496,
	Streaming:   true,
	Punctuation: true,
	// Q4_K_M, and the reason is the graphics chip rather than the model.
	//
	// Accuracy barely moves across this model's quantizations — its own
	// table runs from 7.97% at full precision to 8.49% at Q4_K_M, half a
	// point across the whole range — so the choice is free on that side and
	// is made on the two that matter. Memory, because this one is loaded
	// per stream and four hundred and ninety-six megabytes against seven
	// hundred and fifty is two and a half gigabytes across ten tuners. And
	// the GPU, where quantization is not free at all: the shaders differ
	// per quant, which is how Cohere came to ship Q4_K_M after its author
	// measured it at eight times real time on integrated graphics where
	// Q5_K_M managed a third of that.
	//
	// Nobody has published that measurement for this model — its table is
	// accuracy only, and its throughput figures are from an H100, which
	// says nothing about a UHD 770. So this is the same quant on the same
	// engine on the same class of chip, which is the nearest thing to
	// evidence available, and it is worth checking rather than trusting:
	// the log prints the real-time factor every twenty-five dispatches.
	// Locales, not bare language codes, because that is what this family
	// accepts: handed "en" it answers "unsupported language" and refuses,
	// on the streaming path and the phrase path alike. Its own example
	// passes en-US. modelLanguage widens a bare code onto the first locale
	// that starts with it, so a configuration saved before this still
	// works and lands on en-US.
	//
	// These are the thirty-two the card calls production ready. The eight
	// it lists as adaptation ready are left out: they need fine-tuning
	// first, and offering a language that transcribes badly is worse than
	// not offering it.
	// Locales, not bare language codes, because that is what this family
	// accepts. Handed "en" it answers "unsupported language" and refuses, on
	// the streaming path and the phrase path alike — which is exactly what it
	// did the first time it ran here. Its own example passes en-US.
	//
	// modelLanguage widens a bare code onto the first locale beginning with
	// it, so a setting saved against another model still works and lands on
	// en-US rather than failing.
	//
	// These are the thirty-two the card calls production ready. The eight it
	// lists as adaptation ready are left out: they want fine-tuning first, and
	// offering a language that transcribes badly is worse than not offering it.
	Languages: []string{"auto",
		"en-US", "en-GB", "es-US", "es-ES", "fr-FR", "fr-CA", "it-IT", "pt-BR", "pt-PT",
		"nl-NL", "de-DE", "tr-TR", "ru-RU", "ar-AR", "hi-IN", "ja-JP", "ko-KR", "vi-VN",
		"uk-UA", "pl-PL", "sv-SE", "cs-CZ", "nb-NO", "da-DK", "bg-BG", "fi-FI", "hr-HR",
		"sk-SK", "zh-CN", "hu-HU", "ro-RO", "et-EE"},
}

// It asks for nothing beyond the defaults. Being a streaming model, the phrase
// window does not apply to it — it keeps a running state rather than waiting
// for a phrase — and it has not shown the hallucination on silence that earns a
// noise gate. If it does, this is where that would go.
var nemotronQuirks = modelQuirks{
	PhraseWindow: modelDefaults.PhraseWindow,
	// 1040 ms a feed, which is this family's own figure and not a guess.
	//
	// It is the lookahead the model is run at by default — the engine offers
	// 0, 80, 480 and 1040 ms and ships 13 frames, the last of them — and it is
	// the chunk the engine's own accuracy measurement for this model uses:
	// --stream-chunk-ms 1040 --stream-att-right 13. Feeding less does not
	// produce a word sooner, because the model will not commit one ahead of its
	// lookahead; it only runs the same encoder pass more often.
	StreamChunkSec: 1.04,
}

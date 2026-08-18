package main

// Nemotron Speech Streaming EN 0.6B: the model, and everything it asks of the
// code around it.
//
// The same architecture as the multilingual one beside it — a cache-aware
// streaming FastConformer with an RNN-T decoder — trained on English alone and
// shipped here at Q8_0 rather than Q4_K_M. Doing one language and doing it at
// full weight is the whole of the difference, and between them they buy 2.31%
// of words wrong against 3.28%.
//
// That is the most accurate thing in the catalog that writes as it listens. It
// is behind Cohere and it is a second behind the speaker rather than four,
// which is a trade nothing else here offers.
//
// It is also the most expensive thing in the catalog to run, and not because of
// the download. A streaming model cannot be shared between tuners — the running
// state that makes it streaming belongs to one stream — so every captioned
// tuner loads its own copy. Seven hundred megabytes each. Three tuners is most
// of three gigabytes and six is most of six, which is why this one says
// high-end and means it.
//
// Delete this file and two references break, both named by the compiler: its
// line in captionModelCatalog and its case in quirksFor.

var nemotronEnglish = captionModel{
	Key:  "nemotron-en",
	Name: "Nemotron Speech Streaming EN 0.6B",
	Role: "Best for high-end systems",
	Desc: "English only, written as the audio arrives, and the most accurate of the models that do not wait for the sentence to finish. Wants a machine with memory to spare: it loads a copy for every tuner being captioned.",
	// Streaming is a different shape of model, not a faster one: it keeps a
	// running encoder state and commits words as they settle, so latency is a
	// property of the architecture rather than a setting. The phrase window
	// does not apply to it.
	Latency:   "About a second behind the picture",
	Accuracy:  "Excellent",
	Benchmark: "2.3% of words come out wrong",
	Hardware:  "About 900 MB of memory for every tuner being captioned, because a streaming model cannot be shared between them. Three tuners is around three gigabytes. Check the total before running many.",
	Runtime:   rtTranscribe,
	Repo:      "handy-computer/nemotron-speech-streaming-en-0.6b-gguf",
	// Q8_0, which is the point of this entry.
	//
	// The multilingual one ships Q4_K_M because it is loaded per stream and the
	// memory across several tuners is the binding constraint. This model exists
	// for the machine where that constraint does not bind, so it spends the
	// memory on the accuracy: 2.31% at Q8_0 against 2.38% at Q4_K_M, and 696 MB
	// against 453 MB. Somebody who wants the smaller file already has the other
	// one.
	File:        "nemotron-speech-streaming-en-0.6b-Q8_0.gguf",
	SizeMB:      696,
	Streaming:   true,
	Punctuation: true,
	// No language is passed at all, and that is not the same answer its
	// multilingual sibling gives.
	//
	// That model documents the parameter and its own example passes en-US, so
	// this entry was written to match it — reasoning about the family rather
	// than reading the model. This one's card never mentions a language: it
	// does English and there is nothing to select, so the parameter it was
	// being handed came back "unsupported language" on the continuous path,
	// again on the phrase path it fell back to, and then on every phrase after
	// that. Captions did not work at all.
	//
	// The list below is for the page and reaches the engine nowhere.
	NoLanguage: true,
	Languages:  []string{"en"},
}

// Nothing beyond the defaults. It streams, so the phrase window does not apply,
// and it has not shown the hallucination on silence that earns a noise gate.
var nemotronEnglishQuirks = modelQuirks{
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

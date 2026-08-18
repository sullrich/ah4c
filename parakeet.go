package main

// Parakeet TDT-CTC 110M: the model, and everything it asks of the code around
// it.
//
// The small one. A hundred and ten million parameters against Cohere's billions,
// and it runs on hardware that cannot hold the others — a Celeron, a low-power
// NAS, a Raspberry Pi class board.
//
// What makes it suit those machines is not the file size. It is a FastConformer
// encoder with a transducer head, so a phrase is one pass forward and out;
// there is no autoregressive loop generating a token at a time and no budget
// for that loop to run out of. The model it replaces was a fifth of the size
// and worse on both counts: twice the word error rate, and a generation budget
// so short that any sentence over about seven seconds came back cut off
// mid-word, with the engine saying so in the log.
//
// English only, and offline — it transcribes a phrase at a time rather than as
// it listens, which is the right shape for a machine that cannot promise to
// keep up in real time anyway.
//
// Delete this file and two references break, both named by the compiler: its
// line in captionModelCatalog and its case in quirksFor.

var parakeetTDT = captionModel{
	Key:  "parakeet-tdt-110m",
	Name: "Parakeet TDT-CTC 110M",
	Role: "Best for small systems",
	Desc: "A hundred and thirty-five megabytes, English only, and accurate for its size. For machines too small to run the others.",
	// A phrase model, so it cannot answer before the phrase is complete.
	Latency:   "A couple of seconds behind",
	Accuracy:  "Very good",
	Benchmark: "2.4% of words come out wrong",
	Hardware:  "Runs on almost anything, and shares one copy of itself across every tuner being captioned.",
	Runtime:   rtTranscribe,
	Repo:      "handy-computer/parakeet-tdt_ctc-110m-gguf",
	// Q8_0, on a processor and on a graphics chip alike.
	//
	// Its accuracy does not move above the K-quants: 2.43% at F32, 2.43% at
	// Q8_0, 2.44% at Q6_K and 2.53% by Q4_K_M. Forty-five megabytes is the
	// whole of what dropping to Q4_K_M saves, and this is the model chosen for
	// machines where the constraint is the processor rather than the disk.
	// Cohere is quantized harder for the opposite reason: it is large enough
	// that the memory matters.
	//
	// And a K preset is partly Q8_0 anyway. The engine quantizes a tensor to
	// the K type only where its dimensions divide the block size, and falls
	// back to Q8_0 where they do not — so Q6_K and below are a mixture, and
	// Q8_0 is the one file that is what it says it is.
	//
	//	https://github.com/handy-computer/transcribe.cpp/blob/main/docs/tools/quantization.md
	File:        "parakeet-tdt_ctc-110m-Q8_0.gguf",
	SizeMB:      135,
	Streaming:   false,
	Punctuation: true,
	// The card is explicit that it outputs cased, punctuated transcripts, and
	// there is no language parameter: English is the only thing it does, so
	// passing a code can only be refused. The list is for the page.
	NoLanguage: true,
	Languages:  []string{"en"},
}

// Nothing beyond the defaults.
//
// No noise gate: that exists for the one model whose own card warns it will
// transcribe non-speech, and nothing in this one's says so. No phrase length
// choice either — the window is the accuracy-against-delay trade, and it is
// offered on the model that is chosen for its accuracy rather than on the one
// chosen for fitting.
var parakeetQuirks = modelDefaults

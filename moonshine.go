package main

// Moonshine Streaming Tiny: the model, and everything it asks of the code
// around it.
//
// The small one. Forty-eight megabytes, streams live, and runs on hardware that
// cannot hold either of the others — a Celeron, a low-power NAS, a Raspberry Pi
// class board. It is the least accurate of the three and it is the only one
// some machines can run at all, which is the whole of its case.
//
// Delete this file and two references break, both named by the compiler: its
// line in captionModelCatalog and its case in quirksFor.

var moonshineTiny = captionModel{
	Key:         "moonshine-tiny",
	Name:        "Moonshine Streaming Tiny",
	Role:        "Best for small systems",
	Desc:        "A forty-eight megabyte model that streams live and runs comfortably on a Celeron, a low-power NAS or a Raspberry Pi class board. English only. Less accurate than the big model, and the only one that fits hardware this small.",
	Latency:     "Under a second",
	Accuracy:    "Decent",
	Benchmark:   "4.5% of words come out wrong",
	Hardware:    "Runs on almost anything.",
	Runtime:     rtTranscribe,
	Repo:        "handy-computer/moonshine-streaming-tiny-gguf",
	File:        "moonshine-streaming-tiny-Q8_0.gguf",
	SizeMB:      48,
	Streaming:   true,
	Punctuation: true,
	// English is this family's only language and the engine's default;
	// no parameter is passed. The list is for the page only.
	NoLanguage: true,
	Languages:  []string{"en"},
}

// Nothing beyond the defaults. It streams, so the phrase window does not apply,
// and being this small it has neither the appetite for silence that earns a
// noise gate nor the training data that produces stock phrases.
var moonshineQuirks = modelDefaults

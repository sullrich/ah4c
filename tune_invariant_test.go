package main

// The tune invariant: closed captions must never delay a tune. Wrapping a
// stream and feeding it bytes happen on the tune path, so both must be
// instant, and the first bytes must not start any heavy work. This test is
// deliberately part of the repository rather than scratch: a change that
// puts weight back on the tune path should fail here before it fails a
// recording.

import (
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func TestTunePathIsInstant(t *testing.T) {
	// A configured, "downloaded" model: the wrap must engage, and still cost
	// nothing. The file only has to exist; nothing below loads it.
	m, ok := findCaptionModel("cohere-transcribe")
	if !ok {
		t.Fatal("catalog has no cohere-transcribe")
	}
	if err := os.MkdirAll(captionModels, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := modelPath(m)
	if _, err := os.Stat(fake); err != nil {
		if err := os.WriteFile(fake, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(fake)
	}

	cfg := currentCaptionConfig()
	cfg.Enabled = true
	cfg.Model = m.Key
	captionCfgLock.Lock()
	old := captionCfg
	captionCfg = cfg
	captionCfgLock.Unlock()
	defer func() {
		captionCfgLock.Lock()
		captionCfg = old
		captionCfgLock.Unlock()
	}()

	engine, err := newCaptionEngine(cfg, m, "invariant-test")
	if err != nil {
		t.Fatalf("newCaptionEngine: %v", err)
	}
	defer engine.Close()

	// Creating the engine and feeding the first stream bytes are on the tune
	// path. Generous bound: they must be effectively free. If this ever takes
	// a meaningful fraction of a second, something heavy moved onto the path.
	began := time.Now()
	buf := make([]byte, 64*1024)
	for i := 0; i < 50; i++ {
		engine.feed(buf)
	}
	if d := time.Since(began); d > 100*time.Millisecond {
		t.Fatalf("50 feeds took %s; the tune path is carrying weight", d)
	}

	// The first bytes must not have begun the heavy start: that waits out
	// captionSettle, precisely so it cannot land inside the tune window.
	if atomic.LoadInt64(&engine.begun) != 0 {
		t.Fatal("heavy start began on the first bytes; it must wait for the stream to settle")
	}
	if captionSettle < 5*time.Second {
		t.Fatalf("captionSettle is %s; the settle must cover the tune window", captionSettle)
	}

	// And a tune anywhere on the machine must hold the heavy work off:
	// tuneQuiet has to say "not yet" while a tune is pending, and again
	// through the settled grace once its video flows.
	captionTuneStarting()
	if quiet, _ := tuneQuiet(); quiet {
		t.Fatal("tuneQuiet ignored a pending tune")
	}
	captionTuneSettled()
	if quiet, _ := tuneQuiet(); quiet {
		t.Fatal("tuneQuiet skipped the settled grace")
	}
}

func TestWrapWithMissingPiecesIsInstant(t *testing.T) {
	// With captions on but the model file absent, the wrap must decline
	// instantly — never probe, never download, never wait.
	cfg := currentCaptionConfig()
	cfg.Enabled = true
	cfg.Model = "nemotron-realtime"
	captionCfgLock.Lock()
	old := captionCfg
	captionCfg = cfg
	captionCfgLock.Unlock()
	defer func() {
		captionCfgLock.Lock()
		captionCfg = old
		captionCfgLock.Unlock()
	}()
	if m, _ := findCaptionModel(cfg.Model); modelInstalled(m) {
		t.Skip("model unexpectedly present; nothing to prove here")
	}
	src := os.Stdin
	began := time.Now()
	out := maybeWrapCaptions(nopReadCloser{src}, 0, "invariant-test")
	if d := time.Since(began); d > 50*time.Millisecond {
		t.Fatalf("maybeWrapCaptions took %s with a missing model", d)
	}
	if _, wrapped := out.(*captionStream); wrapped {
		t.Fatal("wrapped a stream for a model that is not on disk")
	}
}

type nopReadCloser struct{ *os.File }

func (nopReadCloser) Close() error { return nil }

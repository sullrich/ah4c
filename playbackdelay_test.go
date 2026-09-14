package main

import (
	"bytes"
	"io"
	"testing"
	"time"
)

func TestTimedGateFlushesWrappedQueueBeforeRelease(t *testing.T) {
	flushes := 0
	src := &armFlushReader{
		ReadCloser: io.NopCloser(bytes.NewReader(nil)),
		flushSource: func(string) {
			flushes++
		},
	}
	g := newGateReader(src, nil, true, time.Now().Add(-time.Second), nil)
	g.vid = map[int]bool{0x100: true}

	keyframe := make([]byte, tsPacketSize)
	keyframe[0] = 0x47
	keyframe[1] = 0x01
	keyframe[2] = 0x00
	keyframe[3] = 0x20 // adaptation field only
	keyframe[4] = 1
	keyframe[5] = 0x40 // random_access_indicator

	if used := g.scan(keyframe); used != len(keyframe) {
		t.Fatalf("first scan consumed %d bytes, want %d", used, len(keyframe))
	}
	if flushes != 1 {
		t.Fatalf("queue flushed %d times, want 1", flushes)
	}
	if g.open {
		t.Fatal("gate released the pre-flush keyframe")
	}

	g.scan(keyframe)
	if flushes != 1 {
		t.Fatalf("queue flushed %d times after the next scan, want 1", flushes)
	}
	if !g.open {
		t.Fatal("gate did not release the first keyframe after the flush")
	}
}

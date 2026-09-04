package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Synthetic transport packets exercise the gate's packet selection without
// depending on an encoder, ADB device, or wall-clock sleeps for video windows.
func gateTestPacket(pid int, keyframe bool, marker byte) []byte {
	p := bytes.Repeat([]byte{marker}, 188)
	p[0], p[1], p[2], p[3] = 0x47, byte(pid>>8)&0x1f, byte(pid), 0x30
	p[4], p[5] = 1, 0
	if keyframe {
		p[5] = 0x40
	}
	return p
}

func gateTestArmed() *gateReader {
	ready := make(chan struct{})
	close(ready)
	g := newGateReader(io.NopCloser(bytes.NewReader(nil)), ready, false, time.Time{}, ready)
	g.vid = map[int]bool{256: true}
	g.startHunt(time.Now())
	return g
}

func TestPlaybackGateConfirmedQuietScene(t *testing.T) {
	g := gateTestArmed()
	g.recordWindow(2000)
	g.playbackConfirmed()
	// Neither an ordinary video packet nor an audio random-access flag may
	// release the stream. The first video random-access packet must do so.
	g.scan(gateTestPacket(256, false, 0x11))
	g.scan(gateTestPacket(257, true, 0x22))
	if g.open {
		t.Fatal("released before a video keyframe")
	}
	key := gateTestPacket(256, true, 0x33)
	g.scan(key)
	if !g.open || !bytes.Equal(g.pend, key) {
		t.Fatal("confirmed quiet scene did not release exactly at the video keyframe")
	}
}

func TestPlaybackGateUnconfirmedQuietSceneStaysClosed(t *testing.T) {
	g := gateTestArmed()
	g.recordWindow(2000)
	g.scan(gateTestPacket(256, true, 0x33))
	if g.open {
		t.Fatal("unconfirmed quiet scene bypassed fallback checks")
	}
}

func TestPlaybackGateRemembersMotionUntilKeyframe(t *testing.T) {
	g := gateTestArmed()
	g.recordWindow(2000)
	g.recordWindow(10000)
	g.recordWindow(2000)
	key := gateTestPacket(256, true, 0x33)
	g.scan(key)
	if !g.open || !bytes.Equal(g.pend, key) {
		t.Fatal("lost motion evidence when the window became quiet before the keyframe")
	}
}

func TestPlaybackGateReconnectClearsMotion(t *testing.T) {
	g := gateTestArmed()
	g.recordWindow(2000)
	g.recordWindow(10000)
	ceilingStart := g.armed0
	g.resync()
	g.vid = map[int]bool{256: true}
	g.scan(gateTestPacket(256, true, 0x33))
	if g.open {
		t.Fatal("motion from the previous encoder session released the new session")
	}
	if g.armed0 != ceilingStart {
		t.Fatal("reconnect reset the overall keyframe deadline")
	}
	g.recordWindow(2000)
	g.recordWindow(10000)
	g.scan(gateTestPacket(256, true, 0x44))
	if !g.open {
		t.Fatal("new motion after reconnect failed to release")
	}
}

func TestPlaybackGateConfirmationSurvivesEncoderReconnect(t *testing.T) {
	g := gateTestArmed()
	g.playbackConfirmed()
	g.resync()
	g.vid = map[int]bool{256: true}
	g.scan(gateTestPacket(256, true, 0x33))
	if !g.open {
		t.Fatal("encoder reconnect discarded the device's playback confirmation")
	}
}

func TestPlaybackGateDoesNotLatchMotionBeforeArming(t *testing.T) {
	g := &gateReader{}
	g.recordWindow(2000)
	g.recordWindow(10000)
	g.startHunt(time.Now())
	g.recordWindow(2000)
	if got := g.releaseKind(); got != "" {
		t.Fatalf("pre-tune motion released gate as %q", got)
	}
}

func TestPlaybackGateUniformFallback(t *testing.T) {
	for _, pendingSwap := range []bool{false, true} {
		t.Run(fmt.Sprintf("pendingSwap=%v", pendingSwap), func(t *testing.T) {
			g := gateTestArmed()
			g.armedAt = time.Now().Add(-2 * riseWait)
			g.recordWindow(busyWindow)
			if pendingSwap {
				g.sess = &stallTolerantReader{}
				g.expectNewStream()
			}
			g.scan(gateTestPacket(256, true, 0x33))
			if g.open == pendingSwap {
				t.Fatalf("open=%v with pendingSwap=%v", g.open, pendingSwap)
			}
		})
	}
}

func TestPlaybackGateDiscardsBufferAtConfirmation(t *testing.T) {
	ready := make(chan struct{})
	g := newGateReader(io.NopCloser(bytes.NewReader(nil)), ready, false, time.Time{}, ready)
	g.vid = map[int]bool{256: true}
	g.playbackConfirmed()
	old := gateTestPacket(256, true, 0x11)
	g.scan(old)
	if g.open {
		t.Fatal("confirmation bypassed the readiness channel")
	}
	close(ready)
	g.scan(old)
	if g.open {
		t.Fatal("released a keyframe from the buffer being read when readiness arrived")
	}
	fresh := gateTestPacket(256, true, 0x22)
	g.scan(fresh)
	if !g.open || !bytes.Equal(g.pend, fresh) {
		t.Fatal("did not release the subsequent fresh keyframe")
	}
}

func TestPlaybackGateReadReassemblesFragmentedKeyframe(t *testing.T) {
	g := gateTestArmed()
	g.playbackConfirmed()
	key := gateTestPacket(256, true, 0x33)
	// MultiReader returns these fragments separately to exercise carry handling.
	g.src = io.NopCloser(io.MultiReader(bytes.NewReader(key[:71]), bytes.NewReader(key[71:])))
	got, err := io.ReadAll(g)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("fragmented keyframe: got %d bytes, error %v", len(got), err)
	}
}

func TestPlaybackGateTimedReleaseStillWorks(t *testing.T) {
	g := gateTestArmed()
	g.timed = true
	g.scan(gateTestPacket(256, true, 0x33))
	if !g.open {
		t.Fatal("timer-based gate now requires playback or motion confirmation")
	}
}

func TestPlaybackHeldPlayerNeedsConsecutiveObservations(t *testing.T) {
	base := map[string]bool{"old": true}
	held := map[string]int{}
	sequence := []struct {
		players map[string]bool
		want    bool
	}{
		{map[string]bool{"old": true}, false},
		{map[string]bool{"new": true}, false},
		{map[string]bool{"old": true}, false},
		{map[string]bool{"new": true}, false},
		{map[string]bool{"new": true}, true},
	}
	for i, step := range sequence {
		if got := heldNewID(base, step.players, held); got != step.want {
			t.Fatalf("observation %d: got %v, want %v", i+1, got, step.want)
		}
	}
}

// Run the real waitForPlayback subprocess path against deterministic ADB
// responses. No commands reach a real device. Exhausted responses fail ADB.
func playbackTestADB(t *testing.T, responses []string) func() int {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PLAYBACK_STATIC_TIMEOUT", "")
	t.Setenv("PLAYBACK_TEST_ADB_DIR", dir)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	script := "#!/bin/sh\nn=0\n[ ! -f \"$PLAYBACK_TEST_ADB_DIR/count\" ] || n=$(cat \"$PLAYBACK_TEST_ADB_DIR/count\")\nn=$((n+1))\necho \"$n\" > \"$PLAYBACK_TEST_ADB_DIR/count\"\ncase $n in\n"
	for i, response := range responses {
		name := strconv.Itoa(i + 1)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(response), 0600); err != nil {
			t.Fatal(err)
		}
		script += fmt.Sprintf("%d) cat \"$PLAYBACK_TEST_ADB_DIR/%s\";;\n", i+1, name)
	}
	script += "*) exit 1;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "adb"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return func() int {
		data, _ := os.ReadFile(filepath.Join(dir, "count"))
		n, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		return n
	}
}

func TestPlaybackWaitConfirmsNewPlayer(t *testing.T) {
	old := "piid:1 state:started usage=USAGE_MEDIA\n"
	newPlayer := "piid:2 state:started usage=USAGE_MEDIA\n"
	calls := playbackTestADB(t, []string{old, newPlayer, newPlayer})
	swap, confirmed := waitForPlayback("test-device", map[string]bool{"1": true}, "", make(chan struct{}))
	if swap || !confirmed || calls() != 3 {
		t.Fatalf("swap=%v confirmed=%v calls=%d; want false, true, 3", swap, confirmed, calls())
	}
}

func TestPlaybackWaitFailuresDoNotConfirm(t *testing.T) {
	for _, transientPlayer := range []bool{false, true} {
		t.Run(fmt.Sprintf("transientPlayer=%v", transientPlayer), func(t *testing.T) {
			var responses []string
			if transientPlayer {
				responses = []string{"piid:2 state:started usage=USAGE_MEDIA\n"}
			}
			calls := playbackTestADB(t, responses)
			swap, confirmed := waitForPlayback("test-device", map[string]bool{"1": true}, "", make(chan struct{}))
			if swap || confirmed || calls() != len(responses)+adbGiveUp {
				t.Fatalf("unexpected success or retry count: swap=%v confirmed=%v calls=%d", swap, confirmed, calls())
			}
		})
	}
}

func TestPlaybackWaitCancellationDoesNotConfirm(t *testing.T) {
	calls := playbackTestADB(t, nil)
	done := make(chan struct{})
	close(done)
	swap, confirmed := waitForPlayback("test-device", nil, "", done)
	if swap || confirmed || calls() != 0 {
		t.Fatalf("canceled wait: swap=%v confirmed=%v calls=%d", swap, confirmed, calls())
	}
}

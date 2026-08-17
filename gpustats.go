package main

import (
	"bufio"
	"encoding/json"
	"os/exec"
	"strconv"
	"sync"
	"time"
)

// Intel engine busyness, from intel_gpu_top.
//
// i915 does not publish a utilisation figure in sysfs the way amdgpu does —
// engine busyness comes from the driver's PMU, and intel_gpu_top is the thing
// that reads it. So on an Intel chip that is the only honest source, and
// without it the GPU chart has nothing to draw whatever else is tried.
//
// It is sampled by one long-lived process rather than a command per poll. The
// tool measures a period rather than reading a counter, so a one-shot
// invocation has to wait out its own sample before it can answer — half a
// second or more of a request handler blocked on a subprocess, several times a
// minute, for a graph. A process that streams samples and a value kept in
// memory costs one process and answers instantly.
//
// If the tool is not installed nothing starts and nothing is reported, which is
// the same as it was before. It is not a dependency.
var (
	intelOnce  sync.Once
	intelMu    sync.Mutex
	intelBusy  float64
	intelPower float64
	intelSeen  time.Time
)

// intelSample is the shape read from intel_gpu_top -J. Only the two fields
// wanted are named; everything else in the object is ignored, so a version that
// adds to it does not break this.
type intelSample struct {
	Power struct {
		GPU float64 `json:"GPU"`
	} `json:"power"`
	Engines map[string]struct {
		Busy float64 `json:"busy"`
	} `json:"engines"`
}

// intelGPU reports engine busyness and GPU watts, or false when there is no
// reading. Starts the sampler on the first call.
func intelGPU() (busy, power float64, ok bool) {
	intelOnce.Do(func() {
		if _, err := exec.LookPath("intel_gpu_top"); err != nil {
			return
		}
		go sampleIntelGPU()
	})
	intelMu.Lock()
	defer intelMu.Unlock()
	// A reading nobody has updated for a minute is a sampler that died or a
	// chip that went away. Stale is worse than absent on a graph.
	if intelSeen.IsZero() || time.Since(intelSeen) > time.Minute {
		return 0, 0, false
	}
	return intelBusy, intelPower, true
}

// sampleIntelGPU runs the tool and keeps the latest sample. It restarts if the
// process dies, with a pause so a tool that refuses to run does not become a
// loop spawning processes.
func sampleIntelGPU() {
	for {
		readIntelGPU()
		time.Sleep(30 * time.Second)
	}
}

func readIntelGPU() {
	cmd := exec.Command("intel_gpu_top", "-J", "-s", "1000")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	if err := cmd.Start(); err != nil {
		return
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	// The tool emits a stream of JSON objects rather than one document, so it
	// is decoded as a stream. A leading "[" and the commas between objects are
	// not valid there, and the decoder reports them as errors it recovers from
	// on the next token; the loop simply keeps reading.
	dec := json.NewDecoder(bufio.NewReader(out))
	for {
		var s intelSample
		if err := dec.Decode(&s); err != nil {
			if dec.More() {
				continue
			}
			return
		}
		busy := 0.0
		for _, e := range s.Engines {
			// The busiest engine, not the sum. Four engines each at 50% is a
			// chip half used, not one at two hundred percent.
			if e.Busy > busy {
				busy = e.Busy
			}
		}
		intelMu.Lock()
		intelBusy, intelPower, intelSeen = busy, s.Power.GPU, time.Now()
		intelMu.Unlock()
	}
}

// intelGPUStrings is what the status handler wants: the same two figures as
// strings, empty when there is no reading, so it can fall through to whatever
// else it has.
func intelGPUStrings() (busy, power string) {
	b, p, ok := intelGPU()
	if !ok {
		return "", ""
	}
	busy = strconv.FormatFloat(b, 'f', 2, 64)
	if p > 0 {
		power = strconv.FormatFloat(p, 'f', 2, 64)
	}
	return busy, power
}

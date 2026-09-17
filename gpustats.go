package main

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// otherGPU is everything that is not NVIDIA: AMD through sysfs, Intel through
// intel_gpu_top. Returns utilisation, memory and power as percentages, empty
// where the hardware does not publish the figure.
//
// Three vendors, three different answers about what is even knowable.
//
// AMD publishes all three in sysfs and needs nothing installed: gpu_busy_percent
// for the engine, mem_info_vram_used against _total for memory, and hwmon's
// power1_average against power1_cap for power.
//
// Intel publishes utilisation only, and only through intel_gpu_top. There is no
// memory figure because an integrated chip has no memory of its own — it is
// using system RAM, which the page already shows — and no power percentage
// because the tool reports watts and there is no cap to divide by. Reporting
// watts in a field labelled percent would be worse than the gap.
//
// NVIDIA is handled by the caller, which asks nvidia-smi first and only falls
// through to here when that is absent.
func otherGPU() (util, mem, power string) {
	util, mem, power = amdSysfs()
	if util == "" {
		util = drmGPUString()
	}
	if util == "" {
		util, _ = intelGPUStrings()
	}
	return util, mem, power
}

func amdSysfs() (util, mem, power string) {
	read := func(p string) (float64, bool) {
		b, err := os.ReadFile(p)
		if err != nil {
			return 0, false
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
		return v, err == nil
	}
	pct := func(used, total float64) string {
		if total <= 0 {
			return ""
		}
		return strconv.FormatFloat(used/total*100, 'f', 2, 64)
	}
	cards, _ := filepath.Glob("/sys/class/drm/card*/device")
	for _, d := range cards {
		if util == "" {
			if v, ok := read(d + "/gpu_busy_percent"); ok {
				util = strconv.FormatFloat(v, 'f', 2, 64)
			}
		}
		if mem == "" {
			u, ok1 := read(d + "/mem_info_vram_used")
			t, ok2 := read(d + "/mem_info_vram_total")
			if ok1 && ok2 {
				mem = pct(u, t)
			}
		}
		if power == "" {
			hwmons, _ := filepath.Glob(d + "/hwmon/hwmon*")
			for _, h := range hwmons {
				// Microwatts in both, so the units cancel.
				a, ok1 := read(h + "/power1_average")
				c, ok2 := read(h + "/power1_cap")
				if ok1 && ok2 {
					power = pct(a, c)
					break
				}
			}
		}
	}
	return util, mem, power
}

// GPU busyness from DRM fdinfo, which needs nothing but /dev/dri.
//
// The kernel accounts GPU time per client on the file descriptor itself. Every
// process holding a DRM fd has, in /proc/<pid>/fdinfo/<fd>, a line per engine
// with the nanoseconds that client has been busy on it:
//
//	drm-engine-render:  12345678 ns
//	drm-engine-copy:    0 ns
//
// Two reads a second apart give the fraction of that second the engine was
// working, which is what a utilisation figure is. It is our own fds, so it
// needs no permission beyond having opened them, and the device node is already
// passed through or captioning would not run at all.
//
// This is the same accounting intel_gpu_top presents, reached without the perf
// interface it uses. That matters because perf_event_open is on Docker's
// default seccomp deny list, so the tool fails inside a container even when it
// is installed — and it is not installed. The interface is common to i915 and
// amdgpu, so one reader serves both.
//
// What it measures is this program's share rather than the whole chip. For a
// graph of what captioning costs, that is the more useful of the two anyway.
var (
	fdinfoMu   sync.Mutex
	fdinfoBusy float64
	fdinfoSeen time.Time
	fdinfoOnce sync.Once
)

// drmBusy reads the engine totals across every DRM fd this process holds.
func drmBusy() (map[string]float64, bool) {
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return nil, false
	}
	out := map[string]float64{}
	found := false
	for _, fd := range fds {
		target, err := os.Readlink("/proc/self/fd/" + fd.Name())
		if err != nil || !strings.HasPrefix(target, "/dev/dri/") {
			continue
		}
		b, err := os.ReadFile("/proc/self/fdinfo/" + fd.Name())
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			name, rest, ok := strings.Cut(line, ":")
			if !ok || !strings.HasPrefix(name, "drm-engine-") {
				continue
			}
			v, err := strconv.ParseFloat(strings.Fields(strings.TrimSpace(rest))[0], 64)
			if err != nil {
				continue
			}
			out[strings.TrimPrefix(name, "drm-engine-")] += v
			found = true
		}
	}
	return out, found
}

// sampleDRM keeps a utilisation figure from two reads a second apart.
func sampleDRM() {
	prev, ok := drmBusy()
	last := time.Now()
	for {
		time.Sleep(time.Second)
		now, ok2 := drmBusy()
		at := time.Now()
		if ok && ok2 {
			elapsed := float64(at.Sub(last).Nanoseconds())
			busy := 0.0
			for name, v := range now {
				// The busiest engine, not the sum: four engines at half is a
				// chip half used, not one at two hundred percent.
				if d := v - prev[name]; d > busy {
					busy = d
				}
			}
			if elapsed > 0 {
				pct := busy / elapsed * 100
				if pct > 100 {
					pct = 100
				}
				fdinfoMu.Lock()
				fdinfoBusy, fdinfoSeen = pct, at
				fdinfoMu.Unlock()
			}
		}
		prev, ok, last = now, ok2, at
	}
}

// drmGPUString is the utilisation figure, empty when there is none.
func drmGPUString() string {
	// Started unconditionally, not only when a DRM fd already exists.
	//
	// The device is opened when the caption engine loads its model, which is
	// well after the first time anybody can open the status page. Gating the
	// sampler on fds being present at the first call means a page opened early
	// consumes the once, finds nothing, and never looks again — the same stale
	// answer that made the driver install report a loader it had just
	// installed as missing.
	//
	// It costs a directory read a second on a machine with no GPU, and it
	// starts reporting by itself the moment one appears.
	fdinfoOnce.Do(func() { go sampleDRM() })
	fdinfoMu.Lock()
	defer fdinfoMu.Unlock()
	if fdinfoSeen.IsZero() || time.Since(fdinfoSeen) > time.Minute {
		return ""
	}
	return strconv.FormatFloat(fdinfoBusy, 'f', 2, 64)
}

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
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		return
	}
	defer noteIntelFailure(&errBuf)
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

// noteIntelFailure explains the one failure that is not ours to fix, once.
//
// Reading Intel engine busyness means reading the i915 PMU, and that means the
// perf_event_open syscall, which Docker's default seccomp profile refuses. So
// the tool fails the same way whether or not it is installed, and the page shows
// nothing either way, which is indistinguishable from a chip that reports
// nothing at all.
//
// It is a compose file change and nobody would guess it, so it is said plainly
// and said once. Everything else here degrades quietly on purpose; this one
// cannot be fixed by anything in this program.
func noteIntelFailure(errBuf *strings.Builder) {
	msg := errBuf.String()
	if !strings.Contains(msg, "PMU") && !strings.Contains(msg, "Permission denied") {
		return
	}
	intelToldOnce.Do(func() {
		logger("[STATS] Intel GPU statistics need the perf interface, which this container is not allowed to use: %s", strings.TrimSpace(msg))
		logger("[STATS] Add cap_add: [PERFMON] to the compose file, and set kernel.perf_event_paranoid to 2 or lower on the host. Captioning on the GPU is unaffected either way; this is only the graph.")
	})
}

var intelToldOnce sync.Once

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

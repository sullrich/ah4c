package main

// The HTTP surface behind the Closed Captions page.
//
// Everything the page shows and everything it can change. Nothing here decides
// anything; it reports what the rest of the package has decided and writes what
// the user picked.

import (
	"fmt"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strconv"
	"strings"
)

// HTTP surface
// ---------------------------------------------------------------------------

// captionStatus is what the Closed Captions page renders.
type captionStatus struct {
	Config    captionConfig        `json:"config"`
	Models    []captionStatusModel `json:"models"`
	Languages map[string]string    `json:"languageNames"`
	Download  captionDownload      `json:"download"`
	// The Runtime fields describe the engine the selected model needs, which is
	// the only one that has to be downloaded for that model to work.
	Runtime       string `json:"runtime"`
	RuntimeReady  bool   `json:"runtimeReady"`
	RuntimeSizeMB int    `json:"runtimeSizeMB"`
	RuntimeName   string `json:"runtimeName"`
	// Runtimes describes each engine, keyed by engine, for the same reason
	// Engines carries both: the page talks about the engine under the radio
	// button, which is not always the saved one.
	Runtimes map[string]string `json:"runtimes"`
	// RuntimeList is every engine, in order, so the page can show both as the
	// separate programs they are rather than swapping one card's contents and
	// leaving the reader to notice the name changed.
	RuntimeList    []speechRuntime `json:"runtimeList"`
	RuntimeVersion string          `json:"runtimeVersion"`
	RuntimeURL     string          `json:"runtimeURL"`
	// Engines carries the builds of both engines, keyed by engine, so the page
	// can show what a model would need before it has been saved. Picking a
	// radio button is browsing, not a decision, and must not change what a tune
	// starting right now will do.
	Engines       map[string][]captionStatusEngine `json:"engines"`
	Drivers       []captionStatusDriver            `json:"drivers"`
	Accel         accelReport                      `json:"accel"`
	DriverInstall gpuInstallState                  `json:"driverInstall"`
	// Recognizer is the measured throughput, so the page can answer "will this
	// keep up" without anybody reading a log.
	Recognizer recognizerReport `json:"recognizer"`
	// Speeds is what the page offers for caption speed, in words a minute, and
	// OnScreen what it offers for the least time a line stays readable.
	Speeds   []int     `json:"speeds"`
	OnScreen []float64 `json:"onScreen"`
	// Streaming is how many tuners are busy, so the page can refuse to switch
	// captions on in the middle of a recording.
	Streaming      int    `json:"streaming"`
	Persistent     bool   `json:"persistent"`
	PersistWarning string `json:"persistWarning"`
	Tuners         int    `json:"tuners"`
	// MemoryWarning is spelled out when the current choice could use a lot of
	// memory, worked out for the tuners actually being captioned rather than
	// left as arithmetic for the reader.
	MemoryWarning string `json:"memoryWarning"`
}

type captionStatusDriver struct {
	gpuRuntime
	Downloaded bool `json:"downloaded"`
	Active     bool `json:"active"`
}

type captionStatusEngine struct {
	engineVariant
	Usable    bool   `json:"usable"`
	Installed bool   `json:"installed"`
	Selected  bool   `json:"selected"`
	URL       string `json:"url"`
	// File is the archive that will actually be fetched, and Shared says so
	// when another build on the list fetches the very same one. Two rows both
	// reading "28 MB" with no explanation is how someone ends up believing they
	// have to download the engine twice.
	File string `json:"file"`
	// PartOf names the engine and version this build belongs to, for the line
	// under the heading rather than in it.
	PartOf string `json:"partOf"`
	Shared string `json:"shared"`
}

type captionStatusModel struct {
	captionModel
	Installed bool `json:"installed"`
	// Engine is the engine this model runs on and EngineName the same thing
	// said in full, so the page can be honest that picking some models means a
	// second download.
	Engine      string `json:"engine"`
	EngineName  string `json:"engineName"`
	EngineReady bool   `json:"engineReady"`
	// Runnable is false when this machine cannot give the model what it needs.
	// Blocked says what is missing, in the words the page shows.
	// Recommended is worked out for this machine rather than fixed in the
	// catalog: what to use depends on whether there is a graphics card to use
	// it with, and a label that ignores that is advice for somebody else.
	Recommended bool   `json:"recommended"`
	Why         string `json:"why"`
	Runnable    bool   `json:"runnable"`
	Blocked     string `json:"blocked"`
	// Memory is what one simultaneous stream costs in RAM, and Reuse says what
	// happens to that copy when the stream ends.
	Memory string `json:"memory"`
	Reuse  string `json:"reuse"`
	// MemoryMB is one stream's cost and MemoryTotalMB the ceiling across the
	// tuners actually being captioned, worked out here so the page never asks
	// anyone to multiply anything.
	MemoryMB      int `json:"memoryMB"`
	MemoryTotalMB int `json:"memoryTotalMB"`
	// Windows is the phrase lengths this model offers and Window the one in
	// force. Empty means the page shows no choice, which is every streaming
	// model: there is no phrase to lengthen.
	Windows     []float64 `json:"windows"`
	Window      float64   `json:"window"`
	MemoryTotal string    `json:"memoryTotal"`
	URL         string    `json:"url"`
}

// memoryWarning says, in gigabytes, what the current settings could use, when
// that is enough to matter.
//
// Getting this wrong is expensive in a way the rest of the page is not: a
// machine that runs out of memory does not caption badly, it stops doing
// everything, and the number is not obvious from a model list because it
// multiplies by the tuners being captioned. So it is worked out here and said
// plainly rather than left as a sum for the reader.
func memoryWarning(cfg captionConfig) string {
	if !cfg.Enabled {
		return ""
	}
	m, ok := findCaptionModel(cfg.Model)
	if !ok {
		return ""
	}
	n := captionedStreams(cfg)
	per := streamMemoryMB(m)
	totalMB := per * n
	if runtimeOf(m) == rtTranscribe && !modelStreams(m, cfg) {
		// Shared: the total is one copy no matter how many tuners, and by the
		// same yardstick as everything else that rarely warrants a banner.
		totalMB = per
	}
	// One threshold for every model, judged on the total the current settings
	// would actually use. A shared model is judged on its one copy; a
	// per-stream model on all of its copies together — which is how a
	// middleweight on many tuners outranks a heavyweight that is shared, and
	// the warnings land where the memory actually goes.
	if totalMB < 4000 {
		return ""
	}
	if runtimeOf(m) == rtTranscribe && !modelStreams(m, cfg) {
		return fmt.Sprintf("%s keeps about %s in memory — one copy, shared by every tuner, freed when "+
			"the last stream ends.", m.Name, humanMB(totalMB))
	}
	each := humanMB(per)
	total := humanMB(totalMB)
	which := fmt.Sprintf("all %d tuners", n)
	if len(cfg.Tuners) > 0 {
		which = fmt.Sprintf("the %d tuners you have selected", n)
	}
	// Name the cause and the cheapest fix, in that order, and stop.
	//
	// Real time is what makes memory grow with the tuner count: each stream
	// keeps recognition state, so each keeps its own copy of the weights. This
	// used to spend a paragraph explaining that and then advise a smaller
	// model, which sends somebody shopping for a downgrade when the setting
	// above gives the memory back and keeps the model they picked.
	warn := fmt.Sprintf("Real time loads a copy of %s per stream — about %s each, up to %s across %s.",
		m.Name, each, total, which)
	if m.EitherMode {
		warn += " Set Transcription to a sentence at a time and one copy serves them all."
	} else {
		warn += " Caption fewer tuners below, or pick a model that waits for sentences and shares one copy."
	}
	return warn
}

// memoryNote describes what a model costs to run.
//
// No model shares one copy between two streams that are both transcribing. The
// engines decode one thing at a time per loaded copy, so sharing would make
// concurrent streams take turns, and a stream that waits its turn falls behind
// live audio and drops speech. Every simultaneous stream therefore loads its
// own copy, and every copy is freed the moment its stream ends.
func memoryNote(m captionModel, cfg captionConfig, streams int) (memory, reuse, total string) {
	per := streamMemoryMB(m)
	if runtimeOf(m) == rtTranscribe && !modelStreams(m, cfg) {
		// One copy serves every tuner on this path; see txBatchService.
		memory = "one copy shared by every stream"
		total = "about " + humanMB(per) + ", shared"
		reuse = "Freed when the last stream ends."
		return memory, reuse, total
	}
	memory = humanMB(per) + " per stream"
	if streams > 1 {
		total = fmt.Sprintf("up to %s across %d tuners", humanMB(per*streams), streams)
	} else {
		total = "about " + humanMB(per)
	}
	reuse = "Each stream has its own copy, freed the moment it ends."
	return memory, reuse, total
}

// streamMemoryMB is what one captioned stream really costs in memory.
//
// It is not the size of the file. The weights are the bulk of it, but a stream
// also needs its decoder cache, the encoder's activations and ggml's own
// working buffers, and those scale with the model rather than being a fixed
// cost — a second stream of a 2.4 GB model was measured at about three
// gigabytes resident, not 2.4. Reporting the file size
// as the memory cost understates it by roughly a quarter, and understating
// memory is how a machine gets pushed into swap by a setting that looked safe.
//
// The allowance below is fitted to that measurement rather than derived, so it
// is an estimate and is worded as one wherever it is shown. It is deliberately
// not generous in the other direction: it is better to over-warn about memory
// than to have somebody discover the truth when the box stops responding.
const (
	// streamOverhead covers buffers that grow with the model.
	streamOverhead = 1.2
	// streamWorkingMB covers the decoder cache and the fixed per-session cost.
	// A 0.6B run allocates tens of megabytes of key/value cache alone.
	streamWorkingMB = 100
)

func streamMemoryMB(m captionModel) int {
	sizeMB := m.SizeMB
	// The installed file is the truth when it is present.
	if st, err := os.Stat(modelPath(m)); err == nil && st.Size() > 0 {
		sizeMB = int(st.Size() / (1024 * 1024))
	}
	return int(float64(sizeMB)*streamOverhead) + streamWorkingMB
}

// humanMB writes a size the way a person would say it.
func humanMB(mb int) string {
	if mb >= 1024 {
		return fmt.Sprintf("%.1f GB", float64(mb)/1024)
	}
	return fmt.Sprintf("%d MB", mb)
}

// captionThreads divides the machine between the streams that may caption at
// once, rather than promising all of it to each of them.
//
// Left unset, the engine takes a sensible default for one session, which is
// most of the cores. That is right for one session and badly wrong for seven:
// seven sessions each starting twenty threads on a twenty thread machine is a
// hundred and forty threads competing for it, and the throughput lost to
// context switching swamps the work. Captions then fall behind on hardware that
// was never short of capacity — the machine was busy fighting itself.
//
// A share each, floored at one. The sum stays roughly the size of the machine
// however many tuners are captioned.
func captionThreads(cfg captionConfig) int {
	cpus := availableCPUs()
	streams := captionedStreams(cfg)
	// Rounded up, so the shares cover the machine rather than leaving cores
	// idle, and floored at two so a stream is never reduced to a single thread
	// on a machine with cores to spare. Seven tuners on a twenty thread
	// processor get three threads each: twenty-one in total, which is the size
	// of the machine, instead of the hundred and forty they were getting.
	per := (cpus + streams - 1) / streams
	if floor := 2; per < floor && cpus >= floor {
		per = floor
	}
	if per < 1 {
		per = 1
	}
	if per > cpus {
		per = cpus
	}
	return per
}

// availableCPUs is how much processor this container may actually use.
//
// runtime.NumCPU is not that number. It honors the affinity mask but knows
// nothing about a cgroup quota, so ah4c in Docker with --cpus=4 on a twenty
// thread host is told twenty, and would hand out threads on that basis while
// the kernel throttles it to four. The quota is where the real answer is, and
// it is worth reading rather than assuming: guessing high here is how a machine
// ends up thrashing, which is the fault this whole function exists to fix.
//
// Both cgroup layouts are checked. Anything unreadable or unlimited falls back
// to the affinity count, which is the best available answer on a bare host.
func availableCPUs() int {
	n := runtime.NumCPU()
	if q := cgroupCPUQuota(); q > 0 && q < n {
		return q
	}
	if n < 1 {
		return 1
	}
	return n
}

// cgroupCPUQuota reads the quota as a whole number of processors, or 0 when
// there is no limit.
func cgroupCPUQuota() int {
	// cgroup v2: "max 100000" or "400000 100000" in one file.
	if b, err := os.ReadFile("/sys/fs/cgroup/cpu.max"); err == nil {
		f := strings.Fields(string(b))
		if len(f) == 2 && f[0] != "max" {
			quota, err1 := strconv.Atoi(f[0])
			period, err2 := strconv.Atoi(f[1])
			if err1 == nil && err2 == nil && period > 0 && quota > 0 {
				return atLeastOne(quota / period)
			}
		}
		return 0
	}
	// cgroup v1: quota and period in separate files, -1 meaning unlimited.
	qb, err1 := os.ReadFile("/sys/fs/cgroup/cpu/cpu.cfs_quota_us")
	pb, err2 := os.ReadFile("/sys/fs/cgroup/cpu/cpu.cfs_period_us")
	if err1 != nil || err2 != nil {
		return 0
	}
	quota, err1 := strconv.Atoi(strings.TrimSpace(string(qb)))
	period, err2 := strconv.Atoi(strings.TrimSpace(string(pb)))
	if err1 != nil || err2 != nil || quota <= 0 || period <= 0 {
		return 0
	}
	return atLeastOne(quota / period)
}

func atLeastOne(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// captionComputeThreads is the shared recognizer's allowance: the machine's
// performance cores, one thread per physical core.
//
// More measures worse. ggml synchronizes its workers with spin
// barriers, so every thread waits for the slowest at every step: mix in
// hyperthread siblings and the barriers pay for shared execution units; mix
// in a hybrid chip's efficiency cores and every op finishes at E-core speed.
// The fast configuration was a handful of threads landing on performance
// cores — which is also what leaves the efficiency cores free for the ffmpeg
// decoders and the proxy, the things captions decorate rather than compete
// with.
func captionComputeThreads() int {
	n := performanceCores()
	if reserve := availableCPUs() / 4; n > availableCPUs()-reserve {
		n = availableCPUs() - reserve
	}
	if n < 2 {
		n = 2
	}
	return n
}

// captionGPUThreads is the shared recognizer's allowance when the arithmetic
// is happening on a graphics chip: half of what the processor path would get.
//
// Half, and not a fixed number, because the machine is the thing that decides
// how much of itself it can spare — a four-core box and a thirty-two-core box
// are not both entitled to the same four threads, and neither should be told a
// figure that was measured on somebody else's hardware. It is derived from the
// same probe as the processor path, so a cgroup quota, a hybrid chip's
// efficiency cores and a machine with very few cores are all already accounted
// for by the time this halves it.
//
// Half rather than all of it because on this path the threads are not doing
// the arithmetic. The GPU is; these run the mel frontend and the handoffs, and
// ggml spin-waits every one of them while the GPU computes, so a thread beyond
// what the frontend needs is a core burned idling next to a tune. The floor of
// two is the same floor the processor path has: below that there is nothing to
// share out.
func captionGPUThreads() int {
	return gpuThreadShare(captionComputeThreads())
}

// gpuThreadShare turns a processor thread allowance into the GPU path's share
// of it. One rule, applied wherever a session is opened, so the two places
// that open one cannot drift apart on what a GPU backend is entitled to.
//
// It never raises the figure it is given. The allowance handed in has already
// been divided by whatever else is running — on the per-stream path that is
// the other streams — and half of a small share is still smaller than the
// share, which is the direction this may move it and the only one.
func gpuThreadShare(cpuThreads int) int {
	n := cpuThreads / 2
	if n < 1 {
		n = 1
	}
	if n > cpuThreads {
		n = cpuThreads
	}
	return n
}

// performanceCores counts physical performance cores. On an Intel hybrid chip
// the kernel lists the P-cores' logical CPUs under cpu_core; elsewhere every
// core is a performance core and the count is physical rather than logical.
// Both fall back conservatively: half the logical CPUs.
func performanceCores() int {
	if b, err := os.ReadFile("/sys/devices/cpu_core/cpus"); err == nil {
		if n := countCPUList(strings.TrimSpace(string(b))); n > 0 {
			// The list is logical CPUs; P-cores have two apiece.
			return (n + 1) / 2
		}
	}
	return (availableCPUs() + 1) / 2
}

// countCPUList counts entries in a kernel cpu list like "0-15" or "0-7,16-19".
func countCPUList(s string) int {
	total := 0
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if lo, hi, ok := strings.Cut(part, "-"); ok {
			a, err1 := strconv.Atoi(lo)
			b, err2 := strconv.Atoi(hi)
			if err1 != nil || err2 != nil || b < a {
				return 0
			}
			total += b - a + 1
		} else if _, err := strconv.Atoi(part); err == nil {
			total++
		} else {
			return 0
		}
	}
	return total
}

// captionedStreams is how many tuners could be captioning at the same time.
// tunersStreaming counts tuners with a stream on them right now.
//
// The page uses it to refuse to switch captions on mid-stream. Enabling them is
// no longer only a settings write: it is what asks for the graphics driver, and
// that install is a package at a time against whatever is playing. Startup does
// it where nothing can be tuning; asking for it in the middle of a recording is
// the one thing this program is not allowed to make easy.
func tunersStreaming() int {
	tunerLock.Lock()
	defer tunerLock.Unlock()
	n := 0
	for i := range tuners {
		if tuners[i].active {
			n++
		}
	}
	return n
}

func captionedStreams(cfg captionConfig) int {
	// Only tuners that exist. A selection left over from a larger setup would
	// otherwise inflate every memory figure and shrink every thread share for
	// streams that can never run.
	n := 0
	for _, t := range cfg.Tuners {
		if t >= 0 && t < len(tuners) {
			n++
		}
	}
	if n == 0 {
		n = len(tuners)
	}
	if n == 0 {
		n = 1
	}
	return n
}

func captionStatusPayload() captionStatus {
	cfg := currentCaptionConfig()
	cur := currentEngineVariant()
	hasGPU := gpuAvailable()
	pick, why := recommendedModel()
	streams := captionedStreams(cfg)
	models := make([]captionStatusModel, 0, len(captionModelCatalog))
	for _, m := range captionModelCatalog {
		rt := runtimeOf(m)
		mem, reuse, total := memoryNote(m, cfg, streams)
		perMB := streamMemoryMB(m)
		totalMB := perMB * streams
		if runtimeOf(m) == rtTranscribe && !modelStreams(m, cfg) {
			totalMB = perMB // one shared copy, whatever the tuner count
		}
		blocked := ""
		switch {
		case m.NeedsGPU && !hasGPU:
			blocked = "Needs a GPU, and no GPU build can run in this container yet. On a processor it " +
				"falls behind live audio and drops most of what is said. Set up Vulkan or CUDA above " +
				"and this appears — integrated graphics are enough."
		case m.NeedsGPU && gpuVariant(rt) == "":
			// The card is there; the build that uses it is not. Saying so is
			// the difference between a two minute fix and a mystery.
			blocked = "This machine has a usable GPU, but the GPU build of " + findSpeechRuntime(rt).Name +
				" has not been downloaded yet. Download it above and this appears."
		}
		models = append(models, captionStatusModel{
			captionModel:  m,
			Installed:     modelInstalled(m),
			Engine:        rt,
			EngineName:    findSpeechRuntime(rt).Name,
			EngineReady:   runtimeInstalled(rt, cur),
			Recommended:   m.Key == pick,
			Why:           map[bool]string{true: why}[m.Key == pick],
			Runnable:      blocked == "",
			Blocked:       blocked,
			Memory:        mem,
			Reuse:         reuse,
			MemoryMB:      perMB,
			MemoryTotalMB: totalMB,
			MemoryTotal:   total,
			Windows:       quirksFor(m).Windows,
			Window:        phraseWindowFor(quirksFor(m), cfg),
			URL:           modelURL(m),
		})
	}
	engineURL, _, _ := engineAsset()
	needed := findSpeechRuntime(neededRuntime())
	curVariant, _ := findEngineVariant(cur)
	recog := recognizerSnapshot()
	persistent, dir := captionDirPersistent()
	persistWarning := ""
	if !persistent {
		persistWarning = fmt.Sprintf("%s is not a bind mount, so anything downloaded here is lost when the container is recreated. Add this to your compose file and recreate it:  - ${HOST_DIR}/ah4c/captions:/opt/captions", dir)
	}
	drivers := make([]captionStatusDriver, 0, len(gpuRuntimes))
	for _, g := range gpuRuntimes {
		drivers = append(drivers, captionStatusDriver{
			gpuRuntime: g,
			Downloaded: driverDownloaded(g),
			Active:     driverActive(g),
		})
	}
	// Both engines' builds are offered, so the page can say what a model would
	// cost before it is chosen rather than after.
	engines := make(map[string][]captionStatusEngine, len(speechRuntimes))
	for _, eng := range speechRuntimes {
		list := make([]captionStatusEngine, 0, len(engineVariants))
		for _, v := range engineVariants {
			if !runtimeVariantOffered(eng.Key, runtime.GOOS, runtime.GOARCH, v.Key) {
				continue
			}
			url, _, _, ok := runtimeAssetFor(eng.Key, runtime.GOOS, runtime.GOARCH, v.Key)
			if !ok {
				continue
			}
			v.SizeMB = runtimeSizeMB(eng.Key, v)
			// The heading stays the thing being chosen — where it runs. Which
			// engine that build belongs to is said underneath, because it
			// identifies the download without pretending to be a decision.
			list = append(list, captionStatusEngine{
				engineVariant: v,
				Usable:        engineUsable(v),
				Installed:     runtimeInstalled(eng.Key, v.Key),
				Selected:      v.Key == cur,
				URL:           url,
				File:          path.Base(url),
				PartOf:        eng.Name + " " + eng.Version,
			})
		}
		// Some builds are the same archive under two names, because an engine
		// can put more than one backend in one file. Say so on both rows: they
		// otherwise read as two separate downloads of identical size, and
		// downloading one silently marks the other installed, which looks like
		// a bug rather than a convenience.
		for i := range list {
			for j := range list {
				if i == j || list[i].URL != list[j].URL {
					continue
				}
				list[i].Shared = fmt.Sprintf(
					"Same file as %q — one download covers both, so you only need to fetch one of them.",
					list[j].Name)
				break
			}
		}
		engines[eng.Key] = list
	}
	state := "ready"
	switch {
	case engineLibPath() == "":
		state = fmt.Sprintf("no %s build is published for %s/%s", needed.Name, runtime.GOOS, runtime.GOARCH)
	case !engineInstalled():
		state = needed.Name + " has not been downloaded yet"
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		state = "ffmpeg was not found, so audio cannot be decoded"
	}
	return captionStatus{
		Config:         cfg,
		Models:         models,
		Languages:      captionLanguageNames,
		Download:       downloadStatus(),
		Runtime:        state,
		RuntimeReady:   engineInstalled(),
		RuntimeSizeMB:  runtimeSizeMB(needed.Key, curVariant),
		RuntimeName:    needed.Name,
		Runtimes:       runtimeDescriptions(),
		RuntimeList:    speechRuntimes,
		RuntimeVersion: needed.Version,
		RuntimeURL:     engineURL,
		Recognizer:     recog,
		Speeds:         captionSpeeds,
		OnScreen:       captionOnScreen,
		Streaming:      tunersStreaming(),
		Persistent:     persistent,
		PersistWarning: persistWarning,
		MemoryWarning:  memoryWarning(cfg),
		Engines:        engines,
		Drivers:        drivers,
		Accel:          accelStatus(),
		DriverInstall:  gpuInstallStatus(),
		Tuners:         len(tuners),
	}
}

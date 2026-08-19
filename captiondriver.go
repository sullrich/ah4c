package main

// Getting the pieces onto the machine: model weights, the speech engine, and
// the graphics driver.
//
// All of it downloads, unpacks and installs around tunes rather than through
// them. The rules that decide when it may run are in CLAUDE.md and every one of
// them cost a recording before it was written down.

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ebitengine/purego"
)

// Model download
// ---------------------------------------------------------------------------

type captionDownload struct {
	Model    string `json:"model"`
	Active   bool   `json:"active"`
	File     string `json:"file"`
	Done     int64  `json:"done"`
	Total    int64  `json:"total"`
	Index    int    `json:"index"`
	Count    int    `json:"count"`
	Err      string `json:"err"`
	Finished bool   `json:"finished"`
}

var (
	dlLock  sync.Mutex
	dlState captionDownload
)

func downloadStatus() captionDownload {
	dlLock.Lock()
	defer dlLock.Unlock()
	return dlState
}

// startModelDownload pulls a model from Hugging Face in the background. It is a
// no-op if a download is already running.
func startModelDownload(m captionModel) error {
	// The page hides a model it cannot run, but the endpoint is reachable
	// without it, and this is the one download where a mistake costs gigabytes.
	if m.NeedsGPU && !gpuAvailable() {
		return fmt.Errorf("%s needs a GPU, and no GPU build can run in this container", m.Name)
	}
	dlLock.Lock()
	if dlState.Active {
		dlLock.Unlock()
		return fmt.Errorf("a download is already running")
	}
	dlState = captionDownload{Model: m.Key, Active: true, Count: 1}
	dlLock.Unlock()

	go func() {
		// No gate at the door, deliberately.
		//
		// This download already yields all the way through: streamToFile stops
		// between reads for as long as any tune is in flight, a quarter second
		// at a time. Work that yields throughout does not need permission to
		// begin, because it is never the thing holding the disk when a tune
		// turns up.
		//
		// A gate here was worse than nothing. Ten seconds of proven quiet is
		// not a thing a three-tuner machine reliably produces, so the press
		// sat in a loop that said nothing while it waited — which from the
		// page is indistinguishable from a button that does not work.
		err := fetchModel(m)
		dlLock.Lock()
		dlState.Active = false
		dlState.Finished = true
		if err != nil {
			dlState.Err = err.Error()
			logger("[CC] Model download failed: %v", err)
		} else {
			logger("[CC] Model %s is ready", m.Key)
		}
		refreshCaptionReady()
		dlLock.Unlock()
	}()
	return nil
}

func fetchModel(m captionModel) error {
	if err := os.MkdirAll(captionModels, 0o755); err != nil {
		return err
	}
	// The download gets its own client: the package-level default in main.go
	// carries a 5 second response header timeout, which is right for an encoder
	// and far too tight for a several hundred megabyte model off a CDN.
	client := &http.Client{Timeout: 2 * time.Hour}
	url := modelURL(m) + "?download=true"
	dlLock.Lock()
	dlState.File, dlState.Index = m.File, 1
	dlLock.Unlock()
	logger("[CC] Downloading %s from %s", m.File, url)
	if err := streamToFile(client, url, modelPath(m)); err != nil {
		return fmt.Errorf("%s: %w", m.File, err)
	}
	return nil
}

// streamToFile downloads url to dst via a temporary name, so an interrupted
// download is never mistaken for an installed file on the next start.
func streamToFile(client *http.Client, url, dst string) error {
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s", resp.Status)
	}
	dlLock.Lock()
	dlState.Total = resp.ContentLength
	dlLock.Unlock()

	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	buf := make([]byte, 512*1024)
	var done int64
	paused := false
	for {
		// Yield to tunes for the whole download, not merely at the start of it.
		//
		// The caller waits for a quiet moment before beginning, which was the
		// entire protection, and then this ran for minutes — half a gigabyte
		// for a streaming model, a gigabyte and a half for the phrase one —
		// over the same network and onto the same disk the tuners are using.
		// A tune that started thirty seconds in was competing with a transfer
		// that had no idea it existed, and lost: forty seconds without
		// confirming playback, which is longer than the DVR waits.
		//
		// So the transfer stops while a tune is in flight and resumes when it
		// settles. Tunes take seconds and the connection tolerates a pause of
		// that order; if it does not, the download fails and is retried, which
		// is a button rather than a recording.
		for tunesPending() {
			if !paused {
				paused = true
				logger("[CC] Pausing the download for a tune in progress")
			}
			time.Sleep(250 * time.Millisecond)
		}
		paused = false
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				os.Remove(tmp)
				return werr
			}
			done += int64(n)
			dlLock.Lock()
			dlState.Done = done
			dlLock.Unlock()
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			os.Remove(tmp)
			return rerr
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// startRuntimeDownload fetches the engine the selected model needs, in the
// background. It is the one piece of native code captions need, and it is
// pulled on demand rather than shipped in the image.
//
// Which engine that is follows from the model rather than from a separate
// choice on the page: picking a model and then being asked which of
// two C++ libraries should run it is not a question anyone wants.
// modelKey may name a model that has not been saved yet, so the page can fetch
// the engine for something it is only considering.
func startRuntimeDownload(variant, modelKey string) error {
	if _, found := findEngineVariant(variant); !found || variant == "auto" {
		variant = currentEngineVariant()
	}
	rt := neededRuntime()
	if m, ok := findCaptionModel(modelKey); ok {
		rt = runtimeOf(m)
	}
	eng := findSpeechRuntime(rt)
	url, dir, lib, ok := runtimeAssetFor(rt, runtime.GOOS, runtime.GOARCH, variant)
	if !ok {
		return fmt.Errorf("no %s build is published for %s/%s", eng.Name, runtime.GOOS, runtime.GOARCH)
	}
	dlLock.Lock()
	if dlState.Active {
		dlLock.Unlock()
		return fmt.Errorf("a download is already running")
	}
	dlState = captionDownload{Model: "engine", Active: true, Count: 1, Index: 1,
		File: eng.Name + " " + eng.Version + " (" + variant + ")"}
	dlLock.Unlock()

	logger("[CC] Downloading %s from %s", eng.Name, url)
	go func() {
		// Same rule as the model download: never fight a recording.
		// No gate here either, and for the same reason as the model download:
		// countingReader stops between reads while a tune is in flight, so the
		// transfer and the decompression behind it both stand aside on their
		// own. Gating it only added a silent wait in front of work that was
		// already polite.
		//
		// The engine is a library plus the ggml backends it loads from
		// alongside itself, so the whole archive is taken.
		err := fetchRuntime(url, dir, lib, rt == rtTranscribe)
		dlLock.Lock()
		dlState.Active = false
		dlState.Finished = true
		if err != nil {
			dlState.Err = err.Error()
			logger("[CC] %s download failed: %v", eng.Name, err)
		} else {
			logger("[CC] %s %s is ready", eng.Name, eng.Version)
		}
		refreshCaptionReady()
		dlLock.Unlock()
	}()
	return nil
}

// fetchRuntime downloads the release archive and unpacks it into dir.
//
// With all set it takes every file, which is what an engine that loads sibling
// libraries needs; otherwise it takes the one named library, matching on file
// name so a change to the directory prefix inside the archive does not break
// the download.
func fetchRuntime(url, dir, lib string, all bool) error {
	dst := filepath.Join(captionRuntime, dir)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	dlLock.Lock()
	dlState.Total = resp.ContentLength
	dlLock.Unlock()

	gz, err := gzip.NewReader(&countingReader{r: resp.Body})
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	found := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		isLib := path.Base(h.Name) == lib
		if !all {
			if !isLib {
				continue
			}
			// The one file wanted is here, so stop reading rather than pulling
			// the rest of a half gigabyte archive through the connection.
			if err := writeRuntimeFile(dst, lib, tr); err != nil {
				return err
			}
			return nil
		}
		rel, ok := archiveRelPath(h.Name)
		if !ok {
			continue
		}
		if err := writeRuntimeFile(dst, rel, tr); err != nil {
			return err
		}
		found = found || isLib
	}
	if !found {
		return fmt.Errorf("%s was not found in the archive", lib)
	}
	return nil
}

// archiveRelPath drops the archive's own top-level directory, so what lands on
// disk is not named after the release it came out of, and refuses anything that
// would climb out of the destination.
func archiveRelPath(name string) (string, bool) {
	clean := strings.TrimPrefix(path.Clean("/"+name), "/")
	i := strings.IndexByte(clean, '/')
	if i < 0 {
		return "", false
	}
	rel := clean[i+1:]
	if rel == "" || rel == "." {
		return "", false
	}
	return rel, true
}

// writeRuntimeFile writes one archive entry through a temporary name, so an
// interrupted download never leaves a half library looking installed.
func writeRuntimeFile(dir, rel string, r io.Reader) error {
	dst := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// countingReader reports download progress for a stream that is being
// decompressed on the fly.
type countingReader struct {
	r      io.Reader
	done   int64
	paused bool
}

func (c *countingReader) Read(p []byte) (int, error) {
	// The engine archive is smaller than a model but the reasoning is the
	// same, and it is decompressed on the way in, so it costs processor as
	// well as network. It waits for a tune exactly as the model download does.
	for tunesPending() {
		if !c.paused {
			c.paused = true
			logger("[CC] Pausing the download for a tune in progress")
		}
		time.Sleep(250 * time.Millisecond)
	}
	c.paused = false
	n, err := c.r.Read(p)
	if n > 0 {
		c.done += int64(n)
		dlLock.Lock()
		dlState.Done = c.done
		dlLock.Unlock()
	}
	return n, err
}

// removeCaptionModel deletes a downloaded model, freeing the several hundred
// megabytes it occupies.
func removeCaptionModel(m captionModel) error {
	dlLock.Lock()
	active := dlState.Active && dlState.Model == m.Key
	dlLock.Unlock()
	if active {
		return fmt.Errorf("that model is still downloading")
	}
	if err := os.Remove(modelPath(m)); err != nil && !os.IsNotExist(err) {
		return err
	}
	logger("[CC] Removed model %s", m.Key)
	refreshCaptionReady()
	return nil
}

// ---------------------------------------------------------------------------
// GPU driver support
// ---------------------------------------------------------------------------

// A GPU build of the engine needs a driver in the container, and the base image
// ships none. That is handled the same way the model is: the packages are
// downloaded once into the bind mount, where they survive a rebuild, and put
// back in place at startup from that copy without touching the network again.
// Nobody who leaves captions off pays anything for it.
//
// The packages are the distribution's own rather than files picked by hand. A
// Vulkan driver pulls in a dozen libraries, and letting the package manager
// work out which is the difference between something that runs on other
// people's machines and something that runs on mine.

type gpuRuntime struct {
	Key      string   `json:"key"`
	Name     string   `json:"name"`
	Desc     string   `json:"desc"`
	Packages []string `json:"packages"`
	// Needs is the library whose presence proves the driver is in place.
	Needs string `json:"needs"`
	Note  string `json:"note"`
}

var gpuRuntimes = []gpuRuntime{
	{
		Key:      "vulkan",
		Name:     "Vulkan driver",
		Desc:     "The Vulkan loader and the open source drivers, which cover Intel and AMD graphics. On NVIDIA the container runtime brings its own driver and only the loader is used.",
		Packages: []string{"libvulkan1", "mesa-vulkan-drivers"},
		Needs:    "libvulkan.so.1",
		Note:     "Your compose file also has to pass the graphics device through, with a devices entry for /dev/dri.",
	},
}

func findGPURuntime(key string) (gpuRuntime, bool) {
	for _, g := range gpuRuntimes {
		if g.Key == key {
			return g, true
		}
	}
	return gpuRuntime{}, false
}

func driverDir(g gpuRuntime) string { return filepath.Join(captionDrivers, g.Key) }

// driverDownloaded reports whether the saved set in the bind mount is
// complete: every package the runtime names has its package file present. A
// partial set — a loader without the drivers behind it, say — installs
// something that looks alive and works as nothing, so partial does not count.
func driverDownloaded(g gpuRuntime) bool {
	ents, err := os.ReadDir(driverDir(g))
	if err != nil {
		return false
	}
	for _, pkg := range g.Packages {
		found := false
		for _, e := range ents {
			if !e.IsDir() && strings.HasPrefix(e.Name(), pkg+"_") && strings.HasSuffix(e.Name(), ".deb") {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// driverActive reports whether the driver is loadable right now.
func driverActive(g gpuRuntime) bool {
	// Through the same cache the engine probe uses, because it is the same
	// question about the same library. This asked it afresh every time, and the
	// page asks on every poll — a second and a half apart, for as long as
	// anybody has it open — so the loader's global lock was being taken all day
	// for an answer that had already been worked out. The engine's own calls
	// into native code contend for that lock, which is the reason the cache was
	// written in the first place; this simply was not using it.
	return engineUsable(engineVariant{Needs: g.Needs})
}

type gpuInstallState struct {
	Kind     string `json:"kind"`
	Active   bool   `json:"active"`
	Finished bool   `json:"finished"`
	Err      string `json:"err"`
	Log      string `json:"log"`
	// Step names what is happening and Done of Total how far through it is, so
	// the page can show a bar rather than the word "Installing" for a minute
	// and a half while somebody wonders whether it has hung.
	Step  string `json:"step"`
	Done  int    `json:"done"`
	Total int    `json:"total"`
}

// driverUrgent is set while somebody is waiting at the page for this.
//
// The per-package quiet wait exists for work nobody asked for — the restore at
// startup, which is free anyway because it runs before the port is bound. On a
// button press it is the wrong trade entirely: up to fifteen seconds of waiting
// in front of each of thirty-seven packages is nine minutes of somebody staring
// at a page, to protect tunes that the person pressing the button knows about.
//
// So an explicit press goes straight through. It is still one dpkg at a time
// and still behind nice and ionice, so a tune that does arrive loses only the
// package in flight rather than the whole set.
var driverUrgent atomic.Bool

// noteDriverStep publishes progress for the page. Called from the download and
// the install, which are the two parts long enough to need one.
func noteDriverStep(step string, done, total int) {
	gpuLock.Lock()
	gpuState.Step, gpuState.Done, gpuState.Total = step, done, total
	gpuLock.Unlock()
}

var (
	gpuLock  sync.Mutex
	gpuState gpuInstallState
)

func gpuInstallStatus() gpuInstallState {
	gpuLock.Lock()
	defer gpuLock.Unlock()
	return gpuState
}

// startDriverDownload fetches the packages into the bind mount and puts them in
// place, in the background.
func startDriverDownload(kind string) error {
	g, ok := findGPURuntime(kind)
	if !ok {
		return fmt.Errorf("unknown driver %q", kind)
	}
	if runtime.GOOS != "linux" {
		return fmt.Errorf("drivers are only installable inside the container")
	}
	gpuLock.Lock()
	if gpuState.Active {
		gpuLock.Unlock()
		return fmt.Errorf("a driver download is already running")
	}
	gpuState = gpuInstallState{Kind: kind, Active: true}
	gpuLock.Unlock()

	go func() {
		// Somebody is at the page waiting for this, so it goes now.
		//
		// There was a wait for a quiet minute in front of it and a wait for a
		// quiet moment in front of each of thirty-seven packages behind it,
		// which on a machine with tuners running is minutes of a progress bar
		// not moving. Those gates are for the restore at startup, which nobody
		// asked for and which is free anyway because it runs before the port is
		// bound. A button press is somebody who knows what they are doing and
		// what it costs.
		//
		// It is still one dpkg at a time and still behind nice and ionice, so a
		// tune arriving in the middle loses the package in flight rather than
		// the whole set.
		driverUrgent.Store(true)
		defer driverUrgent.Store(false)
		log, err := fetchDriver(g)
		if err == nil {
			var l2 string
			l2, err = applyDriver(g)
			log += l2
		}
		gpuLock.Lock()
		gpuState.Active = false
		gpuState.Finished = true
		gpuState.Step, gpuState.Done, gpuState.Total = "", 0, 0
		gpuState.Log = tailLines(log, 12)
		if err != nil {
			gpuState.Err = err.Error()
			logger("[CC] %s could not be set up: %v", g.Name, err)
		} else {
			logger("[CC] %s is ready", g.Name)
			// Whether a GPU build can load is cached, and installing a
			// driver is the one moment that answer changes.
			forgetEngineUsable()
			forgetBrokenDrivers()
			refreshGPUReady()
			// Record that this driver is wanted. Downloading it is the only
			// point at which the intent is expressed: the engine picker will
			// not offer a GPU build until the driver already loads, so waiting
			// for that selection to save the choice means it is never saved and
			// the driver disappears on the next restart.
			cfg := currentCaptionConfig()
			if cfg.GPURuntime != g.Key {
				cfg.GPURuntime = g.Key
				if e := saveCaptionConfig(cfg); e != nil {
					logger("[CC] Could not record the driver choice: %v", e)
				}
			}
		}
		gpuLock.Unlock()
	}()
	return nil
}

// fetchDriver downloads the packages and everything they depend on into the
// bind mount. Only this step needs the network.
func fetchDriver(g gpuRuntime) (string, error) {
	if _, err := exec.LookPath("apt-get"); err != nil {
		return "", fmt.Errorf("this image has no apt-get, so the driver cannot be fetched from here")
	}
	dir := driverDir(g)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	var log strings.Builder
	// Polite, like everything else here. The two calls that go through this —
	// refreshing the package lists and reading the dependency tree — are the
	// part of the driver fetch that is neither gated nor divided: they run
	// straight after the gate whatever the gate answered, and one of them
	// rewrites every package list in the container. Losing to a tuner is the
	// least they can do.
	run := func(args ...string) error {
		logger("[CC] %v", args)
		cmd := politeCommand(args[0], args[1:]...)
		cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
		b, e := cmd.CombinedOutput()
		log.Write(b)
		return e
	}
	// apt-get download writes into the working directory and has no option
	// that moves it, so the working directory is where the staging set is.
	// Not logged per call the way run is: this one is invoked once per package
	// in the closure, and forty lines of apt command line buries the two lines
	// that say what actually happened.
	runIn := func(dir string, args ...string) error {
		cmd := politeCommand(args[0], args[1:]...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
		b, e := cmd.CombinedOutput()
		log.Write(b)
		return e
	}
	// The download lands in a staging directory and replaces the saved set
	// only once it is complete. Clearing first and downloading after was
	// tried, and a fetch that failed partway left a set with the loader and
	// no drivers behind it — which installs cleanly, loads cleanly, and
	// captions nothing. A failed fetch now changes nothing.
	staging := filepath.Join(dir, "incoming")
	os.RemoveAll(staging)
	if err := os.MkdirAll(filepath.Join(staging, "partial"), 0o755); err != nil {
		return "", err
	}
	stagingAbs, err := filepath.Abs(staging)
	if err != nil {
		return "", err
	}
	// The stable suite of the base image freezes its graphics drivers for
	// years — the driver it offers today shipped before this GPU's compute
	// paths were optimized, and a modern engine on a museum driver runs at a
	// fraction of the hardware's speed while looking perfectly healthy. The
	// distribution's backports suite carries the current driver for exactly
	// this reason, and it is the only place this fetches from.
	//
	// There is no fallback to the base image's own driver, deliberately. A
	// 2022 driver installs, loads and captions, and does it several times
	// slower on the same chip while every check passes — which is a worse
	// outcome than no driver and a sentence saying why. If the live suite
	// cannot be reached the second attempt is the archive of the same suite,
	// which can only ever hold the same era of driver.
	suite := backportsSuite()
	if suite == "" {
		return log.String(), fmt.Errorf("this image's distribution could not be identified, so the current graphics driver cannot be located")
	}
	// What has to be saved is everything a *fresh* container will need, and
	// that is not what apt would download here.
	//
	// "apt-get install --download-only" fetches what has to change in this
	// container. Run it once in a container where the driver already works and
	// it fetches the two named packages and nothing else, because everything
	// underneath them is already installed — and that two-package set is what
	// then replaced a complete one and got saved as the thing to restore. The
	// next rebuild installed a driver on top of an empty space where a dozen
	// libraries used to be, and the loader skipped every one of them.
	//
	// Worse, that state is self-sealing. The driver goes in with dpkg and
	// --force-depends, so the package sits there installed with its
	// dependencies unmet, and every apt run afterwards inherits the mess:
	// asked to fetch the package again it tries to satisfy the version already
	// installed, cannot, and exits without downloading anything. The only way
	// out was to delete the directory by hand.
	//
	// So the set is worked out from the packaging rather than from this
	// container's state: the full recursive dependency closure, fetched
	// one-by-one with a command that has no solver in it to be confused. It
	// downloads a package because it is in the closure, not because this
	// container happens to lack it, which makes the saved set the same whether
	// it is built on a clean container or a broken one.
	var (
		closure []string
		aptErr  error
	)
	for i, src := range backportsSources(suite) {
		if i > 0 {
			logger("[CC] The driver could not be fetched from %s (%v); trying %s", backportsSources(suite)[i-1].name, aptErr, src.name)
			os.RemoveAll(staging)
			if err := os.MkdirAll(filepath.Join(staging, "partial"), 0o755); err != nil {
				return log.String(), err
			}
		}
		if err := writeAptSource(src, &log); err != nil {
			aptErr = err
			continue
		}
		if err := run(append([]string{"apt-get", "update"}, src.opts...)...); err != nil {
			aptErr = fmt.Errorf("apt-get update: %w", err)
			continue
		}
		c, cerr := driverClosure(g.Packages, suite, src.opts, &log)
		if len(c) == 0 {
			aptErr = fmt.Errorf("could not work out what %s depends on: %w", strings.Join(g.Packages, " "), cerr)
			continue
		}
		logger("[CC] %s needs %d packages in all; fetching them from %s", g.Name, len(c), src.name)
		closure = c
		// A package at a time, for the same reason the install goes a package
		// at a time. The whole closure on one command line is tens of megabytes
		// over the network and onto the disk inside a single call that cannot
		// be paused, cannot be divided once it has started, and takes about as
		// long as a DVR is willing to wait for a tune. Split up, the longest it
		// can be in anybody's way is one package, and it stands aside between
		// them.
		warned := false
		noteDriverStep("Fetching packages", 0, len(c))
		for i, p := range c {
			if !driverUrgent.Load() && !waitTuneQuietHeld(5*time.Second, 15*time.Second) && !warned {
				warned = true
				logger("[CC] %s is being fetched through a busy machine, one package at a time", g.Name)
			}
			if aptErr = runIn(stagingAbs, downloadArgs(suite, src.opts, []string{p})...); aptErr != nil {
				aptErr = fmt.Errorf("fetching %s: %w", p, aptErr)
				break
			}
			noteDriverStep("Fetching packages", i+1, len(c))
			if i == len(c)-1 || (i+1)%10 == 0 {
				logger("[CC] %s: %d of %d packages fetched", g.Name, i+1, len(c))
			}
		}
		if aptErr == nil {
			break
		}
	}
	if len(closure) == 0 {
		os.RemoveAll(staging)
		return log.String(), fmt.Errorf("the current graphics driver could not be fetched: %w", aptErr)
	}
	// Completeness is judged before anything is replaced, and against the
	// whole closure rather than the two packages that were asked for by name.
	// Checking only those is what let a set with no libraries under it pass.
	var missing []string
	have := map[string]bool{}
	if ents, e := os.ReadDir(staging); e == nil {
		for _, f := range ents {
			if n, _, ok := strings.Cut(f.Name(), "_"); ok && strings.HasSuffix(f.Name(), ".deb") {
				have[n] = true
			}
		}
	}
	for _, pkg := range closure {
		if !have[pkg] {
			missing = append(missing, pkg)
		}
	}
	if len(missing) > 0 {
		os.RemoveAll(staging)
		if aptErr != nil {
			return log.String(), fmt.Errorf("downloading %s: %w — the previously saved packages were left untouched", strings.Join(g.Packages, " "), aptErr)
		}
		return log.String(), fmt.Errorf("the download finished without %d of the %d packages needed (%s); the previously saved packages were left untouched",
			len(missing), len(closure), strings.Join(missing, " "))
	}
	if old, _ := savedDebs(g); len(old) > 0 {
		for _, p := range old {
			os.Remove(p)
		}
	}
	if ents, e := os.ReadDir(staging); e == nil {
		for _, f := range ents {
			if strings.HasSuffix(f.Name(), ".deb") {
				os.Rename(filepath.Join(staging, f.Name()), filepath.Join(dir, f.Name()))
			}
		}
	}
	os.RemoveAll(staging)

	// Whether or not apt honored the archive directory, the packages have to
	// end up in the bind mount, because that is the only thing a rebuild does
	// not erase. If they went to the default cache instead, move them.
	if !driverDownloaded(g) {
		if n := harvestDebs(dir); n > 0 {
			logger("[CC] Recovered %d packages from the default apt cache", n)
		}
	}
	if !driverDownloaded(g) {
		if aptErr != nil {
			return log.String(), fmt.Errorf("downloading %s: %w", strings.Join(g.Packages, " "), aptErr)
		}
		return log.String(), fmt.Errorf("no packages ended up in %s", dir)
	}
	n, _ := savedDebs(g)
	names := make([]string, len(n))
	for i, p := range n {
		names[i] = filepath.Base(p)
	}
	// The filenames carry the versions, and the versions are the whole story:
	// a driver from the wrong era looks identical in every way but speed.
	logger("[CC] Saved %d packages for %s in %s: %s", len(n), g.Name, dir, strings.Join(names, " "))
	return log.String(), nil
}

// backportsSuite names the backports suite for whatever distribution this
// image is built on, or "" if that cannot be worked out. Backports is where
// the distribution keeps current graphics drivers for a stable release.
func backportsSuite() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	codename := ""
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "VERSION_CODENAME="); ok {
			codename = strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	if codename == "" {
		return ""
	}
	return codename + "-backports"
}

// backportsSnapshot is the date the archive is read at when the live suite
// cannot be reached. Any timestamp is accepted and resolves to the archive as
// it stood then, so this only has to be a date the driver was known good.
const backportsSnapshot = "20260601T000000Z"

// aptSource is one place to fetch from: the line that names it, a name for the
// log, and any options apt needs to read it.
type aptSource struct {
	name string
	line string
	opts []string
}

// backportsSources is where the driver may be fetched from, in order.
//
// Both are the same suite of the same distribution, and that is the point.
// The second is the archive of the first, read at a fixed date, for the day
// the live index has moved on or the mirror will not answer. Neither can hand
// back the base image's own driver, which is the one outcome worth refusing:
// it installs, it loads, it captions, and it does all of it several times
// slower than the driver being asked for while every check passes.
func backportsSources(suite string) []aptSource {
	return []aptSource{
		{
			name: suite,
			line: fmt.Sprintf("deb http://deb.debian.org/debian %s main\n", suite),
		},
		{
			name: "the " + suite + " archive at " + backportsSnapshot,
			line: fmt.Sprintf("deb https://snapshot.debian.org/archive/debian/%s/ %s main\n", backportsSnapshot, suite),
			// The archive serves the Release file exactly as it was, so its
			// valid-until date is in the past by design. Refusing it on those
			// grounds would refuse the archive entirely.
			opts: []string{"-o", "Acquire::Check-Valid-Until=false"},
		},
	}
}

// writeAptSource puts one source in place, replacing whatever the last attempt
// left behind. One file, rewritten, so a fallback never ends up listed beside
// the source it was falling back from.
func writeAptSource(s aptSource, log *strings.Builder) error {
	if err := os.WriteFile("/etc/apt/sources.list.d/backports.list", []byte(s.line), 0o644); err != nil {
		fmt.Fprintf(log, "could not add %s: %v\n", s.name, err)
		return fmt.Errorf("could not add the %s package source: %w", s.name, err)
	}
	logger("[CC] Using %s for a current graphics driver", s.name)
	return nil
}

// downloadArgs fetches packages by name and nothing else. apt-get download
// takes the candidate version of each package it is given and writes the file
// out; it does not consult what is installed, plan an installation, or have an
// opinion about dependencies, which is exactly why it is used here. The set to
// fetch has already been decided by driverClosure.
func downloadArgs(suite string, opts, pkgs []string) []string {
	args := append([]string{"apt-get", "download"}, opts...)
	if suite != "" {
		// One knob, applied to the closure and the fetch alike, so both answer
		// about the same versions of the same packages.
		args = append(args, "-o", "APT::Default-Release="+suite)
	}
	return append(args, pkgs...)
}

// driverClosure is every package the named ones need, worked out from the
// packaging rather than from what this container has installed.
//
// The base system is left out: anything the distribution marks essential, or
// required, or important, is in every Debian image there is and will be in the
// rebuilt container too. Saving a copy of the C library to restore over the top
// of itself is at best wasted and at worst the way a container breaks.
// Everything below that line is fair game, because a slim image carries none of
// it and a driver needs a dozen of them.
func driverClosure(pkgs []string, suite string, opts []string, log *strings.Builder) ([]string, error) {
	args := append([]string{"apt-cache"}, opts...)
	if suite != "" {
		args = append(args, "-o", "APT::Default-Release="+suite)
	}
	args = append(args, "depends", "--recurse", "--no-recommends", "--no-suggests",
		"--no-conflicts", "--no-breaks", "--no-replaces", "--no-enhances")
	args = append(args, pkgs...)
	out, err := politeCommand(args[0], args[1:]...).CombinedOutput()
	log.Write(out)
	if err != nil {
		return nil, err
	}
	// Package names sit at the left margin. Everything indented is a
	// relationship, and a name in angle brackets is a virtual package, which
	// nothing can download — the real package providing it is listed under it.
	seen := map[string]bool{}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '|' || line[0] == '<' {
			continue
		}
		name := strings.TrimSpace(line)
		// An architecture qualifier is how apt disambiguates a name, not part
		// of it; the file it downloads is named without one, and the check that
		// every package arrived compares the two.
		if n, _, ok := strings.Cut(name, ":"); ok {
			name = n
		}
		if name == "" || strings.ContainsAny(name, " \t") || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("apt-cache named no packages")
	}

	keep := map[string]bool{}
	for _, p := range pkgs {
		// The packages actually asked for stay in whatever their priority.
		keep[p] = true
	}
	shown := append([]string{"apt-cache"}, opts...)
	if suite != "" {
		shown = append(shown, "-o", "APT::Default-Release="+suite)
	}
	shown = append(shown, "show", "--no-all-versions")
	shown = append(shown, names...)
	info, err := exec.Command(shown[0], shown[1:]...).Output()
	if err != nil {
		// Without priorities there is no safe way to leave anything out, and
		// too many packages restores correctly while too few does not.
		logger("[CC] Could not read the package priorities; saving the whole dependency set")
		return names, nil
	}
	base := map[string]bool{}
	pkg, prio, essential := "", "", false
	flush := func() {
		if pkg != "" && (essential || prio == "required" || prio == "important") {
			base[pkg] = true
		}
		pkg, prio, essential = "", "", false
	}
	for _, line := range strings.Split(string(info), "\n") {
		switch {
		case strings.TrimSpace(line) == "":
			flush()
		case strings.HasPrefix(line, "Package: "):
			flush()
			pkg = strings.TrimSpace(strings.TrimPrefix(line, "Package: "))
		case strings.HasPrefix(line, "Priority: "):
			prio = strings.TrimSpace(strings.TrimPrefix(line, "Priority: "))
		case strings.HasPrefix(line, "Essential: "):
			essential = strings.TrimSpace(strings.TrimPrefix(line, "Essential: ")) == "yes"
		}
	}
	flush()

	var out2 []string
	for _, n := range names {
		if base[n] && !keep[n] {
			continue
		}
		out2 = append(out2, n)
	}
	return out2, nil
}

// harvestDebs copies anything apt left in the default cache into the bind
// mount, and returns how many it moved.
func harvestDebs(dir string) int {
	ents, err := os.ReadDir("/var/cache/apt/archives")
	if err != nil {
		return 0
	}
	moved := 0
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".deb") {
			continue
		}
		src := filepath.Join("/var/cache/apt/archives", e.Name())
		b, err := os.ReadFile(src)
		if err != nil {
			continue
		}
		if os.WriteFile(filepath.Join(dir, e.Name()), b, 0o644) == nil {
			moved++
		}
	}
	return moved
}

// savedDebs lists the packages held for a driver.
func savedDebs(g gpuRuntime) ([]string, error) {
	ents, err := os.ReadDir(driverDir(g))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".deb") {
			out = append(out, filepath.Join(driverDir(g), e.Name()))
		}
	}
	return out, nil
}

// applyDriver unpacks the saved packages into the running container. This is
// what runs after a rebuild, and it needs no network.
func applyDriver(g gpuRuntime) (string, error) {
	debs, err := savedDebs(g)
	if err != nil {
		return "", err
	}
	if len(debs) == 0 {
		return "", fmt.Errorf("no saved packages to install")
	}
	// Before the port is bound there is nothing to be polite to.
	//
	// serving is false until main has finished this and opened the door, and
	// until the door is open the DVR gets connection refused rather than a
	// request nobody answers — so there is no tune to interrupt, no gate to
	// satisfy, and no reason to take the whole set a package at a time. That is
	// what restoreGPURuntime's own comment says the startup path does, and it
	// was the one place the code did not do it: thirty-seven dpkg invocations,
	// each reading and writing the package database, where one would do.
	//
	// The polite path below is still the right shape once the door is open, and
	// still what a driver installed from the page gets.
	atStartup := !serving.Load()
	if atStartup {
		logger("[CC] Installing %d saved packages for %s in one go; nothing can be tuning yet", len(debs), g.Name)
	} else {
		logger("[CC] Installing %d saved packages for %s, one at a time with a look for a quiet moment between each", len(debs), g.Name)
	}
	// Whatever was true about these libraries stops being true here.
	forgetEngineUsable()
	forgetBrokenDrivers()
	defer refreshGPUReady()
	// --force-unsafe-io because the fsync per file is the entire weight of
	// this. dpkg syncs to be certain a package survives a power cut mid
	// install; these packages exist in the bind mount either way, and a
	// container that loses power reinstalls them from there on the way back
	// up. What it buys is nothing, and what it costs is an array full of
	// synchronous writes beside a tuner trying to prove it is playing.
	// One package at a time, with the gate re-checked between each.
	//
	// A gate can only promise the moment it is asked. Ten seconds of proven
	// quiet said nothing about the thirty that followed, and a DVR starting
	// several recordings at once is exactly the thing that arrives in the
	// middle of them — which is what happened: the window was found, the
	// install began, the tunes started a second later and one of them died.
	//
	// The rule already written down for this is that long work yields
	// throughout rather than at the door. dpkg cannot be paused, so this was
	// treated as work that could only be gated and not yielded. That was
	// wrong: it cannot be paused, but it can be *divided*. Thirty-seven
	// packages is thirty-seven jobs of a second or two, and between any two of
	// them the machine is free. The exposure drops from the length of the
	// whole install to the length of one package.
	//
	// Order does not matter because --force-depends installs regardless of what
	// is not there yet; anything left unconfigured is configured by a later
	// package or caught by the verification below.
	// A package is never skipped. The first version of this loop meant to wait
	// and try the same one again, and wrote "i--; continue" inside a range —
	// where the next iteration assigns i from the range anyway, so the
	// decrement did nothing and the package was dropped instead. On a busy
	// machine that dropped most of them, and a driver missing most of its
	// libraries installs cleanly, loads cleanly and offers no device. That is
	// the shape of tonight's "the driver isn't loading": not a failure, a
	// silent partial success.
	//
	// So quiet is preferred and not required. One package is a second or two of
	// polite work, which is a smaller thing to risk against a tune than an
	// incomplete driver is against every tune afterwards.
	var out []byte
	var failed []string
	in := 0
	err = nil
	warned := false
	noteDriverStep("Installing packages", 0, len(debs))

	// One dpkg for the whole set, and it does a better job than the loop as
	// well as a faster one: handed every package at once, dpkg orders them
	// itself and satisfies dependencies between them that --force-depends is
	// otherwise papering over one at a time.
	//
	// If it fails, the loop below still runs. A set that will not go in
	// together may still go in singly, and a driver missing one library is
	// worth more than a driver missing all of them.
	asSet := false
	if atStartup {
		args := append([]string{"-i", "--force-depends", "--force-unsafe-io"}, debs...)
		cmd := politeCommand("dpkg", args...)
		cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
		b, e := cmd.CombinedOutput()
		out = append(out, b...)
		if e == nil {
			asSet = true
			in = len(debs)
			noteDriverStep("Installing packages", in, len(debs))
			logger("[CC] %s: %d of %d packages in", g.Name, in, len(debs))
		} else {
			logger("[CC] %s would not install as a set (%v); going through them one at a time instead", g.Name, e)
		}
	}

	// Nothing left to do one at a time when the set went in as a set.
	rest := debs
	if asSet {
		rest = nil
	}
	for i, deb := range rest {
		if !driverUrgent.Load() && !waitTuneQuietHeld(5*time.Second, 15*time.Second) && !warned {
			warned = true
			logger("[CC] %s is going in through a busy machine, one package at a time and yielding between them. Nothing is being skipped.", g.Name)
		}
		cmd := politeCommand("dpkg", "-i", "--force-depends", "--force-unsafe-io", deb)
		cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
		b, e := cmd.CombinedOutput()
		out = append(out, b...)
		if e != nil {
			err = e
			failed = append(failed, filepath.Base(deb))
		} else {
			in++
		}
		noteDriverStep("Installing packages", in, len(debs))
		if i == len(debs)-1 || (i+1)%10 == 0 {
			// Packages that went in, not loops that were run. This counted the
			// index, so it read "37 of 37 packages in" whatever dpkg had made
			// of them — a line that says the same thing on success and on total
			// failure is worse than no line, because it is believed.
			logger("[CC] %s: %d of %d packages in", g.Name, in, len(debs))
		}
	}
	if len(failed) > 0 {
		logger("[CC] %s: %d packages would not install: %s", g.Name, len(failed), strings.Join(failed, " "))
	}
	// Ask the loader again rather than reading back what was true before.
	//
	// driverActive goes through the same answer-store the engine probe uses,
	// and that store was emptied before this install and not after it. The
	// install takes the better part of a minute, and the Closed Captions page
	// polls every second and a half — so a poll during the install asked
	// whether libvulkan.so.1 loads, was told no because it genuinely did not
	// yet, and stored it. The check below then read that back and reported a
	// driver that had installed perfectly as one that will not load.
	//
	// Which is why it was reported by somebody watching the page and not by
	// somebody who pressed the button and walked away.
	forgetEngineUsable()
	if err != nil && !driverActive(g) {
		return string(out), fmt.Errorf("installing the saved packages: %w", err)
	}
	if !driverActive(g) {
		return string(out), fmt.Errorf("%s still will not load", g.Needs)
	}
	// The loader loading proves nothing about the drivers behind it: a
	// forced install can put a current driver on top of last-era libraries,
	// and the driver then fails to load while everything looks installed —
	// the loader just skips it and reports no devices. So each hardware
	// driver is opened the way the loader would open it, failures are named
	// with the loader's own words, and if the network is still here apt is
	// asked to complete what the forced install skipped.
	if bad := brokenVulkanDrivers(); len(bad) > 0 {
		// Asking apt to finish the job was tried here and it cannot: a forced
		// install leaves the package present with its dependencies unmet, and
		// the only repair apt will consider from there is removing it. It said
		// so in as many words — "The following packages will be REMOVED" —
		// and then refused, because removal is forbidden. Two dead ends, one
		// after the other, printed into the log every restart.
		//
		// The honest answer is that the saved set is wrong and no amount of
		// work in this container will make it right. That is a download, and
		// the page is where the button is.
		for _, b := range bad {
			logger("[CC] Vulkan driver %s", b)
		}
		setDriverFault("The saved driver packages are missing libraries they depend on, so the loader skips every driver and offers no device. Press the driver download below to fetch a complete set; the log names each missing piece.")
		return string(out), fmt.Errorf("%d graphics drivers installed but cannot load; the log names the missing pieces", len(bad))
	}
	// Zero broken drivers must mean drivers exist: an empty manifest
	// directory verifies vacuously and captions nothing. This is the state a
	// partial download installs into, and it is a failure with a next step,
	// not a success.
	if g.Key == "vulkan" && !anyVulkanManifests() {
		setDriverFault("The driver packages installed but left no driver behind them, so there is nothing for the loader to offer. Press the driver download below to fetch a complete set.")
		return string(out), fmt.Errorf("the packages installed but no Vulkan driver manifests exist; the saved set is incomplete — press the driver download to fetch it fresh")
	}
	setDriverFault("")
	return string(out), nil
}

// politeCommand builds a command that loses every contest with a tune.
//
// The quiet gate decides when heavy work may start; this decides what happens
// when it turns out to have been wrong. A package install cannot be paused
// partway — stopping dpkg between unpacking and configuring leaves a container
// worse than either — so the one thing left is to make sure that whatever it is
// doing, the tuners get the processor and the disk first. Nice and ionice are
// in the base image; if they ever are not, the command runs exactly as it did
// before.
func politeCommand(name string, args ...string) *exec.Cmd {
	pre := []string{}
	if p, err := exec.LookPath("ionice"); err == nil {
		pre = append(pre, p, "-c3")
	}
	if p, err := exec.LookPath("nice"); err == nil {
		pre = append(pre, p, "-n", "19")
	}
	if len(pre) == 0 {
		return exec.Command(name, args...)
	}
	full := append(append(pre, name), args...)
	return exec.Command(full[0], full[1:]...)
}

// driverFault is why the graphics driver cannot be used, in the words the page
// shows, or empty when there is nothing wrong with it.
//
// It is recorded where the answer is found rather than asked for when needed.
// Finding it means opening every driver library the manifests name, and the
// page asks on every poll; the install is the one moment the answer can change,
// so that is the moment it is written down.
var (
	driverFaultLock sync.Mutex
	driverFaultWhy  string
)

func setDriverFault(why string) {
	driverFaultLock.Lock()
	driverFaultWhy = why
	driverFaultLock.Unlock()
}

func driverFaultNow() string {
	driverFaultLock.Lock()
	defer driverFaultLock.Unlock()
	return driverFaultWhy
}

// anyVulkanManifests reports whether any driver manifest exists at all,
// hardware or otherwise.
func anyVulkanManifests() bool {
	for _, dir := range []string{
		"/usr/share/vulkan/icd.d", "/etc/vulkan/icd.d",
		"/usr/local/share/vulkan/icd.d", "/usr/local/etc/vulkan/icd.d",
	} {
		if ents, err := os.ReadDir(dir); err == nil {
			for _, e := range ents {
				if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
					return true
				}
			}
		}
	}
	return false
}

// brokenVulkanDrivers opens every hardware driver named by the Vulkan
// manifests and reports the ones that fail, each with the loader's verbatim
// error — which names the missing library, and the missing library names the
// stale dependency.
// The result is remembered until something happens that could change it, which
// is a driver being installed and nothing else. Working it out means opening
// every driver the manifests name, and the page asks for it on every poll; the
// answer only moves when dpkg has just rewritten the libraries underneath it.
var (
	brokenLock  sync.Mutex
	brokenKnown []string
	brokenSeen  bool
)

// forgetBrokenDrivers is called wherever the drivers themselves change.
func forgetBrokenDrivers() {
	brokenLock.Lock()
	brokenSeen = false
	brokenLock.Unlock()
}

func brokenVulkanDrivers() []string {
	brokenLock.Lock()
	if brokenSeen {
		out := brokenKnown
		brokenLock.Unlock()
		return out
	}
	brokenLock.Unlock()

	bad := scanBrokenVulkanDrivers()

	brokenLock.Lock()
	brokenKnown, brokenSeen = bad, true
	brokenLock.Unlock()
	return bad
}

func scanBrokenVulkanDrivers() []string {
	var bad []string
	for _, dir := range []string{
		"/usr/share/vulkan/icd.d", "/etc/vulkan/icd.d",
		"/usr/local/share/vulkan/icd.d", "/usr/local/etc/vulkan/icd.d",
	} {
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			l := strings.ToLower(e.Name())
			if strings.Contains(l, "lvp") || strings.Contains(l, "llvmpipe") || strings.Contains(l, "swiftshader") ||
				strings.Contains(l, "gfxstream") || strings.Contains(l, "virtio") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			var manifest struct {
				ICD struct {
					LibraryPath string `json:"library_path"`
				} `json:"ICD"`
			}
			if json.Unmarshal(b, &manifest) != nil || manifest.ICD.LibraryPath == "" {
				continue
			}
			lib := manifest.ICD.LibraryPath
			if !filepath.IsAbs(lib) && strings.Contains(lib, "/") {
				lib = filepath.Join(dir, lib)
			}
			h, err := purego.Dlopen(lib, purego.RTLD_NOW)
			if err != nil {
				bad = append(bad, fmt.Sprintf("%s: %s cannot load: %v", e.Name(), lib, err))
				continue
			}
			_ = h
		}
	}
	return bad
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// captionDirPersistent reports whether the caption directory survives the
// container being recreated.
//
// It is a plain directory inside the image unless the compose file binds it to
// the host, and a container that is brought down and up again starts from the
// image. Downloading several hundred megabytes into somewhere that evaporates
// is a miserable way to find that out, so it is checked and said out loud.
func captionDirPersistent() (bool, string) {
	abs, err := filepath.Abs(captionDir)
	if err != nil {
		return true, ""
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		// Not a Linux container; nothing to warn about.
		return true, ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// The mount point is the fifth field.
		fields := strings.Fields(sc.Text())
		if len(fields) >= 5 && fields[4] == abs {
			return true, ""
		}
	}
	return false, abs
}

// renderNodes lists the graphics devices visible inside the container.
//
// A container gets no device nodes unless the compose file passes them, so this
// is usually the reason a GPU build sees nothing: the driver is installed, the
// card is in the machine, and /dev/dri simply is not here.
func renderNodes() []string {
	ents, err := os.ReadDir("/dev/dri")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "render") || strings.HasPrefix(e.Name(), "card") {
			out = append(out, "/dev/dri/"+e.Name())
		}
	}
	return out
}

// accelReport is what the page shows about hardware acceleration: not just
// whether it was asked for, but whether each thing it depends on is actually
// there.
type accelReport struct {
	Variant  string   `json:"variant"`
	Active   bool     `json:"active"`
	Headline string   `json:"headline"`
	Detail   string   `json:"detail"`
	Devices  []string `json:"devices"`
}

// accelStatus works out whether the GPU is really going to be used, and if not,
// which of the pieces is missing.
func accelStatus() accelReport {
	cfg := currentCaptionConfig()
	v, ok := findEngineVariant(cfg.Engine)
	r := accelReport{Variant: cfg.Engine, Devices: renderNodes()}
	if !ok || v.Key == "cpu" {
		r.Headline = "Running on the processor"
		r.Detail = "No GPU build is selected. This is fine for a few tuners at once; a GPU build spares the cores and the heat when many are running."
		return r
	}
	switch {
	case !engineUsable(v):
		r.Headline = "Not accelerated: the driver is missing"
		r.Detail = fmt.Sprintf("%s is selected but %s will not load. Download the driver below.", v.Name, v.Needs)
	case v.Key == "vulkan" && len(r.Devices) == 0:
		r.Headline = "Not accelerated: no graphics device in the container"
		r.Detail = "The driver is in place but /dev/dri is not here. Add a devices entry for /dev/dri to your compose file and recreate the container."
	case !engineInstalled():
		r.Headline = "Not accelerated: the engine build is not downloaded"
		r.Detail = fmt.Sprintf("Download the %s build above.", v.Name)
	case driverFaultNow() != "":
		// The loader loading says nothing about the drivers behind it. This is
		// the state a half-complete package set installs into: everything above
		// passes, the loader offers no devices, and without this the page went
		// green while every stream ran on the processor.
		r.Headline = "Not accelerated: the graphics driver cannot load"
		r.Detail = driverFaultNow()
	case txStarted() && !txBackendAvailable(txBackend(v.Key)):
		r.Headline = "Not accelerated: the engine started before " + v.Name + " was ready"
		r.Detail = "Everything it needs is here now, but the engine looks for its backends once, when it first loads, and that had already happened. Restart the container to caption on the GPU."
	default:
		r.Active = true
		r.Headline = "Hardware acceleration is active: " + v.Name
		if len(r.Devices) > 0 {
			r.Detail = "Using " + strings.Join(r.Devices, ", ") + ". The engine falls back to the processor by itself if the device stops answering, so captions keep working either way."
		} else {
			r.Detail = "The engine falls back to the processor by itself if the device stops answering, so captions keep working either way."
		}
	}
	return r
}

// gpuAvailable reports whether any GPU build could actually run here: its
// driver loads, and for Vulkan a graphics device is present as well. It asks
// the same questions accelStatus does, but about every build rather than the
// selected one, because what it decides is whether a model that cannot work
// without a GPU is offered at all.
//
// It deliberately does not care whether that build has been downloaded yet.
// Refusing to show a model because the engine it would use is still a button
// press away would be a maze rather than a gate.
// gpuReady is gpuAvailable's answer, kept where the tune path can read it
// without asking.
//
// gpuAvailable calls engineUsable, and engineUsable dlopens the library when its
// cache is cold: libvulkan and the whole Mesa chain, every symbol resolved
// eagerly, off a disk that may be cold. That is seconds, and maybeWrapCaptions
// was calling it on the tune path while the global tuner lock was held — so the
// tune, and every tune queued behind it, waited for a driver probe.
//
// The cache made this rare rather than impossible, and installing a driver
// empties it on purpose, because the answer really does change when one goes in.
// So the first captioned tune after any install paid the full cost: on a
// container start that is the first tune of the night, which is exactly where
// playback confirmation was timing out, and only with captions on, because
// nothing else on that path asks this question.
//
// It is a stored answer now, refreshed off the tune path wherever the cache is
// emptied. False until something has looked, which costs nothing: the warm-up
// runs before the port is bound, so it has always looked by the time a tune can
// arrive.
var gpuReady atomic.Bool

// refreshGPUReady re-answers the question. Never call it from the tune path: it
// is the thing that dlopens.
func refreshGPUReady() {
	gpuReady.Store(gpuAvailable())
	refreshCaptionReady()
}

func gpuAvailable() bool {
	nodes := renderNodes()
	for _, v := range engineVariants {
		if v.Key == "auto" || v.Key == "cpu" || !engineUsable(v) {
			continue
		}
		if v.Key == "vulkan" && len(nodes) == 0 {
			continue
		}
		return true
	}
	return false
}

// driverRestoreDone closes when the startup driver restore has finished (or
// found nothing to do). The engine's first initialization waits on it, so a
// fresh container does not open the Vulkan library half-installed and settle
// on the processor for the life of the process.
var driverRestoreDone = make(chan struct{})

// serving reports whether ah4c has opened its door yet.
//
// This is the difference between quiet that is observed and quiet that is
// guaranteed, and every gate in this file has been arguing about the former for
// want of the latter. Startup is the one stretch in a container's life where a
// tune cannot arrive: the port is not bound, so the DVR gets connection refused
// rather than a request nobody answers. Not "the machine looks quiet" — the
// door is shut.
//
// So while this is false the gates stop guessing and say yes. Nothing they are
// protecting against can happen yet.
var serving atomic.Bool

// driverRestoreBudget is how long startup will wait for the driver before
// coming up without it.
//
// It has to be finite. A container that cannot install its driver must still
// answer the DVR — ah4c that never listens is worse than ah4c with no graphics
// acceleration, by a distance. Restoring an already-downloaded set is
// thirty-odd packages of a second or two, so this is roughly double what it
// takes and nowhere near what it costs to be wrong.
const driverRestoreBudget = 2 * time.Minute

// restoreGPURuntime puts the graphics driver back, and does it before ah4c
// starts serving.
//
// This used to return immediately and install in the background behind the tune
// gate, on the reasoning that a container which has just restarted faces every
// recording the DVR wants back at once and the server should come up first.
// Both halves of that were true and the conclusion was still wrong, because it
// put the one piece of un-pausable work in this program into a permanent race
// with the tunes it must not interrupt — and the gate that was supposed to
// referee the race could only ever report on the instant it was asked.
//
// Running it here removes the race rather than refereeing it. main calls this
// before it binds the port, so for as long as this takes there is no tune to
// interrupt and no gate to satisfy: dpkg can fsync its way through the whole
// set at full speed and the worst it can do is delay the door opening.
//
// Bounded, because it is now in front of everything. If the install has not
// finished inside the budget, ah4c comes up regardless and the rest of it
// carries on behind the gates like before.
func restoreGPURuntime() {
	// The door opens the moment this returns, whichever way it returns.
	defer serving.Store(true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		restoreGPURuntimeQuietly()
	}()
	select {
	case <-done:
	case <-time.After(driverRestoreBudget):
		logger("[CC] The graphics driver has been going in for %s and is not finished. ah4c is coming up now; the rest of it waits for quiet like everything else does.", driverRestoreBudget)
	}
}

// releaseDriverWaiters lets the engine's first open proceed. Called from more
// than one place and possibly more than once, so it closes exactly once:
// closing a channel twice is a panic that takes every tuner with it.
func releaseDriverWaiters() {
	driverReleased.Do(func() { close(driverRestoreDone) })
}

var (
	driverReleased   sync.Once
	driverRestoreRun sync.Once
)

func restoreGPURuntimeQuietly() {
	defer releaseDriverWaiters()
	if runtime.GOOS != "linux" {
		return
	}
	// A graphics driver is for captioning and nothing else here, so a container
	// with captions switched off does not install one. It is not free: on a
	// fresh bind mount it is a package at a time through dpkg, and that is
	// forty seconds of a startup nobody asked for.
	//
	// It is not simply skipped, though. This is the only stretch of a
	// container's life where nothing can be tuning, so refusing to install here
	// means the install has to happen later, beside live tunes, which is the
	// case that has cost recordings. Switching captions on is what asks for it,
	// and that is where it now runs from — gated and divided, as it has to be
	// once the door is open.
	if !currentCaptionConfig().Enabled {
		logger("[CC] Captions are off, so the graphics driver is left where it is. It goes in when captions are switched on.")
		warmEngineCache()
		return
	}
	restoreSavedDriver()
}

// restoreSavedDriver puts the saved driver back, at most once per process.
func restoreSavedDriver() {
	driverRestoreRun.Do(restoreSavedDriverNow)
}

func restoreSavedDriverNow() {
	// Before the quiet gate, only the check that costs nothing: is anything
	// saved at all. One directory read. The common case — no GPU driver in
	// use — settles instantly and the engine init never waits.
	saved := false
	for _, g := range gpuRuntimes {
		if driverDownloaded(g) {
			saved = true
			continue
		}
		// Some packages but not all of them is a set a failed download left
		// behind. It cannot be restored into anything that works, and the
		// fix needs a button on the page, so say so rather than restoring
		// silence.
		if ents, err := os.ReadDir(driverDir(g)); err == nil {
			for _, e := range ents {
				if !e.IsDir() && strings.HasSuffix(e.Name(), ".deb") {
					logger("[CC] The saved %s packages are incomplete — a download must have failed partway. Press the driver download on the Closed Captions page to fetch a fresh set.", g.Name)
					break
				}
			}
		}
	}
	if !saved {
		warmEngineCache()
		return
	}
	// The install never runs while a tune is in flight. Never, not "unless it
	// has been waiting a while".
	//
	// This waited forty seconds for a quiet stretch and then went ahead
	// anyway, on the theory that nice and ionice made that safe. They do not.
	// dpkg fsyncs its way through thirty-seven packages and an array does not
	// care what priority the process asking is; the tune that was running
	// missed its playback confirmation and the recording died. That was my
	// fallback, not the gate — the gate was working, and I overrode it.
	//
	// It was bounded because the engine's one-time open waits on this
	// finishing, and an unbounded wait starved captions completely. So the two
	// are separated instead. If no quiet stretch turns up in the first forty
	// seconds, the engine is released to start without the driver — captions
	// run on the processor for this session and say so — while the install
	// goes on waiting for a moment when it can run without costing anybody a
	// recording. It takes effect at the next container start, because the
	// engine scans for backends once and has already done it by then.
	//
	// A driver is a convenience. A recording is not.
	//
	// On the ordinary path none of that applies any more: this runs before the
	// port is bound, the gate answers yes immediately because nothing can be
	// tuning, and the install is finished before anybody could have asked. All
	// of the above is what happens when it overruns the startup budget and finds
	// itself running beside real tunes after all — the case this was written
	// for, now the exception rather than the rule.
	if !waitTuneQuietHeld(10*time.Second, 40*time.Second) {
		logger("[CC] No quiet stretch for the graphics driver install yet. Captions will run on the processor this session; the driver goes in when the machine is idle and is used from the next start.")
		releaseDriverWaiters()
		// Five more minutes of looking for a quiet stretch, and then it goes in
		// anyway. Everything below is divided a package at a time and runs
		// behind nice and ionice, so proceeding is a second of polite work per
		// package rather than the un-pausable install this gate was written
		// for. Waiting for ever is the worse of the two: a machine that never
		// goes quiet is a machine that never gets its driver, and captions
		// spend the rest of the container's life on the processor.
		awaitQuiet("The graphics driver restore", 10*time.Second, 5*time.Minute)
	}
	need := false
	for _, g := range gpuRuntimes {
		if driverDownloaded(g) && (!driverActive(g) || len(brokenVulkanDrivers()) > 0) {
			need = true
		}
	}
	if need {
		if !serving.Load() {
			logger("[CC] Putting the saved graphics driver back before the web server starts. Nothing can be tuning yet, so this is the one time it costs nobody anything; ah4c answers as soon as it is done.")
		}
		reinstallSavedDriver()
	}
	warmEngineCache()
	// The engine scans for backends once per process. If it already ran that
	// scan while the driver was broken, this repair cannot reach it until the
	// process restarts — which deserves saying plainly, because from outside
	// it looks like a fixed driver being ignored.
	if txStarted() && len(brokenVulkanDrivers()) == 0 {
		if v := currentEngineVariant(); strings.Contains(v, "vulkan") && !txBackendAvailable(txBackendVulkan) {
			logger("[CC] The graphics driver is repaired, but the engine had already started without it. Restart the container to caption on the GPU.")
		}
	}
}

// awaitDriverRestore blocks until the startup driver restore has finished, or
// until waiting stops being worth it.
//
// Bounded, because a restore that cannot find its quiet stretch may take
// minutes and a stream should not be silent for all of them. Two minutes
// matches what the engine's own open allows, so a caller that waits here and
// then opens the engine cannot be caught out twice by the same delay.
func awaitDriverRestore() {
	select {
	case <-driverRestoreDone:
	case <-time.After(2 * time.Minute):
		logger("[CC] The graphics driver is still being put back after two minutes; choosing a backend with whatever loads now")
	}
}

// txInited reports whether the engine's one-time initialization has run.
func txInited() bool {
	select {
	case <-txInitedCh:
		return true
	default:
		return false
	}
}

// txInitedCh closes when initTranscribe's Once has completed.
var txInitedCh = make(chan struct{})

// txStarted reports whether the engine is up and can be asked questions.
//
// txInited is not enough on its own: initialization that gives up early — no
// build published here, nothing downloaded yet — still closes the channel on
// its way out, having registered none of the entry points. Calling one of them
// on that path is a nil call, which takes the process down.
func txStarted() bool {
	return txInited() && txErr == nil && txBackendAvailable != nil
}

// warmEngineCache primes the which-builds-can-load cache off the tune path.
// The first tune otherwise pays those dlopens itself — under the tuner lock,
// where one slow driver chain delays every tuner's tune.
func warmEngineCache() {
	// Held quiet, not observed quiet. This opens driver libraries, and a dlopen
	// cannot be stopped once it starts, so it is the uninterruptible work the
	// first rule is about. The plain wait does not do it: before the first tune
	// of a container's life the machine reads as quiet because nothing has
	// asked yet, and this runs before the server is even listening — so it
	// answered instantly and started loading drivers into the storm.
	//
	// Bounded, and it proceeds if the minute runs out. What it is protecting
	// against is a dlopen chain landing inside a tune, which is a fraction of a
	// second; what waiting for ever costs is the cache never being primed at
	// all, which puts that same dlopen chain on the first tune instead, under
	// the tuner lock, where it delays every tuner rather than none.
	awaitQuiet("The engine cache warm-up", 10*time.Second, time.Minute)
	// Throw away whatever was learned before this point first.
	//
	// The driver restore waits for the machine to go quiet, so on a fresh
	// container there is a stretch — a minute, sometimes several — where the
	// Vulkan loader genuinely is not installed yet. Anything that asks during
	// that stretch gets a truthful "no" and the cache keeps it for the life of
	// the process, so the driver arriving a moment later changes nothing: the
	// page still says the loader will not load, the engine picker still refuses
	// to offer a GPU build, and everything runs on the processor until somebody
	// recreates the container.
	//
	// This is why it showed up on Intel and nowhere else. An NVIDIA card gets
	// its loader injected by the container runtime before ah4c starts, so the
	// answer is the same whenever it is asked. An Intel or AMD chip depends on
	// exactly the packages this restore puts back, which is the only case where
	// the answer changes underneath the cache.
	//
	// So the moment the restore is done is the moment to forget, and priming
	// straight afterwards means nothing is left to ask a stale question of.
	// Built beside the old answers rather than on top of the space where they
	// were. Emptying the cache and refilling it leaves a window in which it is
	// empty, and a tune arriving in that window pays for the driver load
	// itself — which is the one thing this function exists to prevent.
	fresh := map[string]bool{}
	for _, v := range engineVariants {
		if v.Key == "auto" || v.Needs == "" {
			continue
		}
		h, err := purego.Dlopen(v.Needs, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		fresh[v.Needs] = err == nil && h != 0
	}
	usableLock.Lock()
	usableCache = fresh
	usableLock.Unlock()
	// The tune path reads the stored answer and never asks. This is where it is
	// answered, before the port is bound.
	refreshGPUReady()
}

// reinstallSavedDriver puts the driver back after a container rebuild, from
// the copy in the bind mount, so the choice survives without anyone pressing
// anything or the network being reachable.
func reinstallSavedDriver() {
	if runtime.GOOS != "linux" {
		return
	}
	cfg := currentCaptionConfig()
	for _, g := range gpuRuntimes {
		// Anything with packages saved in the bind mount gets put back, whether
		// or not the config remembers asking for it. The packages being there
		// is the intent; a rebuild wipes the installed copy but not them.
		if !driverDownloaded(g) || driverActive(g) {
			continue
		}
		logger("[CC] %s is saved but not loaded, restoring it from %s", g.Name, driverDir(g))
		if out, err := applyDriver(g); err != nil {
			logger("[CC] %s could not be restored: %v %s", g.Name, err, tailLines(out, 6))
			continue
		}
		logger("[CC] %s restored", g.Name)
		if cfg.GPURuntime != g.Key {
			cfg.GPURuntime = g.Key
			saveCaptionConfig(cfg)
		}
	}
}

// ---------------------------------------------------------------------------

/*

Copyright 2023 Fancy Bits, LLC

Permission is hereby granted, free of charge, to any person obtaining a copy of this software and associated documentation files (the “Software”), to deal in the Software without restriction, including without limitation the rights to use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of the Software, and to permit persons to whom the Software is furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED “AS IS”, WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.

*/

package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"html/template"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/smtp"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
	gopsutilNet "github.com/shirou/gopsutil/v4/net"
	"gopkg.in/natefinch/lumberjack.v2"
)

// tuners
var (
	tunerLock sync.Mutex
	tuners    []tuner
)

// Misc
var (
	envdebug     bool = true
	allowPreview bool = false
)

// /status page reader handling
var (
	activeReaders []*reader
	readersLock   sync.Mutex
)

// All tuners
type tuner struct {
	url      string
	pre      string
	start    string
	stop     string
	tunerip  string
	reboot   string
	cmd      string
	active   bool
	filePath string
	index    int
	teecmd   string
}

// All readers
type reader struct {
	io.ReadCloser
	t             *tuner
	channel       string
	started       bool
	cmd           *exec.Cmd
	file          *os.File
	teecmdIn      io.WriteCloser
	cmdMutex      sync.Mutex
	teecmdRunning bool
	port          int
	teecmd        *exec.Cmd
	gateReady     chan struct{}
	gateDone      chan struct{}
	gateStop      sync.Once
	gateBase      map[string]bool
	gateSig       string
	gate          *gateReader
	startedAt     time.Time
}

// Create a global file object to write logs to
var loggerhandle *log.Logger

// status page type
type ExportedTuner struct {
	Tunerip string
	Url     string
	Active  bool
}

// status page type
type ExportedReader struct {
	T        int
	Channel  string
	Name     string
	Started  string
	Elapsed  int64
	FileName string
	Cmd      string
}

type Entry struct {
	Id            string `json:"id"`
	StationId     string `json:"stationId"`
	ChannelName   string `json:"channelName"`
	ChannelNumber string `json:"channelNumber"`
	StreamURL     string `json:"streamURL"`
	Logo          string `json:"Logo"`
	Group         string `json:"Group"`
}

type ConfigEnvVariable struct {
	Key   string
	Value string
}

type ConfigTuner struct {
	Number     string
	Cmd        string
	EncoderUrl string
	TunerIp    string
}

type ConfigData struct {
	EnvVariables []ConfigEnvVariable
	Tuners       []ConfigTuner
}

// Early init called before main
func init() {
	// Intitalize HTTP Transport
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 5 * time.Second
	transport.DialContext = (&net.Dialer{
		Timeout: 5 * time.Second,
	}).DialContext
	http.DefaultClient.Transport = transport
	// Intitalize logging subsystem
	loggerhandle = log.New(&lumberjack.Logger{
		Filename:   "/tmp/ah4c.log",
		MaxSize:    25,   // megabytes
		MaxBackups: 3,    // maximum backups
		MaxAge:     28,   // days
		Compress:   true, // enabled by default
	}, "", log.LstdFlags)
}

func (r *reader) startTeeCMD() error { // Removed the readers argument
	if r.t.teecmd == "" {
		return nil
	}
	r.cmdMutex.Lock()
	defer r.cmdMutex.Unlock()
	// Check if TEECMD is already running
	if r.teecmdRunning {
		return nil
	}
	// Find the next available port number starting at 4444
	nextPort := 4444
	for _, existingReader := range activeReaders {
		if existingReader.port >= nextPort {
			nextPort = existingReader.port + 1
		}
	}
	r.port = nextPort // Set the port in the reader
	// Start ffmpeg with the new command
	logger("Starting TEECMD %s", r.t.teecmd)
	// Execute command and assign stdin stdout stderr
	cmdparts := strings.Fields(r.t.teecmd)
	cmd := exec.Command(cmdparts[0], cmdparts[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to get stdin pipe for TEECMD: %v", err)
	}
	r.teecmdIn = stdin
	r.teecmd = cmd
	r.teecmdRunning = true
	logger("TEECMD has started")
	// Attach stderr and stdout
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("[ERR] Failed to start TEECMD: %v", err)
	}
	// Monitor TEECMD process and restart if needed
	go func() {
		cmd.Wait()
		r.cmdMutex.Lock()
		r.teecmd = nil
		r.teecmdIn = nil
		r.teecmdRunning = false
		r.cmdMutex.Unlock()
		if err := r.startTeeCMD(); err != nil {
			fmt.Printf("[ERR] Failed to restart TEECMD: %v\n", err)
		}
	}()
	return nil
}

// Called from io.Copy when reading socket data
func (r *reader) Read(p []byte) (int, error) {
	if !r.started {
		r.started = true
		addReader(r)
		go func() {
			base := r.gateBase
			if err := execute(r.t.start, r.channel, r.t.tunerip); err != nil {
				logger("[ERR] Failed to run start script: %v", err)
				if r.gateReady != nil {
					close(r.gateReady)
				}
				return
			}
			if r.gateReady != nil {
				if base != nil {
					if waitForPlayback(r.t.tunerip, base, r.gateSig, r.gateDone) && r.gate != nil {
						r.gate.expectNewStream()
					}
				} else {
					logger("[PLAYBACK] %s no audio baseline, gating on motion alone", r.t.tunerip)
				}
				close(r.gateReady)
			}
		}()
	}
	// Determine the index of the tuner
	tunerIndex := -1
	for index := range tuners {
		if &tuners[index] == r.t {
			tunerIndex = index
			break
		}
	}
	if tunerIndex == -1 {
		return 0, fmt.Errorf("tuner not found")
	}
	// Create the file if it doesn't exist
	if r.file == nil && allowPreview {
		filePath := fmt.Sprintf("/tmp/video_%d.ts", tunerIndex)
		file, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return 0, fmt.Errorf("error opening file: %v", err)
		}
		r.file = file
		r.t.filePath = filePath
	}
	if r.t.teecmd != "" {
		if err := r.startTeeCMD(); err != nil {
			return 0, fmt.Errorf("[ERR] Failed to start TEECMD: %v", err)
		}
	}
	// Read from the source
	n, err := r.ReadCloser.Read(p)
	// Write out to preview file if enabled
	if allowPreview || r.t.teecmd != "" {
		data := make([]byte, n)
		copy(data, p[:n])
		if allowPreview {
			go func() {
				// Write to file
				if _, err := r.file.Write(data); err != nil {
					logger("Error while writing to preview file")
				}
			}()
		}
		// Write to TEECMD if enabled
		if r.t.teecmd != "" {
			go func() {
				if _, err := r.teecmdIn.Write(data); err != nil {
					logger("Error while writing to TEECMD")
				}
			}()
		}
	}
	return n, err
}

// Called from io.Copy when closing socket
func (r *reader) Close() error {
	logger("Performing Close() for %s", r.t.tunerip)
	if r.gateDone != nil {
		r.gateStop.Do(func() { close(r.gateDone) })
	}
	if r.cmd != nil {
		// If there's a command running, terminate it.
		if err := r.cmd.Process.Kill(); err != nil {
			logger("[ERR] Failed to kill command: %v", err)
		}
	}
	if err := execute(r.t.stop, r.t.tunerip, r.channel); err != nil {
		logger("[ERR] Failed to run stop script: %v", err)
		execute(r.t.reboot, r.t.tunerip, r.channel)
	}
	tunerLock.Lock()
	r.t.active = false
	tunerLock.Unlock()
	if allowPreview {
		r.file.Close()
		// Construct the file path based on the tuner
		filePath := fmt.Sprintf("/tmp/video_%d.ts", r.t.index)
		// Delete the video file
		if err := os.Remove(filePath); err != nil {
			logger("[ERR] Failed to remove video file: %v", err)
		}
	}
	r.cmdMutex.Lock()
	if r.teecmd != nil && r.teecmdRunning {
		if err := r.teecmd.Process.Kill(); err != nil {
			logger("Error killing ffmpeg process: %v", err)
		} else {
			logger("ffmpeg process killed")
		}
		r.teecmd = nil
		r.teecmdRunning = false
	}
	// Close the ffmpeg input pipe
	if r.teecmdIn != nil {
		if err := r.teecmdIn.Close(); err != nil {
			logger("Error closing TEECMD input pipe: %v", err)
		}
		r.teecmdIn = nil
	}
	r.cmdMutex.Unlock()
	removeReader(r)
	return r.ReadCloser.Close()
}

func parseCommand(cmd string) []string {
	var args []string
	var currentArg string
	inQuotes := false
	for _, c := range cmd {
		switch c {
		case ' ':
			if inQuotes {
				currentArg += string(c)
			} else if currentArg != "" {
				args = append(args, currentArg)
				currentArg = ""
			}
		case '\'':
			inQuotes = !inQuotes
		default:
			currentArg += string(c)
		}
	}
	if currentArg != "" {
		args = append(args, currentArg)
	}
	return args
}

// Tune into a application or network encoder. early is the pre-roll already
// playing for this request, for the hold to carry on with, or nil.
func tune(idx, channel string, early *earlyTune) (io.ReadCloser, error) {
	tunerLock.Lock()
	defer tunerLock.Unlock()
	intidx, _ := strconv.Atoi(idx)
	var t *tuner
	for i, ti := range tuners {
		if i == intidx || idx == "" || idx == "auto" {
			if ti.active {
				logger("Tuner %d is active - skipping", i)
				continue
			}
			t = &tuners[i]
			// Handle application encoder
			if t.cmd != "" {
				logger("Attempting application tune for device %s %v", t.cmd, idx)
				cmdAndArgs := parseCommand(t.cmd)
				cmd := exec.Command(cmdAndArgs[0], cmdAndArgs[1:]...)
				pipeReader, pipeWriter := io.Pipe()
				cmd.Stdout = pipeWriter
				cmd.Stderr = os.Stderr
				err := cmd.Start()
				if err != nil {
					logger("[ERR] Failed to run command %s", err)
					t.active = false
					continue
				}
				go func() {
					cmd.Wait()
					pipeWriter.Close()
				}()
				if err := execute(t.pre, t.tunerip, channel); err != nil {
					logger("[ERR] Failed to run pre script: %v", err)
					t.active = false
					continue
				}
				t.active = true
				t.index = i
				return &reader{
					ReadCloser: maybeWrapCaptions(pipeReader, i, fmt.Sprintf("tuner%d", i)),
					channel:    channel,
					t:          t,
					cmd:        cmd,
				}, nil
			}
			// Network encoder
			logger("Attempting network tune for device %s %s %v %v", t.url, t.tunerip, channel, idx)
			tuneStart := time.Now()
			var ready chan struct{}
			var base map[string]bool
			var sig string
			// The delay decides when the program starts, so playback detection
			// does not run at all alongside it: no adb baseline before the
			// tune, and nothing polling the box while the hold is in flight.
			if holdDelay == 0 && strings.EqualFold(os.Getenv("PLAYBACK_DETECTION"), "TRUE") {
				ready = make(chan struct{})
				base = audioBaseline(t.tunerip)
				sig = mediaSignature(t.tunerip)
			}
			if err := execute(t.pre, t.tunerip, channel); err != nil {
				logger("[ERR] Failed to run pre script: %v %s", err, t.tunerip)
				t.active = false
				continue
			}
			label := fmt.Sprintf("tuner=%s", t.tunerip)
			var body io.ReadCloser
			var gate *gateReader
			if holdDelay > 0 {
				// A tune held by the delay does not open the encoder yet: the
				// wait is the pre-roll or NULL packets, and the encoder is
				// opened when the delay is up, so the program starts at the
				// top of its own session rather than joined in progress.
				body = newLateEncoder(t.url, label, early.from(tuneStart), early.player(), i, fmt.Sprintf("tuner%d", i))
			} else {
				resp, err := http.Get(t.url)
				if err != nil {
					logger("[ERR] Failed to fetch source: %v", err)
					t.active = false
					continue
				} else if resp.StatusCode != 200 {
					logger("[ERR] Failed to fetch source: %v", resp.Status)
					resp.Body.Close()
					t.active = false
					continue
				}
				// NULL_FRAME_INSERTION=TRUE (case-insensitive): fill encoder stalls with MPEG-TS NULLs so DVR never sees a zero-byte gap.
				body = resp.Body
				if strings.EqualFold(os.Getenv("NULL_FRAME_INSERTION"), "TRUE") {
					body = newStallTolerantReader(resp.Body, func() (io.ReadCloser, error) {
						r, e := http.Get(t.url)
						if e != nil {
							return nil, e
						}
						if r.StatusCode != 200 {
							r.Body.Close()
							return nil, fmt.Errorf("status %s", r.Status)
						}
						return r.Body, nil
					}, label)
				}
				// The gate holds the stream back until the hold says so:
				// playback detection with a pre-roll to show while it waits.
				hold := newTuneHold(tuneStart, ready, label, early.player())
				if hold != nil {
					gate = newGateReader(body, hold.ready, false, time.Time{}, ready)
					body = gate
				}
				body = hold.wrap(maybeWrapCaptions(body, i, fmt.Sprintf("tuner%d", i)))
			}
			t.active = true
			t.index = i
			r := &reader{
				ReadCloser: body,
				channel:    channel,
				t:          t,
				gateReady:  ready,
				gateDone:   make(chan struct{}),
				gateBase:   base,
				gateSig:    sig,
				gate:       gate,
			}
			return r, nil
		}
	}
	return nil, fmt.Errorf("device(s) not available")
}

// Custom execute command with timing stats
func execute(args ...string) error {
	t0 := time.Now()
	logger("[EXECUTE] Running %v", args)
	cmd := exec.Command(args[0], args[1:]...)
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	err := cmd.Run()
	outStr, errStr := stdoutBuf.String(), stderrBuf.String()
	logger("[EXECUTE] Stdout: '%s'", outStr)
	logger("[EXECUTE] Stderr: '%s'", errStr)
	logger("[EXECUTE] Finished running %v in %v", args[0], time.Since(t0))
	return err
}

// GIN custom logging middleware
func CustomLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Skip logging for the /getlogs route
		if c.Request.URL.Path == "/logs/text" {
			c.Next()
			return
		}
		// Skip logging for the /api/status route
		if c.Request.URL.Path == "/api/status" {
			c.Next()
			return
		}
		// Skip logging for the /status/channelsactivity route
		if c.Request.URL.Path == "/status/channelsactivity" {
			c.Next()
			return
		}
		if c.Request.URL.Path == "/api/captions" {
			c.Next()
			return
		}
		// Process request and log it
		t := time.Now()
		// Call the next handler in the chain
		c.Next()
		// Log the request
		latency := time.Since(t)
		clientIP := c.ClientIP()
		logger("[GIN-debug] Request: %s %s %s, latency: %s, status: %d",
			clientIP, c.Request.Method, c.Request.URL, latency, c.Writer.Status())
	}
}

// If GIN panics, try to recover.
func CustomRecovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if err := recover(); err != nil {
				// Log the panic error and stack trace
				logger("[PANIC] Panic failure recovery -> %s\n", err)
				buf := make([]byte, 1<<16)
				stackSize := runtime.Stack(buf, true)
				logger("[PANIC] Failure stack: %s\n", string(buf[0:stackSize]))
				// Send a custom response to the client
				c.JSON(http.StatusInternalServerError, gin.H{
					"error": "Internal server error",
				})
			}
		}()
		c.Next()
	}
}

// Called every 30 mins to report current working conditions
func stats() {
	// CPU usage.
	cpuPercent, _ := cpu.Percent(0, false)
	logger("[STATS] CPU usage: %v%%", cpuPercent[0])
	// Memory usage.
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	logger("[STATS] Alloc = %v MiB", bToMb(m.Alloc))
	logger("[STATS] TotalAlloc = %v MiB", bToMb(m.TotalAlloc))
	logger("[STATS] Sys = %v MiB", bToMb(m.Sys))
	logger("[STATS] NumGC = %v", m.NumGC)
	// Total memory.
	v, _ := mem.VirtualMemory()
	logger("[STATS] Total memory: %v MiB", bToMb(v.Total))
	logger("[STATS] Memory used: %v MiB", bToMb(v.Used))
	logger("[STATS] Memory used percent: %v%%", v.UsedPercent)
	// NVIDIA stats if present
	_, err := exec.Command("which", "nvidia-smi").Output()
	if err != nil {
		return
	}
	// Execute nvidia-smi
	out, err := exec.Command("nvidia-smi").Output()
	if err != nil {
		fmt.Printf("Error executing nvidia-smi: %v\n", err)
		return
	}
	output := string(out)
	// Extract fan speed
	fanRegex := regexp.MustCompile(`(?m)^[\|\s]*\d+%`)
	fanMatch := fanRegex.FindString(output)
	fanSpeed := strings.TrimSuffix(fanMatch, "%")
	// Extract GPU utilization
	utilRegex := regexp.MustCompile(`(?m)\d+%      Default`)
	utilMatch := utilRegex.FindString(output)
	gpuUtil := strings.TrimSuffix(strings.TrimSpace(utilMatch), "%      Default")
	// Extract memory usage
	memRegex := regexp.MustCompile(`(?m)\d+MiB /  \d+MiB`)
	memMatch := memRegex.FindString(output)
	memUsage := strings.TrimSpace(memMatch)
	// Extract power usage
	powerRegex := regexp.MustCompile(`(?m)\d+W / \d+W`)
	powerMatch := powerRegex.FindString(output)
	powerUsage := strings.TrimSpace(powerMatch)
	logger("[STATS] GPU Fan Speed: %s%%", fanSpeed)
	logger("[STATS] GPU Utilization: %s%%", gpuUtil)
	logger("[STATS] GPU Memory Usage: %s", memUsage)
	logger("[STATS] GPU Power Usage: %s", powerUsage)
}

func bToMb(b uint64) uint64 {
	return (b + 1024*1024 - 1) / 1024 / 1024
}

// Called from main()
func run() error {
	// Lets get to playing!
	r := gin.New()
	r.SetTrustedProxies(nil)
	r.Use(CustomLogger())
	r.Use(func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/static") {
			c.Header("Cache-Control", "no-store")
		}
		c.Next()
	})
	r.StaticFile("/favicon.ico", "./static/favicon.ico")
	//	r.Use(CustomRecovery())
	r.LoadHTMLGlob("html/*")
	r.StaticFS("/static", http.Dir("static"))
	r.GET("/", func(c *gin.Context) {
		r.LoadHTMLGlob("html/*")
		routes := r.Routes()
		c.HTML(http.StatusOK, "index.html", routes)
	})
	scrcpyProxy := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: "127.0.0.1:8000"})
	r.GET("/device", func(c *gin.Context) {
		c.HTML(http.StatusOK, "device.html", nil)
	})
	r.GET("/scrcpy", func(c *gin.Context) {
		loc := "/scrcpy/"
		if q := c.Request.URL.RawQuery; q != "" {
			loc += "?" + q
		}
		c.Redirect(http.StatusMovedPermanently, loc)
	})
	r.Any("/scrcpy/*proxyPath", func(c *gin.Context) {
		c.Request.URL.Path = c.Param("proxyPath")
		c.Request.URL.RawPath = strings.TrimPrefix(c.Request.URL.RawPath, "/scrcpy")
		scrcpyProxy.ServeHTTP(c.Writer, c.Request)
	})
	r.GET("/routes", func(c *gin.Context) {
		r.LoadHTMLGlob("html/*")
		routes := r.Routes()
		c.HTML(http.StatusOK, "routes.html", routes)
	})
	// Play tuner / channel from network or app
	r.GET("/play/tuner:tuner/:channel", func(c *gin.Context) {
		tuner := c.Param("tuner")
		channel := c.Param("channel")
		reader, err := tuneEarly(tuner, channel)
		if err != nil {
			logger("[ERR] Failed to tune %s", err)
			errorMessage := fmt.Sprintf("<html><body><h1>Error: %s</h1></body></html>", err.Error())
			c.Data(500, "text/html; charset=utf-8", []byte(errorMessage))
			return
		}
		// Closing the reader is what releases the tuner, runs the stop script
		// and closes the encoder's connection, so every path must reach it.
		defer reader.Close()
		starttime := time.Now()
		var bytesCopied int64
		// The first stretch of a hold goes out as 1xx, which puts nothing in
		// the body; the body carries whatever is left, however long that is.
		h, taken := holdOnHints(c.Writer, reader, tuner, channel)
		switch {
		case h != nil:
			defer h.Close()
			if bytesCopied, err = h.stream(reader); err != nil {
				logger("[IO] stream: %v", err)
			}
			return
		case taken:
			logger("[HOLD] tuner=%s channel=%s the DVR left during the hold", tuner, channel)
			return
		}
		c.Header("Transfer-Encoding", "identity")
		c.Header("Content-Type", "video/mp2t")
		c.Writer.WriteHeaderNow()
		c.Writer.Flush()
		if bytesCopied, err = copyFlush(c.Writer, reader); err != nil {
			logger("[IO] io.Copy: %v", err)
		}
		logger("[IOINFO] Successfully copied %v bytes", bytesCopied)
		elapsedtime := time.Since(starttime)
		speed := float64(bytesCopied) * 8 / elapsedtime.Seconds() / 1000000 // Convert from bytes/second to Mbits/second
		logger("[IOINFO] Transfer speed: %v Mbits/second", speed)
	})
	// Show m3u for provider and substitute template ip address
	r.GET("/m3u/:channel", func(c *gin.Context) {
		r.LoadHTMLGlob("m3u/*.m3u")
		channel := c.Param("channel")
		// Check if the file exists
		if _, errread := os.Stat("m3u/" + channel); errread == nil {
			// Get the proxy IP address used to rewrite m3u ip addresses
			IPADDRESS := os.Getenv("IPADDRESS")
			c.HTML(http.StatusOK, channel, gin.H{
				"IPADDRESS": IPADDRESS,
			})
		} else {
			logger("Could not find m3u file for %s", channel)
			return
		}
		r.LoadHTMLGlob("html/*")
	})
	// Show registered env variables
	r.GET("/env", func(c *gin.Context) {
		env := os.Environ()
		var envData string
		for _, val := range env {
			envData += val + "\n"
		}
		c.HTML(http.StatusOK, "env.html", gin.H{
			"EnvData": template.HTML("<pre>" + template.HTMLEscapeString(envData) + "</pre>"),
		})
	})
	// Show raw logs
	r.GET("/logs/text", func(c *gin.Context) {
		content, err := os.ReadFile("/tmp/ah4c.log")
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if n, err := strconv.Atoi(c.Query("tail")); err == nil && n > 0 {
			lines := strings.Split(strings.TrimRight(string(content), "\n"), "\n")
			if len(lines) > n {
				lines = lines[len(lines)-n:]
			}
			c.String(http.StatusOK, "%s", strings.Join(lines, "\n"))
			return
		}
		c.String(http.StatusOK, "%s", content)
	})
	r.GET("/logs", func(c *gin.Context) {
		c.HTML(http.StatusOK, "logs.html", nil)
	})
	r.GET("/status/andlogs", func(c *gin.Context) {
		IPADDRESS := os.Getenv("IPADDRESS")
		c.HTML(http.StatusOK, "status_and_logs.html", gin.H{"IPADDRESS": IPADDRESS})
	})
	// Show logs in json
	r.GET("/logs/json", func(c *gin.Context) {
		file, err := os.Open("/tmp/ah4c.log")
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Could not open log file: %v", err)})
			return
		}
		defer file.Close()
		content, err := io.ReadAll(file)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Could not read log file: %v", err)})
			return
		}
		lines := strings.Split(string(content), "\n")
		logEntries := make([]gin.H, 0, len(lines))
		for _, line := range lines {
			if line != "" {
				entry := gin.H{
					"log": line,
				}
				logEntries = append(logEntries, entry)
			}
		}
		c.JSON(http.StatusOK, logEntries)
	})
	// Used by /stream - read mpeg ts from filesystem
	r.GET("/video", func(c *gin.Context) {
		// Get the index query parameter, default to 0
		indexStr := c.DefaultQuery("index", "0")
		index, err := strconv.Atoi(indexStr)
		if err != nil {
			// handle error
			c.String(http.StatusBadRequest, "Invalid index")
			return
		}
		// Get the tuner based on the index
		if index < 0 || index >= len(tuners) {
			c.String(http.StatusBadRequest, "Index out of range")
			return
		}
		// Construct the file path based on the tuner
		filePath := fmt.Sprintf("/tmp/video_%d.ts", index)
		// Set the Content-Type header to video/MP2T
		c.Header("Content-Type", "video/MP2T")
		// Set CORS headers if needed
		c.Header("Access-Control-Allow-Origin", "*")
		// Serve the file
		c.File(filePath)
	})
	r.GET("/status", statusPageHandler)
	r.GET("/api/status", apiStatusHandler)
	r.GET("/api/version", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"version": buildVersion()})
	})
	r.GET("/api/pyatv", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"pyatv": strings.EqualFold(os.Getenv("PYATV"), "TRUE")})
	})
	r.GET("/captions", func(c *gin.Context) {
		c.HTML(http.StatusOK, "captions.html", nil)
	})
	r.GET("/api/captions", func(c *gin.Context) {
		c.JSON(http.StatusOK, captionStatusPayload())
	})
	r.POST("/api/captions", func(c *gin.Context) {
		cfg := currentCaptionConfig()
		if err := c.ShouldBindJSON(&cfg); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if _, ok := findCaptionModel(cfg.Model); !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unknown model"})
			return
		}
		if _, ok := findEngineVariant(cfg.Engine); !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unknown engine"})
			return
		}
		if err := saveCaptionConfig(cfg); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		logger("[CC] Settings saved: enabled=%v model=%s language=%s", cfg.Enabled, cfg.Model, cfg.Language)
		c.JSON(http.StatusOK, captionStatusPayload())
	})
	r.POST("/api/captions/driver/:kind", func(c *gin.Context) {
		if err := startDriverDownload(c.Param("kind")); err != nil {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "started"})
	})
	r.POST("/api/captions/runtime/:variant", func(c *gin.Context) {
		if err := startRuntimeDownload(c.Param("variant"), c.Query("model")); err != nil {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "started"})
	})
	r.POST("/api/captions/download/:model", func(c *gin.Context) {
		m, ok := findCaptionModel(c.Param("model"))
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unknown model"})
			return
		}
		if err := startModelDownload(m); err != nil {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "started"})
	})
	r.DELETE("/api/captions/model/:model", func(c *gin.Context) {
		m, ok := findCaptionModel(c.Param("model"))
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unknown model"})
			return
		}
		if err := removeCaptionModel(m); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, captionStatusPayload())
	})
	r.POST("/api/tuner/:index/control/:action", func(c *gin.Context) {
		index, err := strconv.Atoi(c.Param("index"))
		if err != nil || index < 0 || index >= len(tuners) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid tuner index"})
			return
		}
		action := c.Param("action")
		if _, ok := adbKeycodes[action]; !ok && action != "reboot" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unknown action"})
			return
		}
		if err := adbControl(tuners[index].tunerip, action); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	r.GET("/api/tuner/:index/preview", func(c *gin.Context) {
		index, err := strconv.Atoi(c.Param("index"))
		if err != nil || index < 0 || index >= len(tuners) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid tuner index"})
			return
		}
		if tuners[index].url == "" {
			c.JSON(http.StatusNotFound, gin.H{"error": "no encoder url for this tuner"})
			return
		}
		req, err := http.NewRequestWithContext(c.Request.Context(), "GET", tuners[index].url, nil)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("encoder returned %s", resp.Status)})
			return
		}
		c.Header("Content-Type", "video/mp2t")
		c.Writer.WriteHeaderNow()
		io.Copy(c.Writer, resp.Body)
	})
	r.POST("/api/tuner/:index/release", func(c *gin.Context) {
		index, err := strconv.Atoi(c.Param("index"))
		if err != nil || index < 0 || index >= len(tuners) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid tuner index"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": releaseTuner(index)})
	})
	r.POST("/api/tuners/control/:action", func(c *gin.Context) {
		action := c.Param("action")
		if _, ok := adbKeycodes[action]; !ok && action != "reboot" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unknown action"})
			return
		}
		skipped := 0
		for i := range tuners {
			tunerLock.Lock()
			busy := tuners[i].active
			tunerLock.Unlock()
			if busy {
				logger("[CONTROL] skipping %s for tuner %d, it is streaming", action, i)
				skipped++
				continue
			}
			go adbControl(tuners[i].tunerip, action)
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "skipped": skipped})
	})
	// Route for /stream - if video preview is enabled
	r.GET("/stream", func(c *gin.Context) {
		streamPageHandler(c)
	})
	// Route for /test/webhook
	r.GET("/test/webhook", func(c *gin.Context) {
		testcase := c.DefaultQuery("reason", "Testing Webhook")
		alertWebhook(testcase)
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("Attempting test webhook"))
	})
	// Route for /test/email
	r.GET("/test/email", func(c *gin.Context) {
		sendEmail("This is a test email from ah4c")
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("Attempting email testemail"))
	})
	r.GET("/status/channelsactivity", func(c *gin.Context) {
		var IPADDR string
		if os.Getenv("CHANNELSIP") != "" {
			IPADDR = os.Getenv("CHANNELSIP")
		} else {
			IPADDR = os.Getenv("IPADDRESS")
		}
		resp, err := http.Get("http://" + IPADDR + ":8089/dvr")
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), body)
	})
	r.GET("/edit", func(c *gin.Context) {
		// Read the contents of the file
		filePath := "./env"
		content, err := os.ReadFile(filePath)
		if err != nil {
			c.String(http.StatusInternalServerError, fmt.Sprintf("Failed to read file: %s", err.Error()))
			return
		}
		c.HTML(http.StatusOK, "edit.html", gin.H{
			"content": string(content),
		})
	})
	r.POST("/save", func(c *gin.Context) {
		// Get the modified content from the form
		content := c.PostForm("content")
		// Write the modified content to the file
		filePath := "./env"
		err := os.WriteFile(filePath, []byte(content), 0644)
		if err != nil {
			c.String(http.StatusInternalServerError, fmt.Sprintf("Failed to write file: %s", err.Error()))
			return
		}
		c.String(http.StatusOK, "File saved successfully <meta http-equiv='refresh' content='1; url=/edit'>")
		loadenv()
	})

	r.POST("/m3usave/:file", func(c *gin.Context) {
		var entries []Entry
		if err := c.ShouldBindJSON(&entries); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		log.Println("Received entries:", entries)

		filename := c.Param("file")
		file, err := os.Create("./m3u/" + filename)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		defer file.Close()

		writer := bufio.NewWriter(file)
		_, err = writer.WriteString("#EXTM3U\n\n")
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		for _, entry := range entries {
			disabledTxt := ""
			if strings.HasPrefix(entry.Id, "#") {
				disabledTxt = "#"
			}
			extinfLine := fmt.Sprintf(
				"#%sEXTINF:-1 channel-id=\"%s\" channel-number=\"%s\" tvc-guide-stationid=\"%s\" tvg-group=\"%s\" tvg-logo=\"%s\",%s\n",
				disabledTxt,
				entry.Id,
				entry.ChannelNumber,
				entry.StationId,
				entry.Group,
				entry.Logo,
				entry.ChannelName,
			)
			_, err = writer.WriteString(extinfLine)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			_, err = writer.WriteString(fmt.Sprintf("%s%s\n\n", disabledTxt, entry.StreamURL)) // use StreamURL here
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
		}

		err = writer.Flush()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"status": "File saved successfully"})
	})

	r.GET("/m3us", func(c *gin.Context) {
		files, err := os.ReadDir("./m3u")
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		m3us := []string{}
		for _, f := range files {
			if strings.HasSuffix(f.Name(), ".m3u") {
				m3us = append(m3us, f.Name())
			}
		}

		c.HTML(http.StatusOK, "m3us.html", gin.H{"m3us": m3us})
	})

	r.GET("/editm3u/:file", func(c *gin.Context) {
		filename := c.Param("file")
		file, err := os.Open("./m3u/" + filename)
		if err != nil {
			c.String(http.StatusInternalServerError, "Error opening file: %v", err)
			return
		}
		defer file.Close()

		scanner := bufio.NewScanner(file)
		var entries []Entry
		var currentEntry Entry

		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "#http") {
				line = strings.TrimPrefix(line, "#")
			}
			if strings.HasPrefix(line, "#EXTINF:") || strings.HasPrefix(line, "##EXTINF:") {
				extinfParts := strings.SplitN(line, ",", 2)
				if len(extinfParts) != 2 {
					continue
				}

				currentEntry = Entry{}
				currentEntry.ChannelName = extinfParts[1]

				// channel-id
				idParts := extractAttribute(extinfParts[0], "channel-id")
				if idParts != nil {
					currentEntry.Id = idParts[0]
				}

				// channel-number
				channelNumParts := extractAttribute(extinfParts[0], "channel-number")
				if channelNumParts != nil {
					currentEntry.ChannelNumber = channelNumParts[0]
				}

				// tvc-guide-stationid
				stationIdParts := extractAttribute(extinfParts[0], "tvc-guide-stationid")
				if stationIdParts != nil {
					currentEntry.StationId = stationIdParts[0]
				}

				// tvg-group
				groupParts := extractAttribute(extinfParts[0], "tvg-group")
				if groupParts != nil {
					currentEntry.Group = groupParts[0]
				}

				// tvg-logo
				logoParts := extractAttribute(extinfParts[0], "tvg-logo")
				if logoParts != nil {
					currentEntry.Logo = logoParts[0]
				}
			} else if len(line) > 0 && line[0] != '#' {
				currentEntry.StreamURL = line
				entries = append(entries, currentEntry)
			}
		}

		if err := scanner.Err(); err != nil {
			c.String(http.StatusInternalServerError, "Error scanning file: %v", err)
			return
		}

		c.HTML(http.StatusOK, "editm3u.html", gin.H{
			"filename": filename,
			"entries":  entries,
		})
	})

	r.GET("/config", func(c *gin.Context) {
		configData := parseEnvFile("./env")
		c.HTML(200, "config.html", configData)
	})

	r.POST("/configsave", func(c *gin.Context) {
		// Print all form data
		c.Request.ParseForm()
		// Load current configuration data
		configData := parseEnvFile("./env")
		// Update global variables
		for i, envVariable := range configData.EnvVariables {
			configData.EnvVariables[i].Value = c.PostForm(envVariable.Key)
		}
		// Update tuner variables
		for i := 0; i < len(configData.Tuners); i++ {
			configData.Tuners[i].Cmd = c.PostForm("CMD" + configData.Tuners[i].Number)
			configData.Tuners[i].EncoderUrl = c.PostForm("ENCODER" + configData.Tuners[i].Number + "_URL")
			configData.Tuners[i].TunerIp = c.PostForm("TUNER" + configData.Tuners[i].Number + "_IP")
		}
		c.Redirect(http.StatusMovedPermanently, "/config")
		saveConfigToFile("./env", configData)
	})

	// Report stats every 30 minutes
	if envdebug {
		ticker := time.NewTicker(30 * time.Minute)
		go func() {
			for range ticker.C {
				stats()
			}
		}()
	}
	logger("[START] ah4c is ready")
	// Not r.Run: it builds its own listener and leaves the send buffer to the
	// kernel, which autotunes it into the megabytes of stale video. See
	// playbackdelay.go.
	return serveLive(r, ":7654")
}

// Helper function to extract attribute from a line
func extractAttribute(line, attribute string) []string {
	parts := strings.SplitN(line, attribute+"=\"", 2)
	if len(parts) != 2 {
		return nil
	}
	parts = strings.SplitN(parts[1], "\"", 2)
	if len(parts) != 2 {
		return nil
	}
	return parts
}

func loadenv() {
	// Load environment variables from env if the file exists.
	if _, errenv := os.Stat("env"); errenv == nil {
		if envdebug {
			logger("[ENV] Loading env")
		}
		godotenv.Load("env")
	} else {
		logger("[ENV] Not loading env")
	}
	// Get the proxy IP address used to rewrite m3u ip addresses
	IPADDRESS := os.Getenv("IPADDRESS")
	if os.Getenv("ALLOW_DEBUG_VIDEO_PREVIEW") == "TRUE" {
		allowPreview = true
	}
	logger("[ENV] IPADDRESS                  %s", IPADDRESS)
	logger("[ENV] ALERT_SMTP_SERVER          %s", os.Getenv("ALERT_SMTP_SERVER"))
	logger("[ENV] ALERT_AUTH_SERVER          %s", os.Getenv("ALERT_AUTH_SERVER"))
	logger("[ENV] ALERT_EMAIL_FROM           %s", os.Getenv("ALERT_EMAIL_FROM"))
	logger("[ENV] ALERT_EMAIL_PASS           %s", os.Getenv("ALERT_EMAIL_PASS"))
	logger("[ENV] ALERT_EMAIL_TO             %s", os.Getenv("ALERT_EMAIL_TO"))
	logger("[ENV] ALERT_WEBHOOK_URL          %s", os.Getenv("ALERT_WEBHOOK_URL"))
	logger("[ENV] ALLOW_DEBUG_VIDEO_PREVIEW  %s", os.Getenv("ALLOW_DEBUG_VIDEO_PREVIEW"))
	logger("[ENV] NULL_FRAME_INSERTION       %s", os.Getenv("NULL_FRAME_INSERTION"))
	logger("[ENV] PLAYBACK_DETECTION         %s", os.Getenv("PLAYBACK_DETECTION"))
	logger("[ENV] PLAYBACK_DELAY             %s", os.Getenv("PLAYBACK_DELAY"))
	logger("[ENV] PLAYBACK_STATIC_TIMEOUT    %s", os.Getenv("PLAYBACK_STATIC_TIMEOUT"))
	// Retrieve the number of tuners from the environment variable "NUMBER_TUNERS".
	// This value represents the number of distinct tuners that the program will manage.
	numTunersStr := os.Getenv("NUMBER_TUNERS")
	numTuners, errtuners := strconv.Atoi(numTunersStr)
	if errtuners != nil {
		panic("Could not find an environment variable named NUMBER_TUNERS")
	}
	// Get directory of scripts
	streamerApp := os.Getenv("STREAMER_APP")
	// Loop over the number of tuners and create each one
	for i := 1; i <= numTuners; i++ {
		iStr := strconv.Itoa(i)
		var encoderurl string = "ENCODER" + iStr + "_URL"
		var tunerip string = "TUNER" + iStr + "_IP"
		var cmd string = "CMD" + iStr
		var teecmd string = "TEECMD" + iStr
		t := tuner{
			url:     os.Getenv(encoderurl),
			pre:     "./" + streamerApp + "/prebmitune.sh",
			start:   "./" + streamerApp + "/bmitune.sh",
			stop:    "./" + streamerApp + "/stopbmitune.sh",
			reboot:  "./" + streamerApp + "/reboot.sh",
			cmd:     os.Getenv(cmd),
			tunerip: os.Getenv(tunerip),
			teecmd:  os.Getenv(teecmd),
		}
		if envdebug {
			logger("[ENV] Creating tuner             %d", i)
			logger("[ENV] ENCODER%s_URL               %s", iStr, os.Getenv(encoderurl))
			logger("[ENV] TUNER%s_IP                  %s", iStr, os.Getenv(tunerip))
			logger("[ENV] CMD%s                       %s", iStr, os.Getenv(cmd))
			logger("[ENV] TEECMD%s                    %s", iStr, os.Getenv(teecmd))
			logger("[ENV] PRE SCRIPT                 %s", "./"+streamerApp+"/prebmitune.sh")
			logger("[ENV] START SCRIPT               %s", "./"+streamerApp+"/bmitune.sh")
			logger("[ENV] STOP SCRIPT                %s", "./"+streamerApp+"/stopbmitune.sh")
			logger("[ENV] REBOOT SCRIPT              %s", "./"+streamerApp+"/reboot.sh")
			logger("\n")
		}
		// Add the tuner to the tuners slice
		tuners = append(tuners, t)
	}
}

// Almighty main function
func main() {
	logger("[START] ah4c %s is starting", buildVersion())
	loadenv()
	tuneHoldStartup()
	loadCaptionConfig()
	warnIfNotPersistent()
	restoreGPURuntime()
	// Start GIN
	errrun := run()
	if errrun != nil {
		panic(errrun)
	}
}

// Log to a file and also console.  Send email on failures.
func logger(format string, v ...interface{}) {
	// Format the string
	logText := fmt.Sprintf(format, v...)
	// Check if logText is empty or just whitespace
	if strings.TrimSpace(logText) == "" {
		return
	}
	// Write to the console
	fmt.Println(logText)
	// Write to the log file
	loggerhandle.Println(logText)
	// If the log text contains the word "failed", send an email
	if strings.Contains(strings.ToLower(logText), "failed") {
		sendEmail(logText)
		alertWebhook(logText)
	}
}

// Alerting webhook
func alertWebhook(message string) {
	webhookURL := os.Getenv("ALERT_WEBHOOK_URL")
	// If the webhook URL is not set, do nothing
	if webhookURL == "" {
		return
	}
	// URL encode the message and replace $reason in the webhook URL with the encoded message
	encodedMessage := url.QueryEscape(message)
	webhookURL = strings.Replace(webhookURL, "$reason", encodedMessage, -1)
	resp, err := http.Get(webhookURL)
	if err != nil {
		logger("Error sending webhook alert: %s", err)
		return
	}
	defer resp.Body.Close()
	logger("Webhook alert sent successfully")
}

// Send email
func sendEmail(message string) {
	from := os.Getenv("ALERT_EMAIL_FROM")
	to := os.Getenv("ALERT_EMAIL_TO")
	smtpServer := os.Getenv("ALERT_SMTP_SERVER")
	authServer := os.Getenv("ALERT_AUTH_SERVER")
	useSendmail := os.Getenv("ALERT_EMAIL_USE_SENDMAIL") == "TRUE"
	if from == "" || to == "" {
		return
	}
	if useSendmail {
		cmd := exec.Command("sendmail", "-f", from, "-t")
		cmd.Stdin = strings.NewReader(message)
		err := cmd.Run()
		if err != nil {
			logger("sendmail error: %s", err)
			return
		}
		logger("Alert sent email successfully using sendmail")
	} else {
		pass := os.Getenv("ALERT_EMAIL_PASS")
		auth := smtp.PlainAuth("", from, pass, authServer)
		if pass == "" {
			auth = nil // Set auth to nil when ALERT_EMAIL_PASS is not set
		}
		msg := "From: " + from + "\n" +
			"To: " + to + "\n" +
			"Subject: ah4c error Detected\n\n" +
			message
		err := smtp.SendMail(smtpServer, auth, from, []string{to}, []byte(msg))
		if err != nil {
			logger("smtp error: %s", err)
			return
		}
		logger("Alert sent email successfully")
	}
}

// stream route code
func streamPageHandler(c *gin.Context) {
	if !allowPreview {
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("View preview is disabled"))
		return
	}
	tunerIndices := make([]int, len(tuners))
	for i := range tuners {
		tunerIndices[i] = i
	}
	c.HTML(http.StatusOK, "stream.html", gin.H{
		"TunerIndices": tunerIndices,
	})
}

func statusPageHandler(c *gin.Context) {
	c.HTML(http.StatusOK, "status.html", nil)
}

// /status route code
func apiStatusHandler(c *gin.Context) {
	// Fetch system stats
	cpuStats, _ := cpu.Percent(0, false)
	memStats, _ := mem.VirtualMemory()
	diskStats, _ := disk.Usage("/")
	netStats, _ := gopsutilNet.IOCounters(true)
	var maxSent, maxRecv uint64
	var maxTotalBytes uint64 = 0
	interfaceName := ""
	for _, netStat := range netStats {
		totalBytes := netStat.BytesSent + netStat.BytesRecv
		if totalBytes > maxTotalBytes {
			maxTotalBytes = totalBytes
			maxSent = netStat.BytesSent
			maxRecv = netStat.BytesRecv
			interfaceName = netStat.Name
		}
	}
	megabitsSent := math.Ceil(float64(maxSent) / 1024 / 1024 * 8)
	megabitsRecv := math.Ceil(float64(maxRecv) / 1024 / 1024 * 8)
	// Round up the stats
	roundedCpu := math.Ceil(cpuStats[0])
	roundedMemory := math.Ceil(memStats.UsedPercent)
	roundedDisk := math.Ceil(diskStats.UsedPercent)
	tunerLock.Lock()
	exportedTuners := make([]ExportedTuner, len(tuners))
	for i, t := range tuners {
		exportedTuners[i] = ExportedTuner{
			Tunerip: t.tunerip,
			Url:     t.url,
			Active:  t.active,
		}
	}
	tunerLock.Unlock()
	readersLock.Lock()
	exportedReaders := make([]ExportedReader, len(activeReaders))
	for i, r := range activeReaders {
		var fileName string
		if r.file != nil {
			fileName = r.file.Name()
		}
		var cmdString string
		if r.cmd != nil {
			cmdString = r.cmd.String()
		}
		exportedReaders[i] = ExportedReader{
			T:        r.t.index,
			Channel:  r.channel,
			Name:     channelName(r.channel),
			Started:  fmt.Sprintf("%v", r.started),
			Elapsed:  int64(time.Since(r.startedAt).Seconds()),
			FileName: fileName,
			Cmd:      cmdString,
		}
	}
	readersLock.Unlock()
	fanSpeed := ""
	gpuUtil := ""
	memUsage := ""
	GPUpowerUsagePercent := ""
	// NVIDIA stats if present
	_, err := exec.Command("which", "nvidia-smi").Output()
	if err == nil {
		// Execute nvidia-smi
		out, err := exec.Command("nvidia-smi").Output()
		if err == nil {
			output := string(out)
			// Extract fan speed
			fanRegex := regexp.MustCompile(`(?m)^\|?\s*(\d+)`)
			fanMatch := fanRegex.FindStringSubmatch(output)
			if len(fanMatch) > 1 {
				fanSpeed = fanMatch[1]
			}
			utilRegex := regexp.MustCompile(`(?m)(\d+)%      Default`)
			utilMatch := utilRegex.FindStringSubmatch(output)
			if len(utilMatch) > 1 {
				gpuUtil = utilMatch[1]
			}
			memRegex := regexp.MustCompile(`(?m)(\d+)MiB /  (\d+)MiB`)
			memMatch := memRegex.FindStringSubmatch(output)
			if len(memMatch) > 2 {
				usedMem, err1 := strconv.Atoi(memMatch[1])
				totalMem, err2 := strconv.Atoi(memMatch[2])
				if err1 == nil && err2 == nil {
					memUsage = fmt.Sprintf("%.2f", float64(usedMem)/float64(totalMem)*100)
				}
			}
			powerRegex := regexp.MustCompile(`(?m)(\d+)W / (\d+)W`)
			powerMatch := powerRegex.FindStringSubmatch(output)
			if len(powerMatch) > 2 {
				usedPower, err1 := strconv.Atoi(powerMatch[1])
				totalPower, err2 := strconv.Atoi(powerMatch[2])
				if err1 == nil && err2 == nil {
					GPUpowerUsagePercent = fmt.Sprintf("%.2f", float64(usedPower)/float64(totalPower)*100)
				}
			}

		}
	}
	if gpuUtil == "" || memUsage == "" || GPUpowerUsagePercent == "" {
		u, m, p := otherGPU()
		if gpuUtil == "" {
			gpuUtil = u
		}
		if memUsage == "" {
			memUsage = m
		}
		if GPUpowerUsagePercent == "" {
			GPUpowerUsagePercent = p
		}
	}
	// Response with JSON
	c.JSON(http.StatusOK, gin.H{
		"CPU":           roundedCpu,
		"Memory":        roundedMemory,
		"Disk":          roundedDisk,
		"NetSent":       maxSent,
		"NetRecv":       maxRecv,
		"Tuners":        exportedTuners,
		"Readers":       exportedReaders,
		"megabitsSent":  megabitsSent,
		"megabitsRecv":  megabitsRecv,
		"interface":     interfaceName,
		"GPUfanSpeed":   fanSpeed,
		"GPUCPUUsage":   gpuUtil,
		"GPUMemUsage":   memUsage,
		"GPUPowerUsage": GPUpowerUsagePercent,
	})
}

// Add a new reader to activeReaders
func addReader(r *reader) {
	readersLock.Lock()
	defer readersLock.Unlock()
	r.startedAt = time.Now()
	activeReaders = append(activeReaders, r)
}

// Remove a reader from activeReaders
func removeReader(r *reader) {
	readersLock.Lock()
	defer readersLock.Unlock()
	for i, reader := range activeReaders {
		if reader == r {
			activeReaders = append(activeReaders[:i], activeReaders[i+1:]...)
			break
		}
	}
}

func channelName(channel string) string {
	if channel == "" {
		return ""
	}
	files, err := os.ReadDir("m3u")
	if err != nil {
		return ""
	}
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".m3u") {
			continue
		}
		content, err := os.ReadFile("m3u/" + f.Name())
		if err != nil {
			continue
		}
		lines := strings.Split(string(content), "\n")
		for i := 0; i < len(lines)-1; i++ {
			line := strings.TrimSpace(lines[i])
			if !strings.HasPrefix(line, "#EXTINF:") {
				continue
			}
			if strings.HasSuffix(strings.TrimSpace(lines[i+1]), "/"+channel) {
				if idx := strings.LastIndex(line, ","); idx != -1 {
					return strings.TrimSpace(line[idx+1:])
				}
			}
		}
	}
	return ""
}

func parseEnvFile(filePath string) ConfigData {
	file, err := os.ReadFile(filePath)
	if err != nil {
		log.Printf("Failed to open file: %s", err)
		os.Exit(1)
	}
	lines := strings.Split(string(file), "\n")
	var envVariables []ConfigEnvVariable
	var tuners []ConfigTuner
	tunerRegex := regexp.MustCompile(`(CMD|ENCODER|TUNER)([0-9]+)(_URL|_IP)?`)
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			log.Printf("Invalid line: %s", line)
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.Trim(strings.TrimSpace(parts[1]), "\"")
		if tunerRegex.MatchString(key) {
			tunerNumber := tunerRegex.FindStringSubmatch(key)[2]
			// Find the tuner with this number, or create a new one
			var tuner *ConfigTuner
			for i := range tuners {
				if tuners[i].Number == tunerNumber {
					tuner = &tuners[i]
					break
				}
			}
			if tuner == nil {
				tuners = append(tuners, ConfigTuner{Number: tunerNumber})
				tuner = &tuners[len(tuners)-1] // Get reference to the last element in the slice
			}
			switch {
			case strings.HasPrefix(key, "CMD"):
				tuner.Cmd = value
			case strings.HasPrefix(key, "ENCODER") && strings.HasSuffix(key, "_URL"):
				tuner.EncoderUrl = value
			case strings.HasPrefix(key, "TUNER") && strings.HasSuffix(key, "_IP"):
				tuner.TunerIp = value
			}
		} else {
			envVariables = append(envVariables, ConfigEnvVariable{Key: key, Value: value})
		}
	}
	return ConfigData{EnvVariables: envVariables, Tuners: tuners}
}

func saveConfigToFile(filePath string, configData ConfigData) {
	var lines []string
	// Save global variables
	for _, envVariable := range configData.EnvVariables {
		lines = append(lines, envVariable.Key+"="+"\""+envVariable.Value+"\"")
	}
	lines = append(lines, "\n")
	// Save tuner variables
	for _, tuner := range configData.Tuners {
		lines = append(lines, "CMD"+tuner.Number+"="+"\""+tuner.Cmd+"\"")
		lines = append(lines, "ENCODER"+tuner.Number+"_URL="+"\""+tuner.EncoderUrl+"\"")
		lines = append(lines, "TUNER"+tuner.Number+"_IP="+"\""+tuner.TunerIp+"\"\n")
	}
	err := os.WriteFile(filePath, []byte(strings.Join(lines, "\n")), 0644)
	if err != nil {
		log.Printf("Failed to write to file: %s", err)
		os.Exit(1)
	}
}

// nullTSPacket is an MPEG-TS NULL packet (PID 0x1FFF) — safe keepalive bytes.
var nullTSPacket = func() [188]byte {
	var p [188]byte
	p[0], p[1], p[2], p[3] = 0x47, 0x1F, 0xFF, 0x10
	copy(p[4:], bytes.Repeat([]byte{0xFF}, 184))
	return p
}()

// nullFill is a pre-assembled run of NULL packets for one-shot stall fills.
var nullFill = bytes.Repeat(nullTSPacket[:], 350) // 65.8KB, ≥ any plausible DVR read

// stallTolerantReader fills encoder stalls with NULL TS packets. NULLs are
// gated behind the first real chunk so DVR locks onto the real PAT/PMT.
type stallTolerantReader struct {
	chunks        chan []byte
	closed        chan struct{}
	closeOnce     sync.Once
	bodyMu        sync.Mutex
	body          io.ReadCloser
	reconnectFn   func() (io.ReadCloser, error)
	label         string
	hasFirstChunk atomic.Bool
	reconnects    atomic.Int64
	// dropped counts chunks thrown away to keep the queue from becoming a
	// rest is what would not fit in the last caller's buffer, handed over on
	// the next Read. Only the reading goroutine touches it.
	rest    []byte
	dropped atomic.Int64
	// filled counts NULL bytes put into a live program to cover an encoder
	// stall. saidFill keeps that to one line a tune.
	filled   atomic.Int64
	saidFill bool
}

type sessionSource interface{ sessions() int64 }

func (s *stallTolerantReader) sessions() int64 { return s.reconnects.Load() }

const (
	stallReadGap        = 500 * time.Millisecond
	srcStallReconnect   = 5 * time.Second
	srcReconnectBackoff = 2 * time.Second
	reconnectLogEvery   = 10 * time.Second
	// stallQuietGap keeps a hold's quiet reader from spinning without making
	// it block: long enough that a core is not burned, short enough that the
	// hand-off is not delayed by it.
	stallQuietGap        = 20 * time.Millisecond
	preFirstChunkBudget  = 15 * time.Second // fail over fast on a dead tuner
	postFirstChunkBudget = 3 * time.Minute  // ride through mid-stream glitches
	chunkSize            = 32 * 1024
	// queueDepth is how many chunks may wait between the encoder and the DVR.
	// It was sixty-four — two megabytes, and the only constant here without a
	// reason written beside it. Two megabytes is between one and a half and
	// three and a half seconds of video, and a queue that deep is not headroom,
	// it is a place for lag to live: the moment the DVR reads slower than the
	// encoder sends, it fills, and then every slot the consumer frees is
	// filled again at once. On this user's hardware it refilled two megabytes
	// within about seven seconds of the hand-off and stood full for the rest of
	// the recording — the picture correct, the timeline seconds behind, never
	// catching up, and fast forward snapping back because the viewer really is
	// at the live edge of a recording that is itself late.
	//
	// The depth was never what rode out a stall. A stall is bytes not arriving,
	// so it can only drain this queue, never fill it; what covers a stall is
	// the NULL fill Read manufactures after stallReadGap, which does not care
	// how deep this is. So the depth only ever bought latency. Four chunks is
	// a hundred and twenty-eight kilobytes, enough to cover the scheduling gap
	// between one producer read and one consumer read and no more.
	queueDepth = 16
)

func newStallTolerantReader(body io.ReadCloser, reconnectFn func() (io.ReadCloser, error), label string) *stallTolerantReader {
	s := &stallTolerantReader{
		chunks:      make(chan []byte, queueDepth),
		closed:      make(chan struct{}),
		body:        body,
		reconnectFn: reconnectFn,
		label:       label,
	}
	go s.producer()
	return s
}

func (s *stallTolerantReader) producer() {
	chunk := make([]byte, chunkSize)
	lastReal := time.Now()
	var lastLog time.Time
	for {
		select {
		case <-s.closed:
			return
		default:
		}
		budget := preFirstChunkBudget
		if s.hasFirstChunk.Load() {
			budget = postFirstChunkBudget
		}
		if time.Since(lastReal) > budget {
			logger("[%s] no source bytes for %v; giving up and ending stream", s.label, budget)
			s.closeOnce.Do(func() { close(s.closed) })
			return
		}
		s.bodyMu.Lock()
		body := s.body
		s.bodyMu.Unlock()
		if body == nil {
			if s.reconnectFn == nil {
				s.closeOnce.Do(func() { close(s.closed) })
				return
			}
			nb, rerr := s.reconnectFn()
			if rerr != nil {
				if time.Since(lastLog) > reconnectLogEvery {
					logger("[%s] reconnect to encoder failed: %v", s.label, rerr)
					lastLog = time.Now()
				}
				select {
				case <-time.After(srcReconnectBackoff):
				case <-s.closed:
					return
				}
				continue
			}
			logger("[%s] reconnected to encoder", s.label)
			s.reconnects.Add(1)
			s.bodyMu.Lock()
			s.body = nb
			s.bodyMu.Unlock()
			continue
		}
		n, err := readWithDeadline(body, chunk, srcStallReconnect)
		if n > 0 {
			lastReal = time.Now()
			data := make([]byte, n)
			copy(data, chunk[:n])
			select {
			case s.chunks <- data:
			case <-s.closed:
				return
			default:
				// The queue is full, which means the DVR has been reading
				// slower than the encoder sends. This send used to block, and
				// blocking is what made the queue a place lag goes to live:
				// once queueDepth chunks are in it, every slot the consumer
				// frees is filled again at once, so the viewer stays a full
				// queue behind the encoder for the rest of the tune — two
				// megabytes, which is between one and a half and three and a
				// half seconds of video depending on the bitrate. Nothing
				// drained it. That is what "behind, and it stays behind, and
				// fast forward snaps back" is: the viewer is at the live edge
				// of a recording that is itself late.
				//
				// A live stream would rather lose a moment than carry it for
				// ever, so the oldest chunk goes and the newest is kept. This
				// is the encoder reopen's trick — discard what is stored ahead
				// of the show — made continuous and cheap instead of once and
				// by force.
				// The whole queue is the lag, not the newest chunk in it.
				// Freeing one slot and filling it again leaves the queue full
				// and the viewer exactly as far behind as they were — which
				// is what a first attempt at this did, and the log said so:
				// "dropped 1 chunk(s)", over and over, with the other
				// sixty-three still sitting there. So empty it: everything
				// waiting is old, and the newest chunk goes in alone.
				gone := 0
			drain:
				for {
					select {
					case <-s.chunks:
						gone++
					default:
						break drain
					}
				}
				s.dropped.Add(int64(gone))
				select {
				case s.chunks <- data:
				case <-s.closed:
					return
				}
			}
			if err == nil {
				continue
			}
		}
		if err != nil {
			logger("[%s] encoder stream ended (%v); reconnecting", s.label, err)
		}
		body.Close()
		s.bodyMu.Lock()
		s.body = nil
		s.bodyMu.Unlock()
	}
}

func (s *stallTolerantReader) Read(p []byte) (int, error) {
	// Pre-first-chunk: nil channel disables the NULL-fill case, so Read blocks on chunks/closed only.
	var stall <-chan time.Time
	// No stall timer at all while a hold has the filling switched off. There
	// is nothing to do when it fires — the wait is quiet on purpose — so the
	// timer only exists to make Read return (0, nil), and a reader that
	// returns (0, nil) in a loop is a spin: gateReader.Read calls src.Read in
	// a tight loop until it opens, so every held tuner pegged a core doing
	// nothing. The drain then could not keep up with its own encoder, the
	// queue sat permanently full, and every push threw the whole thing away —
	// fourteen hundred chunks discarded every ten seconds on a tune that had
	// not even handed over yet, and tunes failing outright.
	//
	// With no timer the select simply blocks on chunks or closed, which is
	// what a reader with nothing to say should do.
	if s.hasFirstChunk.Load() {
		t := time.NewTimer(stallReadGap)
		defer t.Stop()
		stall = t.C
	}
	// A chunk left over from a caller whose buffer could not take all of the
	// last one goes first. Without this, copy silently threw the tail away:
	// every caller today reads with thirty-two kilobytes or more so nothing
	// lost a byte, but a caller reading smaller would have quietly dropped
	// most of every chunk — a hole in the video, with nothing said. That is
	// the shape of fault this program has been bitten by twice tonight.
	if len(s.rest) > 0 {
		n := copy(p, s.rest)
		s.rest = s.rest[n:]
		return n, nil
	}
	select {
	case <-s.closed:
		return 0, io.EOF
	case data := <-s.chunks:
		s.hasFirstChunk.Store(true)
		n := copy(p, data)
		if n < len(data) {
			s.rest = append(s.rest[:0], data[n:]...)
		}
		return n, nil
	case <-stall:
		// This reader once had its filling switched off during a hold, on the
		// grounds that a stream held back on purpose should not be papered
		// over. It is not worth what it costs. Returning no bytes here breaks
		// captionStream.Read, which only starts its pump on a read that
		// returns bytes — so the caption wrapper never started, the gate never
		// saw a packet, the drain never logged, and no tune reached its
		// hand-off at all. Blocking instead hung it just as hard, and
		// returning (0, nil) in a loop spun a core.
		//
		// It never earned the risk either: the counter below, added to prove
		// the filling was firing during holds, never once fired.
		//
		// This puts NULL packets into a live program. It exists so a stalled
		// encoder does not show the DVR a zero-byte gap, and that is worth
		// having — but the cost has never been said out loud, and it is the
		// cost that matters here: NULLs carry no frames, so whatever is filled
		// this way is stream the viewer cannot play or seek through. It sits
		// between the playhead and the DVR's live edge and does not move.
		//
		// So it counts and it speaks. If a tune shows seconds of this, the
		// encoder is stalling and the filler is turning those stalls into
		// unplayable time in the recording.
		if len(p) < 188 {
			s.filled.Add(188)
			s.sayFilled()
			return copy(p, nullTSPacket[:]), nil
		}
		n := min(len(p)/188*188, len(nullFill))
		s.filled.Add(int64(n))
		s.sayFilled()
		return copy(p, nullFill[:n]), nil
	}
}

// sayFilled says once, per reader, that the encoder stalled and the gap was
// covered with NULL packets. Once: it used to repeat every ten seconds with a
// running total, which reads like something getting worse rather than one
// stall that was handled.
func (s *stallTolerantReader) sayFilled() {
	if s.saidFill {
		return
	}
	s.saidFill = true
	logger("[%s] the encoder went quiet; the gap is covered with NULL packets, which is stream the viewer cannot play through", s.label)
}

// flush throws away everything waiting in the queue, so what is read next is
// the live edge. The hold calls this just before its gate arms: a gate that
// arms against a full queue picks its keyframe out of two megabytes of
// stored-up stream and hands the DVR video that is already seconds old, and
// emptying the queue afterwards is too late — the stale frames have gone.
func (s *stallTolerantReader) flush(label string) {
	gone := 0
	for {
		select {
		case <-s.chunks:
			gone++
		default:
			if gone > 0 {
				s.dropped.Add(int64(gone))
			}
			return
		}
	}
}

func (s *stallTolerantReader) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	s.bodyMu.Lock()
	body := s.body
	s.bodyMu.Unlock()
	if body != nil {
		return body.Close()
	}
	return nil
}

const (
	adbTimeout           = 5 * time.Second
	adbGiveUp            = 3
	playbackPoll         = 250 * time.Millisecond
	playbackConfirm      = 2
	playbackStaticFor    = 2 * time.Second
	playbackSessionEvery = time.Second
	playbackTimeout      = 12 * time.Second
	keyframeWait         = 8 * time.Second
	keyframeCeiling      = 2 * keyframeWait
	riseWindow           = 250 * time.Millisecond
	riseFactor           = 4
	riseWait             = time.Second
	minWindow            = 8 * 188
	busyWindow           = 46875
)

type gateReader struct {
	// tables holds the encoder's real PAT and PMT once seen, for the hold in
	// front of the gate to send with its filler.
	tables   atomic.Value
	src      io.ReadCloser
	ready    <-chan struct{}
	open     bool
	pat      []byte
	pmt      []byte
	pend     []byte
	carry    []byte
	keep     []byte
	t0       time.Time
	armedAt  time.Time
	armed0   time.Time
	winBytes int
	winStart time.Time
	lastWin  int
	floor    int
	peak     int
	vid      map[int]bool

	// timed is set when a timer chose the wait, so the gate releases on the
	// keyframe nearest the moment the timer named rather than weighing motion.
	timed  bool
	target time.Time
	// detect closes when playback detection has seen the app playing; nil
	// when detection is off. It is what allows the gate to take a keyframe
	detect <-chan struct{}

	sess       sessionSource
	sessSeen   int64
	sess0      int64
	expectSwap atomic.Bool
}

// newGateReader gates src until ready closes. timed says the wait was a timer,
// so the gate releases on the first keyframe after it rather than waiting to
func newGateReader(src io.ReadCloser, ready <-chan struct{}, timed bool, target time.Time, detect <-chan struct{}) *gateReader {
	g := &gateReader{src: src, ready: ready, t0: time.Now(), timed: timed, target: target, detect: detect}
	if sc, ok := src.(sessionSource); ok {
		g.sess = sc
		g.sessSeen = sc.sessions()
		g.sess0 = g.sessSeen
	}
	return g
}

// publishTables snapshots the encoder's real PAT and PMT the moment the gate
// has both, so the hold in front of it can send them with its filler. They are
// the encoder's own tables, read off the connection that is draining — not
// tables invented here, which is the thing that failed before: invented tables
// name PIDs nothing will ever send on, and a DVR locks onto them and waits for
// ever. These name exactly what is about to arrive.
func (g *gateReader) publishTables() {
	if len(g.pat) == 0 || len(g.pmt) == 0 {
		return
	}
	b := make([]byte, 0, len(g.pat)+len(g.pmt))
	b = append(b, g.pat...)
	b = append(b, g.pmt...)
	g.tables.Store(&b)
}

// realTables is the encoder's PAT and PMT, or nil until the gate has seen both.
func (g *gateReader) realTables() []byte {
	if v, ok := g.tables.Load().(*[]byte); ok && v != nil {
		return *v
	}
	return nil
}

func (g *gateReader) resync() {
	g.pat, g.pmt, g.carry, g.keep = nil, nil, nil, nil
	g.vid = nil
	g.winBytes, g.lastWin, g.floor, g.peak = 0, 0, 0, 0
	g.winStart = time.Time{}
	if !g.armedAt.IsZero() {
		g.armedAt = time.Now()
	}
}

func (g *gateReader) expectNewStream() { g.expectSwap.Store(true) }

func (g *gateReader) releaseKind() string {
	// A timer decided how long to wait, so the gate's only remaining job is to
	// start on a picture rather than in the middle of one. Weighing motion here
	if g.timed {
		return "timed"
	}
	if g.floor > 0 && g.lastWin >= g.floor*riseFactor {
		return "moving"
	}
	if g.floor > 0 && g.peak < g.floor*riseFactor && g.lastWin >= busyWindow &&
		time.Since(g.armedAt) >= riseWait &&
		!(g.sess != nil && g.expectSwap.Load() && g.sessSeen == g.sess0) {
		return "uniform"
	}
	return ""
}

func (g *gateReader) armed() bool {
	return g.armedAtTime(time.Now())
}

// armedAtTime is armed as of a given moment, so the rule can be tested.
// The hand-off lands on the keyframe nearest the timer's moment: the first
func (g *gateReader) armedAtTime(now time.Time) bool {
	if g.timed && !g.target.IsZero() {
		if !now.Before(g.target) {
			return true
		}
		return false
	}
	select {
	case <-g.ready:
		return true
	default:
		return false
	}
}

func (g *gateReader) release(reason string) {
	// Tables, then the keyframe, and nothing timestamped before the program:
	// the DVR keeps time by the stream's own timestamps, so anything stamped
	g.pend = append(append(append([]byte{}, g.pat...), g.pmt...), g.keep...)
	g.open, g.keep = true, nil
	if g.timed && !g.target.IsZero() {
		logger("[PLAYBACK] start on keyframe, %v from the mark", time.Since(g.target).Round(time.Millisecond))
		return
	}
	logger("[PLAYBACK] start on a %s keyframe after %v", reason, time.Since(g.t0).Round(time.Millisecond))
}

// gatePSI is the table section in pkt from its table_id on, however the
// encoder packs it — payload only, or behind an adaptation field — or nil
func gatePSI(pkt []byte, pusi bool, afc byte) []byte {
	if !pusi || afc == 0 || afc == 2 {
		return nil
	}
	off := 4
	if afc == 3 {
		off = 5 + int(pkt[4])
	}
	if off+1 >= 188 || pkt[off] != 0 {
		return nil
	}
	return pkt[off+1:]
}

func videoPIDs(s []byte) map[int]bool {
	if len(s) < 12 || s[0] != 0x02 {
		return nil
	}
	slen := int(s[1]&0x0F)<<8 | int(s[2])
	end := 3 + slen - 4
	if end > len(s) {
		end = len(s)
	}
	i := 12 + (int(s[10]&0x0F)<<8 | int(s[11]))
	out := map[int]bool{}
	for i+4 < end {
		st := s[i]
		pid := int(s[i+1]&0x1F)<<8 | int(s[i+2])
		if st == 0x01 || st == 0x02 || st == 0x1B || st == 0x24 {
			out[pid] = true
		}
		i += 5 + (int(s[i+3]&0x0F)<<8 | int(s[i+4]))
	}
	if len(out) == 0 {
		// No stream type this knows. Encoders vary, and a codec nobody here
		// has heard of should still gate on a picture rather than falling back
		// to starting unaligned. The PCR is carried on the video PID on every
		// encoder seen so far, so that is the one to watch.
		if pcr := int(s[8]&0x1F)<<8 | int(s[9]); pcr != 0 && pcr != 0x1FFF {
			out[pcr] = true
		}
	}
	return out
}

func (g *gateReader) scan(b []byte) int {
	i := 0
	for i+188 <= len(b) {
		if b[i] != 0x47 {
			i++
			continue
		}
		pkt := b[i : i+188]
		pid := int(pkt[1]&0x1F)<<8 | int(pkt[2])
		afc, pusi := pkt[3]>>4&3, pkt[1]&0x40 != 0
		if pid == 0 {
			if sec := gatePSI(pkt, pusi, afc); len(sec) > 0 && sec[0] == 0x00 {
				g.pat = append(g.pat[:0], pkt...)
				g.publishTables()
			}
			i += 188
			continue
		}
		if sec := gatePSI(pkt, pusi, afc); len(sec) > 0 && sec[0] == 0x02 {
			g.pmt = append(g.pmt[:0], pkt...)
			g.publishTables()
			if v := videoPIDs(sec); len(v) > 0 {
				g.vid = v
			}
			i += 188
			continue
		}
		if g.vid[pid] {
			g.winBytes += 188
		}
		if g.winStart.IsZero() {
			g.winStart = time.Now()
		}
		if now := time.Now(); now.Sub(g.winStart) >= riseWindow {
			full := now.Sub(g.winStart) <= 2*riseWindow && g.winBytes >= minWindow
			if full && (g.floor == 0 || g.winBytes < g.floor) {
				g.floor = g.winBytes
			}
			if full {
				g.lastWin = g.winBytes
				if g.winBytes > g.peak {
					g.peak = g.winBytes
				}
			}
			g.winBytes, g.winStart = 0, now
		}
		if !g.armed() {
			i += 188
			continue
		}
		if g.armedAt.IsZero() {
			g.armedAt = time.Now()
			if g.armed0.IsZero() {
				g.armed0 = g.armedAt
			}
			// Just told it may open, and nothing released yet. Everything
			// queued behind this reader was captured while the wait was still
			// running, so a keyframe chosen out of it is already old: the DVR
			// is handed the whole queue at once — 512 KB, about six tenths of
			// a second at this bitrate — and the player races through it,
			// dropping frames, then settles. That is what "it dropped a
			// shitload of frames and got really fast for a second and then
			// caught back up" is made of.
			//
			// drainEarly empties the queue flushBeforeArm ahead of its gate
			// for exactly this reason. Playback detection could not: it never
			// knows in advance when it will be armed. It knows now — this is
			// that moment, and it is inside the gate's own goroutine, so
			// there is no race with a release that has not happened yet.
			//
			// The carry goes with it and the buffer is abandoned, so the
			// keyframe is hunted through bytes that arrive after this point
			// and not through what was already stored up.
			if f, ok := g.src.(interface{ flush(string) }); ok {
				f.flush("")
				g.carry = nil
				return len(b)
			}
		}
		if g.vid[pid] && afc >= 2 && pkt[4] > 0 && pkt[5]&0x40 != 0 {
			if kind := g.releaseKind(); kind != "" {
				g.keep = append(g.keep[:0], b[i:]...)
				g.release(kind)
				return len(b)
			}
		}
		i += 188
	}
	return i
}

func (g *gateReader) Read(p []byte) (int, error) {
	for !g.open && len(g.pend) == 0 {
		buf := make([]byte, 32*1024)
		n, err := g.src.Read(buf)
		if g.sess != nil {
			if s := g.sess.sessions(); s != g.sessSeen {
				g.sessSeen = s
				g.resync()
			}
		}
		if n > 0 {
			b := append(g.carry, buf[:n]...)
			used := g.scan(b)
			if !g.open {
				g.carry = append(g.carry[:0], b[used:]...)
			}
		}
		if err != nil {
			if len(g.pend) == 0 {
				return 0, err
			}
			break
		}
		if !g.open && !g.armed0.IsZero() && time.Since(g.armed0) > keyframeCeiling {
			logger("[PLAYBACK] encoder kept restarting for %v; starting unaligned",
				time.Since(g.armed0).Round(time.Second))
			g.pend = append(append([]byte{}, g.pat...), g.pmt...)
			g.open, g.carry = true, nil
		}
		if !g.open && !g.armedAt.IsZero() && time.Since(g.armedAt) > keyframeWait {
			logger("[PLAYBACK] no keyframe within %v; starting unaligned", keyframeWait)
			g.pend = append(append([]byte{}, g.pat...), g.pmt...)
			g.open, g.carry = true, nil
		}
	}
	if len(g.pend) > 0 {
		n := copy(p, g.pend)
		g.pend = g.pend[n:]
		return n, nil
	}
	return g.src.Read(p)
}

func (g *gateReader) Close() error { return g.src.Close() }

func audioPiids(dump string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(dump, "\n") {
		if !strings.Contains(line, "state:started") {
			continue
		}
		if !strings.Contains(line, "CONTENT_TYPE_MOVIE") && !strings.Contains(line, "CONTENT_TYPE_MUSIC") &&
			!strings.Contains(line, "usage=USAGE_MEDIA") {
			continue
		}
		id := ""
		if i := strings.Index(line, "piid:"); i >= 0 {
			id = line[i+len("piid:"):]
		} else if i := strings.Index(line, " ID:"); i >= 0 {
			id = line[i+len(" ID:"):]
		}
		if f := strings.Fields(id); len(f) > 0 {
			out[strings.TrimRight(f[0], ",;")] = true
		}
	}
	return out
}

func heldNewID(base, now map[string]bool, held map[string]int) bool {
	for id := range now {
		if base[id] {
			continue
		}
		held[id]++
		if held[id] >= playbackConfirm {
			return true
		}
	}
	for id := range held {
		if !now[id] {
			delete(held, id)
		}
	}
	return false
}

func adbAudio(tunerip string) []byte {
	ctx, cancel := context.WithTimeout(context.Background(), adbTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "adb", "-s", tunerip, "shell", "dumpsys audio").Output()
	if err != nil {
		return nil
	}
	return out
}

var adbKeycodes = map[string]string{
	"up":     "KEYCODE_DPAD_UP",
	"down":   "KEYCODE_DPAD_DOWN",
	"left":   "KEYCODE_DPAD_LEFT",
	"right":  "KEYCODE_DPAD_RIGHT",
	"select": "KEYCODE_DPAD_CENTER",
	"back":   "KEYCODE_BACK",
	"home":   "KEYCODE_HOME",
	"play":   "KEYCODE_MEDIA_PLAY_PAUSE",
	"wake":   "KEYCODE_WAKEUP",
	"sleep":  "KEYCODE_SLEEP",
}

const stopScriptTimeout = 20 * time.Second

func runStopScript(t *tuner, channel string) error {
	ctx, cancel := context.WithTimeout(context.Background(), stopScriptTimeout)
	defer cancel()
	logger("[CONTROL] stop script %s %s %s", t.stop, t.tunerip, channel)
	out, err := exec.CommandContext(ctx, t.stop, t.tunerip, channel).CombinedOutput()
	if trimmed := strings.TrimSpace(string(out)); trimmed != "" {
		logger("[CONTROL] stop script output: %s", trimmed)
	}
	if err != nil {
		logger("[ERR] stop script failed: %v", err)
	}
	return err
}

func readerGone(r *reader) bool {
	readersLock.Lock()
	defer readersLock.Unlock()
	for _, ar := range activeReaders {
		if ar == r {
			return false
		}
	}
	return true
}

func releaseTuner(index int) string {
	var target *reader
	readersLock.Lock()
	for _, ar := range activeReaders {
		if ar.t == &tuners[index] {
			target = ar
			break
		}
	}
	readersLock.Unlock()
	status := "stopped"
	if target != nil {
		if target.ReadCloser != nil {
			target.ReadCloser.Close()
		}
		deadline := time.Now().Add(stopScriptTimeout)
		for !readerGone(target) && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		if !readerGone(target) {
			logger("[CONTROL] tuner %d source closed but teardown is still running, leaving the lock held", index)
			return "stopping"
		}
	} else {
		tunerLock.Lock()
		locked := tuners[index].active
		tunerLock.Unlock()
		if !locked {
			return "idle"
		}
		time.Sleep(time.Second)
		readersLock.Lock()
		for _, ar := range activeReaders {
			if ar.t == &tuners[index] {
				target = ar
				break
			}
		}
		readersLock.Unlock()
		if target != nil {
			logger("[CONTROL] tuner %d is starting a tune, leaving it alone", index)
			return "busy"
		}
		runStopScript(&tuners[index], "")
		status = "unstuck"
	}
	tunerLock.Lock()
	tuners[index].active = false
	tunerLock.Unlock()
	logger("[CONTROL] tuner %d released (%s)", index, status)
	return status
}

func adbControl(tunerip string, action string) error {
	connectCtx, connectCancel := context.WithTimeout(context.Background(), adbTimeout)
	exec.CommandContext(connectCtx, "adb", "connect", tunerip).Run()
	connectCancel()
	ctx, cancel := context.WithTimeout(context.Background(), adbTimeout)
	defer cancel()
	if action == "reboot" {
		logger("[CONTROL] reboot -> %s", tunerip)
		return exec.CommandContext(ctx, "adb", "-s", tunerip, "shell", "reboot").Run()
	}
	keycode, ok := adbKeycodes[action]
	if !ok {
		return fmt.Errorf("unknown action %q", action)
	}
	logger("[CONTROL] %s -> %s", action, tunerip)
	return exec.CommandContext(ctx, "adb", "-s", tunerip, "shell", "input", "keyevent", keycode).Run()
}

func samePiids(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for id := range a {
		if !b[id] {
			return false
		}
	}
	return true
}

func piidList(m map[string]bool) string {
	if len(m) == 0 {
		return "no started players"
	}
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

func mediaSignature(tunerip string) string {
	ctx, cancel := context.WithTimeout(context.Background(), adbTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "adb", "-s", tunerip, "shell", "dumpsys media_session").Output()
	if err != nil {
		return ""
	}
	return parseMediaSignature(string(out))
}

func parseMediaSignature(dump string) string {
	var parts []string
	for _, line := range strings.Split(dump, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "description=") || strings.Contains(line, "metadata:") ||
			strings.Contains(line, "PlaybackState {") {
			parts = append(parts, line)
		}
	}
	return strings.Join(parts, "|")
}

func audioBaseline(tunerip string) map[string]bool {
	ctx, cancel := context.WithTimeout(context.Background(), adbTimeout)
	exec.CommandContext(ctx, "adb", "connect", tunerip).Run()
	cancel()
	out := adbAudio(tunerip)
	if len(out) == 0 {
		return nil
	}
	return audioPiids(string(out))
}

// playbackStaticBudget is how long the box may keep the exact player and media
// session it already had before the adb check concludes a tune cannot be seen
// from here and hands off to motion gating. Two seconds suits an Osprey, which
// tears into a tune at once; the DTV app holds one player across channel
// changes for longer than that, so PLAYBACK_STATIC_TIMEOUT overrides it, in
// seconds. A value at or above the check's budget means the early handoff
// never fires and the check simply runs its full budget.
func playbackStaticBudget() time.Duration {
	if s := os.Getenv("PLAYBACK_STATIC_TIMEOUT"); s != "" && s != "0" {
		if secs, err := strconv.Atoi(s); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
		logger("[PLAYBACK] PLAYBACK_STATIC_TIMEOUT %q is not a positive number of seconds, using %v", s, playbackStaticFor)
	}
	return playbackStaticFor
}

func waitForPlayback(tunerip string, base map[string]bool, sig string, done <-chan struct{}) bool {
	t0 := time.Now()
	budget := playbackTimeout
	staticFor := playbackStaticBudget()
	deadline := t0.Add(budget)
	held := map[string]int{}
	last := map[string]bool{}
	fails := 0
	swapComing := false
	var staticSince, sigAt time.Time
	nowSig := sig
	for time.Now().Before(deadline) {
		select {
		case <-done:
			return swapComing
		default:
		}
		out := adbAudio(tunerip)
		if len(out) == 0 {
			if fails++; fails >= adbGiveUp {
				logger("[PLAYBACK] %s unreachable over adb, gating on motion alone", tunerip)
				return swapComing
			}
			time.Sleep(playbackPoll)
			continue
		}
		fails = 0
		last = audioPiids(string(out))
		if heldNewID(base, last, held) {
			logger("[PLAYBACK] %s playing after %v", tunerip, time.Since(t0).Round(time.Millisecond))
			return swapComing
		}
		if sig != "" && !swapComing && time.Since(sigAt) >= playbackSessionEvery {
			nowSig, sigAt = mediaSignature(tunerip), time.Now()
			if nowSig != "" && nowSig != sig {
				swapComing = true
				logger("[PLAYBACK] %s media session changed after %v, waiting for audio to start",
					tunerip, time.Since(t0).Round(time.Millisecond))
			}
		}
		if sig != "" && nowSig == sig && samePiids(base, last) {
			if staticSince.IsZero() {
				staticSince = time.Now()
			} else if time.Since(staticSince) >= staticFor {
				logger("[PLAYBACK] %s kept the player and the session it already had (%s), so a tune cannot be seen from here; gating on motion after %v",
					tunerip, piidList(last), time.Since(t0).Round(time.Millisecond))
				return swapComing
			}
		} else {
			staticSince = time.Time{}
		}
		time.Sleep(playbackPoll)
	}
	session := "not readable"
	if sig != "" {
		session = map[bool]string{true: "unchanged", false: "changed"}[nowSig == sig]
	}
	logger("[PLAYBACK] %s not confirmed within %v, gating on motion alone; baseline had %s, the box now has %s, media session %s",
		tunerip, budget, piidList(base), piidList(last), session)
	return swapComing
}

// flushWriter is a response that can be pushed all the way out.
type flushWriter interface {
	io.Writer
	Flush()
}

// copyFlush is io.Copy, flushed after every write.
func copyFlush(dst flushWriter, src io.Reader) (int64, error) {
	buf := make([]byte, 32*1024)
	var n int64
	for {
		r, rerr := src.Read(buf)
		if r > 0 {
			w, werr := dst.Write(buf[:r])
			n += int64(w)
			if werr != nil {
				return n, werr
			}
			dst.Flush()
		}
		if rerr != nil {
			if rerr == io.EOF {
				return n, nil
			}
			return n, rerr
		}
	}
}

// readWithDeadline does r.Read with a timeout: on expiry the body is closed,
// unblocking the blocked Read with an error. No goroutine leak, no buf race.
func readWithDeadline(r io.ReadCloser, buf []byte, timeout time.Duration) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	defer context.AfterFunc(ctx, func() { r.Close() })()
	return r.Read(buf)
}

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
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	"github.com/natefinch/lumberjack"
	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/disk"
	"github.com/shirou/gopsutil/mem"
	gopsutilNet "github.com/shirou/gopsutil/net"
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
	Started  string
	FileName string
	Cmd      string
}

type Entry struct {
	Id          string `json:"id"`
	StationId   string `json:"stationId"`
	ChannelName string `json:"channelName"`
	StreamURL   string `json:"streamURL"`
	Logo        string `json:"Logo"`
	Group       string `json:"Group"`
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
		defer r.cmdMutex.Unlock()
		r.teecmd = nil
		r.teecmdIn = nil
		r.teecmdRunning = false
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
			if err := execute(r.t.start, r.channel, r.t.tunerip); err != nil {
				logger("[ERR] Failed to run start script: %v", err)
				return
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
	go func(data []byte) {
		if allowPreview {
			// Write to file
			if _, err := r.file.Write(data[:n]); err != nil {
				logger("Error while writing to preview file")
			}
		}
	}(p[:n])
	// Write to TEECMD if enabled
	if r.t.teecmd != "" {
		go func(data []byte) {
			if _, err := r.teecmdIn.Write(data[:n]); err != nil {
				logger("Error while writing to TEECMD")
			}
		}(p[:n])
	}
	return n, err
}

// Called from io.Copy when closing socket
func (r *reader) Close() error {
	logger("Performing Close() for %s", r.t.tunerip)
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

// Tune into a application or network encoder
func tune(idx, channel string) (io.ReadCloser, error) {
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
					continue
				}
				go func() {
					cmd.Wait()
					pipeWriter.Close()
				}()
				if err := execute(t.pre, t.tunerip, channel); err != nil {
					logger("[ERR] Failed to run pre script: %v", err)
					continue
				}
				t.active = true
				t.index = i
				return &reader{
					ReadCloser: pipeReader,
					channel:    channel,
					t:          t,
					cmd:        cmd,
				}, nil
			}
			// Network encoder
			logger("Attempting network tune for device %s %s %v %v", t.url, t.tunerip, channel, idx)
			resp, err := http.Get(t.url)
			if err != nil {
				logger("[ERR] Failed to fetch source: %v", err)
				continue
			} else if resp.StatusCode != 200 {
				logger("[ERR] Failed to fetch source: %v", resp.Status)
				continue
			}
			if err := execute(t.pre, t.tunerip, channel); err != nil {
				logger("[ERR] Failed to run pre script: %v %s", err, t.tunerip)
				continue
			}
			t.active = true
			t.index = i
			return &reader{
				ReadCloser: resp.Body,
				channel:    channel,
				t:          t,
			}, nil
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
	r.StaticFile("/favicon.ico", "./static/favicon.ico")
	//	r.Use(CustomRecovery())
	r.LoadHTMLGlob("html/*")
	r.StaticFS("/static", http.Dir("static"))
	r.GET("/", func(c *gin.Context) {
		r.LoadHTMLGlob("html/*")
		routes := r.Routes()
		c.HTML(http.StatusOK, "index.html", routes)
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
		reader, err := tune(tuner, channel)
		if err != nil {
			logger("[ERR] Failed to tune %s", err)
			errorMessage := fmt.Sprintf("<html><body><h1>Error: %s</h1></body></html>", err.Error())
			c.Data(500, "text/html; charset=utf-8", []byte(errorMessage))
			return
		}
		c.Header("Transfer-Encoding", "identity")
		c.Header("Content-Type", "video/mp2t")
		c.Writer.WriteHeaderNow()
		c.Writer.Flush()
		defer func() {
			reader.Close()
		}()
		starttime := time.Now()
		var bytesCopied int64
		if bytesCopied, err = io.Copy(c.Writer, reader); err != nil {
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
		var builder strings.Builder
		builder.WriteString("<pre>\n")
		for _, val := range env {
			builder.WriteString(val)
			builder.WriteString("\n")
		}
		builder.WriteString("</pre>")
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(builder.String()))
	})
	// Show raw logs
	r.GET("/logs/text", func(c *gin.Context) {
		content, err := os.ReadFile("/tmp/ah4c.log")
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
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
			// Check if entry.ChannelName begins with #
			if strings.HasPrefix(entry.ChannelName, "#") {
				// Adjust the #EXTINF line and entry.ChannelName
				extinfLine := fmt.Sprintf(
					"# EXTINF:-1 channel-id=\"%s\" tvc-guide-stationid=\"%s\" tvg-group=\"%s\" tvg-logo=\"%s\",#%s\n",
					entry.Id,
					entry.StationId,
					entry.Group, // Added the tvg-group field
					entry.Logo,  // Added the tvg-logo field
					entry.ChannelName,
				)
				_, err = writer.WriteString(extinfLine)
				if err != nil {
					c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
					return
				}
			} else {
				// Original code for entries without # at the beginning of ChannelName
				extinfLine := fmt.Sprintf(
					"#EXTINF:-1 channel-id=\"%s\" tvc-guide-stationid=\"%s\" tvg-group=\"%s\" tvg-logo=\"%s\",%s\n",
					entry.Id,
					entry.StationId,
					entry.Group, // Added the tvg-group field
					entry.Logo,  // Added the tvg-logo field
					entry.ChannelName,
				)
				_, err = writer.WriteString(extinfLine)
				if err != nil {
					c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
					return
				}
			}
			_, err = writer.WriteString(fmt.Sprintf("%s\n\n", entry.StreamURL)) // use StreamURL here
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
			if strings.HasPrefix(line, "#EXTINF:") {
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
	return r.Run(":7654")
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
	logger("[START] ah4c is starting")
	loadenv()
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
			Started:  fmt.Sprintf("%v", r.started),
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

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

// A staged preview shows the picture from an encoder address typed into a
// Settings or setup-wizard tuner card before it is saved, so a person can read
// an Apple TV pairing PIN or approve an Android prompt without another player.
// It is worth far less than any tune: it is refused while one is starting or
// running, and it ends the moment one begins.

const stagedPreviewBusyReason = "A recording or live stream is starting or playing, so the preview is paused until it finishes."

// stagedPreviewWatchInterval is how often a running preview looks for a tune.
var stagedPreviewWatchInterval = 50 * time.Millisecond

var stagedPreviewClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		TLSHandshakeTimeout:   4 * time.Second,
		ResponseHeaderTimeout: 6 * time.Second,
		IdleConnTimeout:       time.Second,
		DisableKeepAlives:     true,
	},
	CheckRedirect: func(request *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if _, err := stagedPreviewURL(request.URL.String()); err != nil {
			return err
		}
		return nil
	},
}

func registerStagedPreviewRoute(r *gin.Engine) {
	r.GET("/api/config/preview", stagedPreviewHandler)
}

// stagedPreviewURL accepts only a plain http or https address with a host.
func stagedPreviewURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	if raw == "" || err != nil || parsed.Hostname() == "" || parsed.User != nil {
		return nil, errors.New("enter the full encoder stream address, starting with http:// or https://")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		return parsed, nil
	}
	return nil, errors.New("enter the full encoder stream address, starting with http:// or https://")
}

// stagedPreviewBusy reports whether a tune is starting or running. It never
// waits for tunerLock: a tune holds it through its pre script and encoder open,
// and a lock held by anything counts as a tune, so the preview yields to doubt.
func stagedPreviewBusy() bool {
	if tunesPending() || !tunerLock.TryLock() {
		return true
	}
	defer tunerLock.Unlock()
	for i := range tuners {
		if tuners[i].active {
			return true
		}
	}
	return false
}

func stagedPreviewHandler(c *gin.Context) {
	target, err := stagedPreviewURL(c.Query("url"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Enter the full encoder stream address, starting with http:// or https://."})
		return
	}
	if stagedPreviewBusy() {
		c.JSON(http.StatusConflict, gin.H{"error": stagedPreviewBusyReason})
		return
	}
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	var yielded atomic.Bool
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(stagedPreviewWatchInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if stagedPreviewBusy() {
					yielded.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Enter the full encoder stream address, starting with http:// or https://."})
		return
	}
	response, err := stagedPreviewClient.Do(request)
	if yielded.Load() {
		if response != nil {
			response.Body.Close()
		}
		logger("[PREVIEW] Stopped the Settings preview of %s because a tune started", target.Redacted())
		c.JSON(http.StatusConflict, gin.H{"error": stagedPreviewBusyReason})
		return
	}
	if err != nil {
		if c.Request.Context().Err() == nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "ah4c could not get video from this encoder address."})
		}
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("The encoder answered %s instead of sending video.", response.Status)})
		return
	}
	c.Header("Content-Type", "video/mp2t")
	c.Header("Cache-Control", "no-store")
	c.Writer.WriteHeaderNow()
	c.Writer.Flush()
	buffer := make([]byte, 64*1024)
	for {
		n, readErr := response.Body.Read(buffer)
		if n > 0 {
			if _, writeErr := c.Writer.Write(buffer[:n]); writeErr != nil {
				return
			}
			c.Writer.Flush()
		}
		if readErr != nil {
			if yielded.Load() {
				logger("[PREVIEW] Stopped the Settings preview of %s because a tune started", target.Redacted())
			}
			return
		}
	}
}

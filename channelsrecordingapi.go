package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const channelsRecordingRequestTimeout = 7 * time.Second

type channelsDVRStatus struct {
	Activity map[string]string `json:"activity"`
	Busy     bool              `json:"busy"`
}

type channelsDVRJob struct {
	ID       string `json:"ID"`
	DeviceID string `json:"DeviceID"`
	Channel  string `json:"Channel"`
	Name     string `json:"Name"`
}

type channelsDVRChannel struct {
	ID          string `json:"ID"`
	GuideNumber string `json:"GuideNumber"`
	GuideName   string `json:"GuideName"`
}

type channelsDVRDevice struct {
	DeviceID string               `json:"DeviceID"`
	Channels []channelsDVRChannel `json:"Channels"`
}

type channelsRecording struct {
	JobID       string `json:"jobID"`
	Detail      string `json:"detail"`
	Channel     string `json:"channel,omitempty"`
	ChannelName string `json:"channelName,omitempty"`
}

func registerChannelsRecordingRoutes(r *gin.Engine) {
	r.GET("/api/channels/recordings", getChannelsRecordingsHandler)
	r.DELETE("/api/channels/recordings/:job", cancelChannelsRecordingHandler)
}

func channelsRecordingJobID(activityKey, detail string) (string, bool) {
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(detail)), "recording ") {
		return "", false
	}
	const marker = "-job-"
	markerIndex := strings.Index(activityKey, marker)
	if markerIndex < 0 {
		return "", false
	}
	jobID := strings.TrimSpace(activityKey[markerIndex+len(marker):])
	if jobID == "" {
		return "", false
	}
	for _, char := range jobID {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '.' || char == '_' || char == '-' {
			continue
		}
		return "", false
	}
	return jobID, true
}

func channelsDVRBaseURL() (string, error) {
	address := strings.TrimSpace(os.Getenv("CHANNELSIP"))
	if address == "" {
		return "", fmt.Errorf("Channels DVR address is not configured")
	}
	return serverBaseURL(address, "8089")
}

func fetchChannelsDVRJSON(ctx context.Context, requestURL string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Channels DVR returned %s", response.Status)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(target); err != nil {
		return fmt.Errorf("could not read Channels DVR response: %w", err)
	}
	return nil
}

func fetchChannelsDVRStatus(ctx context.Context, baseURL string) (channelsDVRStatus, error) {
	var status channelsDVRStatus
	if err := fetchChannelsDVRJSON(ctx, baseURL+"/dvr", &status); err != nil {
		return channelsDVRStatus{}, err
	}
	if status.Activity == nil {
		status.Activity = map[string]string{}
	}
	return status, nil
}

func activeChannelsRecordingJobs(status channelsDVRStatus) map[string]string {
	jobs := make(map[string]string)
	for key, detail := range status.Activity {
		if jobID, ok := channelsRecordingJobID(key, detail); ok {
			jobs[jobID] = detail
		}
	}
	return jobs
}

func channelsGuideName(device channelsDVRDevice, channel string) string {
	for _, candidate := range device.Channels {
		if candidate.ID == channel || candidate.GuideNumber == channel {
			return candidate.GuideName
		}
	}
	return ""
}

func getChannelsRecordingsHandler(c *gin.Context) {
	baseURL, err := channelsDVRBaseURL()
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), channelsRecordingRequestTimeout)
	defer cancel()
	status, err := fetchChannelsDVRStatus(ctx, baseURL)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("could not check Channels DVR activity: %v", err)})
		return
	}
	jobs := activeChannelsRecordingJobs(status)
	recordings := make([]channelsRecording, 0, len(jobs))
	devices := make(map[string]channelsDVRDevice)
	for jobID, detail := range jobs {
		recording := channelsRecording{JobID: jobID, Detail: detail}
		var job channelsDVRJob
		if err := fetchChannelsDVRJSON(ctx, baseURL+"/dvr/jobs/"+url.PathEscape(jobID), &job); err == nil {
			recording.Channel = job.Channel
			if job.DeviceID != "" {
				device, cached := devices[job.DeviceID]
				if !cached {
					if err := fetchChannelsDVRJSON(ctx, baseURL+"/devices/"+url.PathEscape(job.DeviceID), &device); err == nil {
						devices[job.DeviceID] = device
					}
				}
				recording.ChannelName = channelsGuideName(device, job.Channel)
			}
		}
		recordings = append(recordings, recording)
	}
	sort.Slice(recordings, func(i, j int) bool { return recordings[i].Detail < recordings[j].Detail })
	c.JSON(http.StatusOK, gin.H{"busy": status.Busy, "activity": status.Activity, "recordings": recordings})
}

func cancelChannelsRecordingHandler(c *gin.Context) {
	baseURL, err := channelsDVRBaseURL()
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), channelsRecordingRequestTimeout)
	defer cancel()
	status, err := fetchChannelsDVRStatus(ctx, baseURL)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("could not check Channels DVR activity: %v", err)})
		return
	}
	jobs := activeChannelsRecordingJobs(status)
	jobID := c.Param("job")
	detail, active := jobs[jobID]
	if !active {
		c.JSON(http.StatusOK, gin.H{"status": "already_stopped", "job": jobID})
		return
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, baseURL+"/dvr/jobs/"+url.PathEscape(jobID), nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("could not stop the Channels DVR recording: %v", err)})
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("Channels DVR could not stop the recording: %s", response.Status)})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "stopped", "job": jobID, "activity": detail})
}

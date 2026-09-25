package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

// Picture profiles for Device Control, and which picture each device
// starts with. The page writes a picture where ws-scrcpy keeps its own saved
// video settings before it opens a stream; this file only keeps the choices,
// in the config directory beside settings.json, so every browser sees the
// same ones and they survive a new container.

const (
	maxDevicePictures    = 20
	maxDeviceAssignments = 100
)

// The page's own pictures, which a device may start with as well as a custom one.
var builtInDevicePictures = map[string]bool{"standard": true, "light": true, "sharp": true, "compat": true}

// encoderSoftware asks the page to pick the device's software H.264 encoder,
// whose name differs by Android version.
const encoderSoftware = "@software"

var (
	devicePicturesMu           sync.Mutex
	devicePicturesPathOverride string

	devicePictureIDPattern      = regexp.MustCompile(`^custom-[a-z0-9-]{1,40}$`)
	devicePictureEncoderPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	deviceSerialPattern         = regexp.MustCompile(`^[A-Za-z0-9._:\[\]-]{1,100}$`)
)

type devicePicture struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Width          int    `json:"width"`
	Height         int    `json:"height"`
	Bitrate        int    `json:"bitrate"`
	MaxFps         int    `json:"maxFps"`
	IFrameInterval int    `json:"iFrameInterval"`
	Encoder        string `json:"encoder"`
}

type devicePicturesFile struct {
	Version  int             `json:"version"`
	Pictures []devicePicture `json:"pictures"`
	// Assignments maps a device's adb address to the picture it starts with.
	Assignments map[string]string `json:"assignments"`
}

func registerDevicePictureRoutes(r *gin.Engine) {
	r.GET("/api/device/pictures", getDevicePicturesHandler)
	r.PUT("/api/device/pictures", putDevicePicturesHandler)
}

func devicePicturesPath() string {
	if devicePicturesPathOverride != "" {
		return devicePicturesPathOverride
	}
	return filepath.Join(filepath.Dir(settingsFilePath()), "device-pictures.json")
}

// loadDevicePictures also returns a revision of what it read, so a save
// made from a page showing an older copy can be refused rather than
// silently replacing what another browser saved in between.
func loadDevicePictures() (devicePicturesFile, string, error) {
	f := devicePicturesFile{Version: 1, Pictures: []devicePicture{}, Assignments: map[string]string{}}
	b, err := os.ReadFile(devicePicturesPath())
	if errors.Is(err, os.ErrNotExist) {
		return f, "", nil
	}
	if err != nil {
		return f, "", err
	}
	sum := sha256.Sum256(b)
	revision := hex.EncodeToString(sum[:8])
	if err := json.Unmarshal(b, &f); err != nil {
		return f, revision, fmt.Errorf("device-pictures.json is not valid: %w", err)
	}
	if f.Pictures == nil {
		f.Pictures = []devicePicture{}
	}
	if f.Assignments == nil {
		f.Assignments = map[string]string{}
	}
	return f, revision, nil
}

func saveDevicePictures(f devicePicturesFile) (string, error) {
	f.Version = 1
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return "", err
	}
	b = append(b, '\n')
	path := devicePicturesPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8]), nil
}

// normalizeDevicePictures checks a complete list before it replaces the saved
// one. It trims names, fills the keyframe spacing when it is left out, and
// rejects anything the page could not hand to ws-scrcpy as it stands.
func normalizeDevicePictures(in []devicePicture) ([]devicePicture, error) {
	if len(in) > maxDevicePictures {
		return nil, fmt.Errorf("at most %d profiles can be saved", maxDevicePictures)
	}
	out := make([]devicePicture, 0, len(in))
	ids := map[string]bool{}
	names := map[string]bool{}
	for _, p := range in {
		p.Name = strings.TrimSpace(p.Name)
		p.Encoder = strings.TrimSpace(p.Encoder)
		if p.IFrameInterval == 0 {
			p.IFrameInterval = 10
		}
		label := p.Name
		if label == "" {
			label = "a profile"
		}
		switch {
		case !devicePictureIDPattern.MatchString(p.ID):
			return nil, fmt.Errorf("%s has an invalid id", label)
		case ids[p.ID]:
			return nil, fmt.Errorf("%s has the same id as another profile", label)
		case p.Name == "" || len([]rune(p.Name)) > 40:
			return nil, errors.New("each profile needs a name of 1 to 40 characters")
		case names[strings.ToLower(p.Name)]:
			return nil, fmt.Errorf("there is already a profile named %q", p.Name)
		case p.Width < 128 || p.Width > 4096 || p.Height < 128 || p.Height > 4096:
			return nil, fmt.Errorf("%s: width and height must each be between 128 and 4096", p.Name)
		case p.Bitrate < 250_000 || p.Bitrate > 50_000_000:
			return nil, fmt.Errorf("%s: quality must be between 0.25 and 50 Mbps", p.Name)
		case p.MaxFps < 1 || p.MaxFps > 60:
			return nil, fmt.Errorf("%s: frame rate must be between 1 and 60", p.Name)
		case p.IFrameInterval < 1 || p.IFrameInterval > 60:
			return nil, fmt.Errorf("%s: the keyframe interval must be between 1 and 60 seconds", p.Name)
		case p.Encoder != "" && p.Encoder != encoderSoftware && !devicePictureEncoderPattern.MatchString(p.Encoder):
			return nil, fmt.Errorf("%s: %q is not a compression choice the Android device offers", p.Name, p.Encoder)
		}
		ids[p.ID] = true
		names[strings.ToLower(p.Name)] = true
		out = append(out, p)
	}
	return out, nil
}

// normalizeDeviceAssignments checks which picture each device starts with
// against the pictures that will exist once the save lands. An empty picture
// means the device has no starting picture of its own.
func normalizeDeviceAssignments(in map[string]string, pictures []devicePicture) (map[string]string, error) {
	known := map[string]bool{}
	for _, p := range pictures {
		known[p.ID] = true
	}
	out := map[string]string{}
	for serial, key := range in {
		if !deviceSerialPattern.MatchString(serial) {
			return nil, fmt.Errorf("%q is not a device address", serial)
		}
		if key == "" {
			continue
		}
		if !builtInDevicePictures[key] && !known[key] {
			return nil, fmt.Errorf("%s starts with a profile that no longer exists", serial)
		}
		out[serial] = key
	}
	if len(out) > maxDeviceAssignments {
		return nil, fmt.Errorf("at most %d devices can have a starting picture", maxDeviceAssignments)
	}
	return out, nil
}

func getDevicePicturesHandler(c *gin.Context) {
	devicePicturesMu.Lock()
	defer devicePicturesMu.Unlock()
	f, revision, err := loadDevicePictures()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"pictures": f.Pictures, "assignments": f.Assignments, "revision": revision})
}

func putDevicePicturesHandler(c *gin.Context) {
	var req struct {
		Pictures    *[]devicePicture  `json:"pictures"`
		Assignments map[string]string `json:"assignments"`
		// Revision is what the page last read; the save is refused if the
		// file has changed since.
		Revision string `json:"revision"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
	if err := c.ShouldBindJSON(&req); err != nil || req.Pictures == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "the profiles could not be read"})
		return
	}
	pictures, err := normalizeDevicePictures(*req.Pictures)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	assignments, err := normalizeDeviceAssignments(req.Assignments, pictures)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	devicePicturesMu.Lock()
	defer devicePicturesMu.Unlock()
	current, revision, err := loadDevicePictures()
	if err == nil && req.Revision != revision {
		c.JSON(http.StatusConflict, gin.H{
			"error":       "The profiles were changed in another browser. They have been reloaded, so make your change again.",
			"pictures":    current.Pictures,
			"assignments": current.Assignments,
			"revision":    revision,
		})
		return
	}
	// A file that cannot be read is replaced by a deliberate save rather than
	// blocking every save for ever.
	revision, err = saveDevicePictures(devicePicturesFile{Pictures: pictures, Assignments: assignments})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"pictures": pictures, "assignments": assignments, "revision": revision})
}

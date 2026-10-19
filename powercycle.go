package main

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
)

// POWERCYCLEn is a shell command that cuts and restores power to tuner n's
// streaming device, usually a curl to a smart plug or a PoE switch port. It is
// for a stick that has stopped answering adb, which Reboot cannot reach.
//
// The command is read from the environment when the button is pressed rather
// than copied into the tuner at startup, so one saved in Settings works
// without a restart. Nothing runs it on its own: a power cycle ends whatever
// the tuner is streaming, so it only ever happens because someone asked.

// A command typically turns the outlet off, sleeps a few seconds and turns it
// back on. This only bounds one that never returns.
var powerCycleTimeout = 60 * time.Second

var (
	powerCycleMu      sync.Mutex
	powerCycleRunning = map[int]bool{}
)

func powerCycleCommand(index int) string {
	return strings.TrimSpace(os.Getenv("POWERCYCLE" + strconv.Itoa(index+1)))
}

func registerPowerCycleRoutes(r *gin.Engine) {
	r.GET("/api/tuners/powercycle", func(c *gin.Context) {
		configured := make([]bool, len(tuners))
		for i := range tuners {
			configured[i] = powerCycleCommand(i) != ""
		}
		c.JSON(http.StatusOK, gin.H{"configured": configured})
	})
	r.POST("/api/tuner/:index/powercycle", powerCycleHandler)
}

func powerCycleHandler(c *gin.Context) {
	index, err := strconv.Atoi(c.Param("index"))
	if err != nil || index < 0 || index >= len(tuners) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid tuner index"})
		return
	}
	command := powerCycleCommand(index)
	if command == "" {
		c.JSON(http.StatusConflict, gin.H{"error": "no power cycle command is set for this tuner", "unconfigured": true})
		return
	}
	powerCycleMu.Lock()
	if powerCycleRunning[index] {
		powerCycleMu.Unlock()
		c.JSON(http.StatusConflict, gin.H{"error": "a power cycle is already running for this tuner"})
		return
	}
	powerCycleRunning[index] = true
	powerCycleMu.Unlock()
	defer func() {
		powerCycleMu.Lock()
		delete(powerCycleRunning, index)
		powerCycleMu.Unlock()
	}()

	tunerLock.Lock()
	streaming := tuners[index].active
	tunerLock.Unlock()
	if streaming {
		logger("[CONTROL] power cycle tuner %d (POWERCYCLE%d) while it is streaming", index, index+1)
	} else {
		logger("[CONTROL] power cycle tuner %d (POWERCYCLE%d)", index, index+1)
	}
	if err := runPowerCycle(command); err != nil {
		logger("[CONTROL] power cycle tuner %d failed: %v", index, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	logger("[CONTROL] power cycle tuner %d finished", index)
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// runPowerCycle runs the command through a shell, so it can chain an off, a
// sleep and an on. A failure carries the tail of what the command printed,
// since a curl that reached the wrong address says so there and nowhere else.
func runPowerCycle(command string) error {
	ctx, cancel := context.WithTimeout(context.Background(), powerCycleTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	// Killing only the shell would leave its curl or sleep running, holding
	// the output open, so the command gets its own process group and the
	// timeout kills all of it. WaitDelay is the backstop if that fails.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return &powerCycleError{"timed out after " + powerCycleTimeout.String(), output.String()}
	}
	if err != nil {
		return &powerCycleError{err.Error(), output.String()}
	}
	return nil
}

type powerCycleError struct {
	reason string
	output string
}

func (e *powerCycleError) Error() string {
	output := strings.TrimSpace(e.output)
	if len(output) > 300 {
		output = "…" + output[len(output)-300:]
	}
	if output == "" {
		return e.reason
	}
	return e.reason + ": " + output
}

// powerCycleChanges names the POWERCYCLEn commands that differ between two
// tuner lists, for the log line that reports a save.
func powerCycleChanges(old, next []TunerSpec) []string {
	var changed []string
	for i := 0; i < len(old) || i < len(next); i++ {
		if powerCycleAt(old, i) != powerCycleAt(next, i) {
			changed = append(changed, "POWERCYCLE"+strconv.Itoa(i+1))
		}
	}
	return changed
}

// applyPowerCycleCommands puts commands saved in Settings into effect at once.
// A tuner removed from the list loses its command now, even though the tuner
// itself stays until the restart: its button would otherwise power cycle the
// outlet of a device that is no longer meant to be there.
func applyPowerCycleCommands(old, next []TunerSpec) {
	for i := 0; i < len(old) || i < len(next); i++ {
		key := "POWERCYCLE" + strconv.Itoa(i+1)
		if command := powerCycleAt(next, i); command != "" {
			_ = os.Setenv(key, command)
		} else {
			_ = os.Unsetenv(key)
		}
	}
}

func powerCycleAt(list []TunerSpec, index int) string {
	if index < len(list) {
		return list[index].PowerCycle
	}
	return ""
}

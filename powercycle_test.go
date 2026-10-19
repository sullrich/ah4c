package main

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPowerCycleChangeAloneNeedsNoRestart(t *testing.T) {
	old := []TunerSpec{{TunerIP: "stick1:5555"}, {TunerIP: "stick2:5555"}}
	next := []TunerSpec{{TunerIP: "stick1:5555", PowerCycle: "curl -s http://plug1/cycle"}, {TunerIP: "stick2:5555"}}
	if tunerSettingsChanged(old, next) {
		t.Fatal("a POWERCYCLE change alone asked for a restart")
	}
	next[1].TunerIP = "stick3:5555"
	if !tunerSettingsChanged(old, next) {
		t.Fatal("a Tuner IP change did not ask for a restart")
	}
}

func TestPowerCycleChangesNamesEachCommand(t *testing.T) {
	old := []TunerSpec{{PowerCycle: "a"}, {PowerCycle: "b"}, {PowerCycle: "c"}}
	next := []TunerSpec{{PowerCycle: "a"}, {PowerCycle: "B"}}
	got := powerCycleChanges(old, next)
	want := []string{"POWERCYCLE2", "POWERCYCLE3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("powerCycleChanges = %v, want %v", got, want)
	}
}

func TestApplyPowerCycleCommandsClearsRemovedTuners(t *testing.T) {
	t.Setenv("POWERCYCLE1", "old one")
	t.Setenv("POWERCYCLE2", "old two")
	applyPowerCycleCommands(
		[]TunerSpec{{PowerCycle: "old one"}, {PowerCycle: "old two"}},
		[]TunerSpec{{PowerCycle: "new one"}},
	)
	if got := os.Getenv("POWERCYCLE1"); got != "new one" {
		t.Fatalf("POWERCYCLE1 = %q, want the saved command", got)
	}
	if _, present := os.LookupEnv("POWERCYCLE2"); present {
		t.Fatal("POWERCYCLE2 survived its tuner being removed")
	}
}

func TestSynthesizedTunerVarsIncludePowerCycle(t *testing.T) {
	vars := synthesizeTunerVars([]TunerSpec{{PowerCycle: "curl -s http://plug1/cycle"}})
	if vars["POWERCYCLE1"] != "curl -s http://plug1/cycle" {
		t.Fatalf("POWERCYCLE1 = %q", vars["POWERCYCLE1"])
	}
	if !tunerKeyPattern.MatchString("POWERCYCLE12") {
		t.Fatal("POWERCYCLEn is not treated as a tuner key")
	}
}

func TestRunPowerCycleReportsWhatTheCommandSaid(t *testing.T) {
	if err := runPowerCycle("true"); err != nil {
		t.Fatalf("a command that succeeded failed: %v", err)
	}
	err := runPowerCycle("echo plug1 unreachable >&2; exit 7")
	if err == nil || !strings.Contains(err.Error(), "plug1 unreachable") {
		t.Fatalf("error %v does not carry the command's output", err)
	}
}

func TestRunPowerCycleIsBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the timeout")
	}
	saved := powerCycleTimeout
	powerCycleTimeout = 200 * time.Millisecond
	defer func() { powerCycleTimeout = saved }()
	started := time.Now()
	err := runPowerCycle("sleep 30")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a timeout", err)
	}
	// Well inside WaitDelay: the process group was killed, not waited out.
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("the timeout took %s to take effect", elapsed)
	}
}

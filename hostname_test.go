package main

import "testing"

func TestPageName(t *testing.T) {
	for hostname, want := range map[string]string{
		"ah4c":         "ah4c",
		"ah4c2":        "ah4c2",
		"LivingRoom":   "LivingRoom",
		" ah4c3\n":     "ah4c3",
		"":             "",
		"3f2a9c1b7d4e": "",
		"3f2a9c1b7d4e3f2a9c1b7d4e3f2a9c1b7d4e3f2a9c1b7d4e3f2a9c1b7d4e3f2a": "",
		"3f2a9c1b7d4":   "3f2a9c1b7d4",
		"deadbeefcafe1": "deadbeefcafe1",
	} {
		if got := pageName(hostname); got != want {
			t.Errorf("pageName(%q) = %q, want %q", hostname, got, want)
		}
	}
}

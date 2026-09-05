package main

import "testing"

func TestBuildVersionHonorsInjectedRelease(t *testing.T) {
	if got := buildVersion("v0.1.0"); got != "v0.1.0" {
		t.Fatalf("buildVersion=%q", got)
	}
}

package main

import "testing"

func TestLaunchForgejoURLFrom(t *testing.T) {
	if got := launchForgejoURLFrom("http://forgejo:3000", ""); got != "http://forgejo:3000" {
		t.Errorf("empty override: got %q, want fallback to FORGEJO_URL", got)
	}
	if got := launchForgejoURLFrom("http://forgejo:3000", "http://172.16.0.1:3000"); got != "http://172.16.0.1:3000" {
		t.Errorf("override set: got %q, want override", got)
	}
}

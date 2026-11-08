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

func TestValidateLaunchForgejoURL(t *testing.T) {
	for _, ok := range []string{"http://forgejo:3000", "http://172.16.0.1:3000", "https://git.example.com/"} {
		if err := validateLaunchForgejoURL(ok); err != nil {
			t.Errorf("validateLaunchForgejoURL(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"172.16.0.1:3000", "ftp://host", "http://", "http://:3000"} {
		if err := validateLaunchForgejoURL(bad); err == nil {
			t.Errorf("validateLaunchForgejoURL(%q) = nil, want error", bad)
		}
	}
}

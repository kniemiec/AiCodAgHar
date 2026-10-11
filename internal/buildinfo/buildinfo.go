// Package buildinfo identifies the running binary (HL-3). Version and Commit
// are set at link time:
//
//	-ldflags "-X github.com/kniemiec/AiCodAgHar/internal/buildinfo.Version=v1.2.3 \
//	          -X github.com/kniemiec/AiCodAgHar/internal/buildinfo.Commit=abc1234"
//
// When unset they fall back to the VCS data the Go toolchain embeds.
package buildinfo

import "runtime/debug"

var (
	Version = ""
	Commit  = ""
)

func init() {
	if Version != "" && Commit != "" {
		return
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		fill("dev", "unknown")
		return
	}
	var rev string
	var dirty bool
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if rev != "" && dirty {
		rev += "-dirty"
	}
	fill("dev", rev)
}

// fill sets any field still empty; an explicit ldflags value always wins.
func fill(version, commit string) {
	if Version == "" {
		Version = version
	}
	if Commit == "" {
		if commit == "" {
			commit = "unknown"
		}
		Commit = commit
	}
}

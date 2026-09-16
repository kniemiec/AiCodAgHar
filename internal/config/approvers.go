package config

import (
	"bufio"
	"bytes"
	"strings"
)

// ParseApprovers parses .harness/APPROVERS (design §4.4): one Forgejo
// username per line; blank lines and lines starting with "#" are ignored.
// It's the authorization list a webhook-verified label event's labeling
// user is checked against — VerifyCollaboratorRole is a live sanity check
// on top of it, not a substitute for it.
func ParseApprovers(data []byte) []string {
	var approvers []string
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		approvers = append(approvers, line)
	}
	return approvers
}

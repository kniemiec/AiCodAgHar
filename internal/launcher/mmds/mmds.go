// Package mmds injects credentials and task data into a booting guest via
// Firecracker's MMDS device (ADR-10) — the mechanism that keeps secrets off
// the boot-time/image surface entirely (design §7): they exist only in the
// MMDS response body the guest explicitly fetches at runtime, over a V2
// session-token handshake it must complete first.
//
// This package owns the MMDS-specific mechanics (V2 enforcement, the
// config-before-data-before-boot ordering Firecracker requires) on top of
// fcapi's generic HTTP client. It has no opinion on what the data actually
// is — the launcher passes it the launch request's own Env map directly;
// prompt rendering (which keys mean what, and the templated task string)
// is the prompts package, layered on top in a later step.
package mmds

import (
	"context"

	"github.com/kniemiec/AiCodAgHar/internal/launcher/fcapi"
)

// version is the only mode this launcher ever configures. V1 exposes MMDS
// data to anything that can reach the link-local address, no guest-side
// authentication at all — it fails ADR-1/ADR-2's "credential never touches
// an unauthenticated surface" requirement, so it's not exposed as a choice
// here.
const version = "V2"

// Configure switches the guest's MMDS device into V2 (session-token-
// required) mode. Must be called before Inject and before InstanceStart.
// networkInterfaces lists which attached network interfaces' iface_ids may
// actually serve MMDS requests (design §7/§10) — confirmed against the
// real API by the Firecracker PoC's Step 3
// (docs/plans/firecracker-microvm-launch-poc.md); pass nil/empty when no
// network interface is attached yet.
func Configure(ctx context.Context, fc *fcapi.Client, networkInterfaces []string) error {
	return fc.PutMMDSConfig(ctx, fcapi.MMDSConfig{Version: version, NetworkInterfaces: networkInterfaces})
}

// Inject writes data as the MMDS data document the guest fetches over the
// V2 handshake. Must be called after Configure and before InstanceStart —
// Firecracker requires MMDS to already carry the data a guest's very first
// boot-time fetch expects to see.
func Inject(ctx context.Context, fc *fcapi.Client, data map[string]string) error {
	return fc.PutMMDS(ctx, data)
}

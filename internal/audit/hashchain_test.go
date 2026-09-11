package audit

import "testing"

func buildChain(t *testing.T, n int) []InvocationAuditRecord {
	t.Helper()
	var records []InvocationAuditRecord
	prev := GenesisHash
	for i := 0; i < n; i++ {
		rec := InvocationAuditRecord{
			InvocationID: taskInvocationID(i),
			TaskID:       "task-1",
			Round:        1,
			StepType:     "agent-stage",
			ExitCode:     0,
			Status:       "completed",
		}
		hash, err := ComputeRowHash(rec, prev)
		if err != nil {
			t.Fatalf("ComputeRowHash: %v", err)
		}
		rec.PrevHash = prev
		rec.RowHash = hash
		records = append(records, rec)
		prev = hash
	}
	return records
}

func taskInvocationID(i int) string {
	return "task-1:step-" + string(rune('a'+i))
}

func TestVerifyChain_IntactChainPasses(t *testing.T) {
	records := buildChain(t, 3)
	if err := VerifyChain(records); err != nil {
		t.Fatalf("expected an intact chain to verify, got: %v", err)
	}
}

func TestVerifyChain_TamperedContentFails(t *testing.T) {
	records := buildChain(t, 3)
	records[1].Status = "tampered" // content changed without recomputing row_hash

	err := VerifyChain(records)
	if err == nil {
		t.Fatal("expected a tampered row's content mismatch to be detected")
	}
}

func TestVerifyChain_BrokenLinkFails(t *testing.T) {
	records := buildChain(t, 3)
	records[2].PrevHash = "some-other-hash" // chain link broken, even if row_hash itself is self-consistent

	err := VerifyChain(records)
	if err == nil {
		t.Fatal("expected a broken prev_hash link to be detected")
	}
}

func TestVerifyChain_EmptyChainPasses(t *testing.T) {
	if err := VerifyChain(nil); err != nil {
		t.Fatalf("expected an empty chain to trivially verify, got: %v", err)
	}
}

func TestComputeRowHash_DeterministicForSameInput(t *testing.T) {
	rec := InvocationAuditRecord{InvocationID: "x", TaskID: "t", Round: 1, StepType: "agent-stage"}
	h1, err := ComputeRowHash(rec, GenesisHash)
	if err != nil {
		t.Fatalf("ComputeRowHash: %v", err)
	}
	h2, err := ComputeRowHash(rec, GenesisHash)
	if err != nil {
		t.Fatalf("ComputeRowHash: %v", err)
	}
	if h1 != h2 {
		t.Fatalf("expected identical input to hash identically, got %q vs %q", h1, h2)
	}
}

func TestComputeRowHash_DifferentPrevHashDiffers(t *testing.T) {
	rec := InvocationAuditRecord{InvocationID: "x", TaskID: "t", Round: 1, StepType: "agent-stage"}
	h1, _ := ComputeRowHash(rec, "prev-a")
	h2, _ := ComputeRowHash(rec, "prev-b")
	if h1 == h2 {
		t.Fatal("expected different prevHash to produce different row_hash")
	}
}

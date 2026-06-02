package store

import (
	"testing"
	"time"

	"github.com/rancher/gitsim/pkg/core"
)

func TestBranchStateResolve_ImmediateNoPromotion(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	mc := core.NewManualClock(base)

	bs := &branchState{visibleSHA: "aaa"}
	sha, promoted := bs.resolve(mc)
	if sha != "aaa" || promoted {
		t.Fatalf("want sha=aaa promoted=false, got sha=%s promoted=%v", sha, promoted)
	}
}

func TestBranchStateResolve_PendingNotYetDue(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	mc := core.NewManualClock(base)

	bs := &branchState{
		visibleSHA: "aaa",
		pendingSHA: "bbb",
		promoteAt:  base.Add(500 * time.Millisecond),
	}

	// Clock has not advanced — should still see "aaa".
	sha, promoted := bs.resolve(mc)
	if sha != "aaa" || promoted {
		t.Fatalf("want sha=aaa promoted=false, got sha=%s promoted=%v", sha, promoted)
	}
	if bs.pendingSHA != "bbb" {
		t.Fatal("pendingSHA should not be cleared before promotion")
	}
}

func TestBranchStateResolve_PendingDue(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	mc := core.NewManualClock(base)

	bs := &branchState{
		visibleSHA: "aaa",
		pendingSHA: "bbb",
		promoteAt:  base.Add(500 * time.Millisecond),
	}

	mc.Advance(600 * time.Millisecond)
	sha, promoted := bs.resolve(mc)
	if sha != "bbb" || !promoted {
		t.Fatalf("want sha=bbb promoted=true, got sha=%s promoted=%v", sha, promoted)
	}
	if bs.pendingSHA != "" {
		t.Fatal("pendingSHA should be cleared after promotion")
	}
}

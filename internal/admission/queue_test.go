package admission

import (
	"testing"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/model"
)

func vessels(t *testing.T, names ...string) []model.Vessel {
	t.Helper()
	result := make([]model.Vessel, 0, len(names))
	for _, name := range names {
		vessel, err := model.NewVessel(name, "CS-"+name, 10)
		if err != nil {
			t.Fatalf("new vessel: %v", err)
		}
		result = append(result, vessel)
	}
	return result
}

func names(entries []QueueEntry) []string {
	result := make([]string, 0, len(entries))
	for _, entry := range entries {
		result = append(result, entry.Vessel.Name)
	}
	return result
}

func equalNames(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("position %d got %q want %q (full %v)", index, got[index], want[index], got)
		}
	}
}

func TestQueueReplacePlanKeepsAnchorAndOthers(t *testing.T) {
	queue := NewQueue()
	chamber := "chamber-1"
	planA := uuid.New()
	planB := uuid.New()

	aVessels := vessels(t, "a1", "a2")
	bVessels := vessels(t, "b1", "b2", "b3")
	if err := queue.Add(chamber, planA, aVessels); err != nil {
		t.Fatal(err)
	}
	if err := queue.Add(chamber, planB, bVessels); err != nil {
		t.Fatal(err)
	}
	equalNames(t, names(queue.List(chamber)), "a1", "a2", "b1", "b2", "b3")

	// Reverse plan A's order and add one vessel: it must stay at the head.
	aUpdated := []model.Vessel{aVessels[1], aVessels[0]}
	newVessel := vessels(t, "a3")[0]
	aUpdated = append(aUpdated, newVessel)
	removed := queue.ReplacePlanEntries(chamber, planA, []QueueEntry{
		{Vessel: aUpdated[0], PlanID: planA},
		{Vessel: aUpdated[1], PlanID: planA},
		{Vessel: aUpdated[2], PlanID: planA},
	})
	equalNames(t, names(removed), "a1", "a2")
	equalNames(t, names(queue.List(chamber)), "a2", "a1", "a3", "b1", "b2", "b3")

	// Replace a middle plan: B must keep its position after A.
	bUpdated := []model.Vessel{bVessels[2]} // shrink to b3 only
	queue.ReplacePlanEntries(chamber, planB, []QueueEntry{{Vessel: bUpdated[0], PlanID: planB}})
	equalNames(t, names(queue.List(chamber)), "a2", "a1", "a3", "b3")
}

func TestQueueReplacePlanRollbackRestoresReleased(t *testing.T) {
	queue := NewQueue()
	chamber := "chamber-1"
	planA := uuid.New()
	planB := uuid.New()
	aVessels := vessels(t, "a1", "a2")
	if err := queue.Add(chamber, planA, aVessels); err != nil {
		t.Fatal(err)
	}
	if err := queue.Add(chamber, planB, vessels(t, "b1")); err != nil {
		t.Fatal(err)
	}
	if !queue.MarkReleased(chamber, aVessels[0].ID) {
		t.Fatal("mark released")
	}

	// Replace A's two vessels with a single (non-empty, as revisions guarantee)
	// block: the anchor at the head is preserved.
	replacement := vessels(t, "a9")
	block := queue.ReplacePlanEntries(chamber, planA, []QueueEntry{{Vessel: replacement[0], PlanID: planA}})
	equalNames(t, names(queue.List(chamber)), "a9", "b1")

	// Rollback with the exact captured block restores order AND released flags.
	queue.ReplacePlanEntries(chamber, planA, block)
	restored := queue.List(chamber)
	equalNames(t, names(restored), "a1", "a2", "b1")
	if !restored[0].Released {
		t.Fatal("released flag was not restored on rollback")
	}
	if restored[1].Released {
		t.Fatal("unreleased vessel gained the flag")
	}
}

func TestQueueReplacePlanEmptyBlockAnchorsTailWhenAbsent(t *testing.T) {
	queue := NewQueue()
	chamber := "chamber-1"
	planA := uuid.New()
	planB := uuid.New()
	if err := queue.Add(chamber, planA, vessels(t, "a1")); err != nil {
		t.Fatal(err)
	}
	if err := queue.Add(chamber, planB, vessels(t, "b1")); err != nil {
		t.Fatal(err)
	}
	// Removing the head leaves B; reintroducing an absent plan anchors at tail.
	queue.ReplacePlanEntries(chamber, planA, nil)
	equalNames(t, names(queue.List(chamber)), "b1")
	queue.ReplacePlanEntries(chamber, planA, []QueueEntry{{Vessel: vessels(t, "a2")[0], PlanID: planA}})
	equalNames(t, names(queue.List(chamber)), "b1", "a2")
}

func TestQueuePlanReleasedAndMarkMany(t *testing.T) {
	queue := NewQueue()
	chamber := "chamber-1"
	planA := uuid.New()
	planB := uuid.New()
	aVessels := vessels(t, "a1", "a2")
	if err := queue.Add(chamber, planA, aVessels); err != nil {
		t.Fatal(err)
	}
	if err := queue.Add(chamber, planB, vessels(t, "b1")); err != nil {
		t.Fatal(err)
	}
	if queue.PlanReleased(chamber, planA) {
		t.Fatal("plan should not be released")
	}
	queue.MarkManyReleased(chamber, []uuid.UUID{aVessels[0].ID, aVessels[1].ID})
	if !queue.PlanReleased(chamber, planA) {
		t.Fatal("plan should be released")
	}
	if queue.PlanReleased(chamber, planB) {
		t.Fatal("other plan must not be reported released")
	}
}

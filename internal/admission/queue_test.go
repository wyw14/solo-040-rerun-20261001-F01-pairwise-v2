package admission

import (
	"testing"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/model"
)

func queueVessel(t *testing.T, name string) model.Vessel {
	t.Helper()
	vessel, err := model.NewVessel(name, name, 40)
	if err != nil {
		t.Fatal(err)
	}
	return vessel
}

func names(entries []QueueEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Vessel.Name
	}
	return out
}

func TestReplacePlanKeepsOtherPlansInPlace(t *testing.T) {
	q := NewQueue()
	const chamber = "c1"
	p1 := queueVessel(t, "a1")
	p1b := queueVessel(t, "a2")
	other := queueVessel(t, "b1")
	planA := uuid.New()
	planB := uuid.New()

	if err := q.Add(chamber, planA, []model.Vessel{p1, p1b}); err != nil {
		t.Fatal(err)
	}
	if err := q.Add(chamber, planB, []model.Vessel{other}); err != nil {
		t.Fatal(err)
	}
	// Initial order: a1(A) a2(A) b1(B)
	got := names(q.List(chamber))
	if len(got) != 3 {
		t.Fatalf("setup order wrong: %v", got)
	}

	// Reverse plan A and add a third vessel; B must remain last.
	a3 := queueVessel(t, "a3")
	change, displaced, err := q.ReplacePlan(chamber, planA, []model.Vessel{p1b, p1, a3})
	if err != nil {
		t.Fatal(err)
	}
	if len(change.Added) != 1 || len(change.Removed) != 0 {
		t.Fatalf("unexpected change %+v", change)
	}
	if len(displaced) != 2 {
		t.Fatalf("expected both old entries returned for rollback, got %d", len(displaced))
	}
	order := q.List(chamber)
	if len(order) != 4 {
		t.Fatalf("expected 4 entries, got %v", names(order))
	}
	if order[0].Vessel.Name != "a2" || order[1].Vessel.Name != "a1" || order[2].Vessel.Name != "a3" {
		t.Fatalf("plan A slots wrong: %v", names(order))
	}
	if order[3].Vessel.Name != "b1" || order[3].PlanID != planB {
		t.Fatalf("plan B must remain in its trailing slot: %v", names(order))
	}

	// Shrink plan A back; B still does not move.
	if _, _, err := q.ReplacePlan(chamber, planA, []model.Vessel{p1}); err != nil {
		t.Fatal(err)
	}
	order = q.List(chamber)
	if len(order) != 2 || order[0].Vessel.Name != "a1" || order[1].Vessel.Name != "b1" {
		t.Fatalf("shrink did not preserve other plan slot: %v", names(order))
	}
}

func TestRestorePlanEntriesRollsBack(t *testing.T) {
	q := NewQueue()
	const chamber = "c1"
	a1 := queueVessel(t, "a1")
	a2 := queueVessel(t, "a2")
	planA := uuid.New()
	if err := q.Add(chamber, planA, []model.Vessel{a1, a2}); err != nil {
		t.Fatal(err)
	}
	_, displaced, err := q.ReplacePlan(chamber, planA, []model.Vessel{a2, a1})
	if err != nil {
		t.Fatal(err)
	}
	q.RestorePlanEntries(chamber, planA, displaced)
	order := q.List(chamber)
	if len(order) != 2 || order[0].Vessel.Name != "a1" || order[1].Vessel.Name != "a2" {
		t.Fatalf("rollback must restore original order, got %v", names(order))
	}
}

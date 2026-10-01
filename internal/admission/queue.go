package admission

import (
	"errors"
	"sync"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/model"
)

type QueueEntry struct {
	Vessel   model.Vessel `json:"vessel"`
	PlanID   uuid.UUID    `json:"plan_id"`
	Released bool         `json:"released"`
}

// Queue preserves FIFO order while supporting convoy cancellation recovery.
type Queue struct {
	mu      sync.RWMutex
	entries map[string][]QueueEntry
}

func NewQueue() *Queue { return &Queue{entries: make(map[string][]QueueEntry)} }

func (q *Queue) Add(chamberID string, planID uuid.UUID, vessels []model.Vessel) error {
	if chamberID == "" || planID == uuid.Nil || len(vessels) == 0 {
		return errors.New("queue addition requires chamber, plan and vessels")
	}
	q.mu.Lock()
	for _, vessel := range vessels {
		q.entries[chamberID] = append(q.entries[chamberID], QueueEntry{Vessel: vessel, PlanID: planID})
	}
	q.mu.Unlock()
	return nil
}

func (q *Queue) Peek(chamberID string) (QueueEntry, bool) {
	q.mu.RLock()
	entries := q.entries[chamberID]
	q.mu.RUnlock()
	if len(entries) == 0 {
		return QueueEntry{}, false
	}
	return entries[0], true
}

func (q *Queue) MarkReleased(chamberID string, vesselID uuid.UUID) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for index := range q.entries[chamberID] {
		if q.entries[chamberID][index].Vessel.ID == vesselID {
			q.entries[chamberID][index].Released = true
			return true
		}
	}
	return false
}

// MarkManyReleased flags every matching vessel as released in one lock step.
func (q *Queue) MarkManyReleased(chamberID string, vesselIDs []uuid.UUID) {
	wanted := make(map[uuid.UUID]struct{}, len(vesselIDs))
	for _, id := range vesselIDs {
		wanted[id] = struct{}{}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for index := range q.entries[chamberID] {
		if _, ok := wanted[q.entries[chamberID][index].Vessel.ID]; ok {
			q.entries[chamberID][index].Released = true
		}
	}
}

// PlanReleased reports whether any queued entry of the plan is already released.
func (q *Queue) PlanReleased(chamberID string, planID uuid.UUID) bool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	for _, entry := range q.entries[chamberID] {
		if entry.PlanID == planID && entry.Released {
			return true
		}
	}
	return false
}

func (q *Queue) RemovePlan(chamberID string, planID uuid.UUID) []QueueEntry {
	q.mu.Lock()
	defer q.mu.Unlock()
	entries := q.entries[chamberID]
	kept := entries[:0]
	var removed []QueueEntry
	for _, entry := range entries {
		if entry.PlanID == planID {
			removed = append(removed, entry)
		} else {
			kept = append(kept, entry)
		}
	}
	q.entries[chamberID] = append([]QueueEntry(nil), kept...)
	return removed
}

// ReplacePlanEntries swaps every entry owned by planID for the supplied block
// in one locked step. The block is inserted at the anchor of the plan's first
// current entry so the relative order of other plans is preserved; when the
// plan is absent the block is appended. Supplied entries keep their Released
// flags, which lets a saga restore an exact prior block on rollback.
// It returns the entries that were replaced.
func (q *Queue) ReplacePlanEntries(chamberID string, planID uuid.UUID, block []QueueEntry) []QueueEntry {
	q.mu.Lock()
	defer q.mu.Unlock()
	current := q.entries[chamberID]
	keptBefore := 0
	seenPlan := false
	removed := make([]QueueEntry, 0)
	kept := make([]QueueEntry, 0, len(current))
	for _, entry := range current {
		if entry.PlanID == planID {
			if !seenPlan {
				seenPlan = true
				keptBefore = len(kept)
			}
			removed = append(removed, entry)
		} else {
			kept = append(kept, entry)
		}
	}
	if !seenPlan {
		// Plan not currently queued: anchor at the tail.
		keptBefore = len(kept)
	}
	updated := make([]QueueEntry, 0, len(kept)+len(block))
	updated = append(updated, kept[:keptBefore]...)
	updated = append(updated, block...)
	updated = append(updated, kept[keptBefore:]...)
	q.entries[chamberID] = updated
	return removed
}

func (q *Queue) Restore(chamberID string, entries []QueueEntry) {
	q.mu.Lock()
	current := q.entries[chamberID]
	restored := make([]QueueEntry, 0, len(entries)+len(current))
	for _, entry := range entries {
		entry.Released = false
		restored = append(restored, entry)
	}
	q.entries[chamberID] = append(restored, current...)
	q.mu.Unlock()
}

func (q *Queue) List(chamberID string) []QueueEntry {
	q.mu.RLock()
	result := append([]QueueEntry(nil), q.entries[chamberID]...)
	q.mu.RUnlock()
	return result
}

package admission

import (
	"errors"
	"sort"
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

// PlanQueueChange reports how a plan revision touched the waiting queue.
type PlanQueueChange struct {
	Added   []uuid.UUID `json:"added"`
	Removed []uuid.UUID `json:"removed"`
	Order   []uuid.UUID `json:"order"`
}

// ReplacePlan atomically rewrites the vessels occupying a plan's queue slots
// using slot mapping: slots belonging to other plans never move, and the
// plan's vessels are projected onto its own slot sequence in the new order.
// Extra new vessels are inserted directly after the plan's last slot;
// surplus old slots are dropped. All rewritten slots start unreleased, since
// a revision is only permitted before the transit has admitted anything.
// It returns the displaced entries so the caller can roll back on failure.
func (q *Queue) ReplacePlan(chamberID string, planID uuid.UUID, vessels []model.Vessel) (PlanQueueChange, []QueueEntry, error) {
	if chamberID == "" || planID == uuid.Nil || len(vessels) == 0 {
		return PlanQueueChange{}, nil, errors.New("queue replacement requires chamber, plan and vessels")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	entries := q.entries[chamberID]
	slots := make([]int, 0)
	previous := make(map[uuid.UUID]QueueEntry)
	var previousOrder []uuid.UUID
	for index, entry := range entries {
		if entry.PlanID == planID {
			slots = append(slots, index)
			previous[entry.Vessel.ID] = entry
			previousOrder = append(previousOrder, entry.Vessel.ID)
		}
	}
	change := PlanQueueChange{}
	next := make(map[uuid.UUID]struct{}, len(vessels))
	for _, vessel := range vessels {
		if _, dup := next[vessel.ID]; dup {
			return PlanQueueChange{}, nil, errors.New("revision contains duplicate vessel")
		}
		next[vessel.ID] = struct{}{}
	}
	for _, vesselID := range previousOrder {
		if _, ok := next[vesselID]; !ok {
			change.Removed = append(change.Removed, vesselID)
		}
	}

	rebuilt := make([]QueueEntry, 0, len(entries)+len(vessels))
	vesselIndex := 0
	for index, entry := range entries {
		if entry.PlanID != planID {
			rebuilt = append(rebuilt, entry)
			continue
		}
		isLastSlot := index == slots[len(slots)-1]
		if vesselIndex < len(vessels) {
			rebuilt = append(rebuilt, QueueEntry{Vessel: vessels[vesselIndex], PlanID: planID})
			vesselIndex++
		}
		if isLastSlot {
			for ; vesselIndex < len(vessels); vesselIndex++ {
				rebuilt = append(rebuilt, QueueEntry{Vessel: vessels[vesselIndex], PlanID: planID})
			}
		}
	}
	if len(slots) == 0 {
		for _, vessel := range vessels {
			rebuilt = append(rebuilt, QueueEntry{Vessel: vessel, PlanID: planID})
		}
	}
	q.entries[chamberID] = rebuilt

	displaced := make([]QueueEntry, 0, len(previousOrder))
	for _, vesselID := range previousOrder {
		displaced = append(displaced, previous[vesselID])
	}
	for _, vessel := range vessels {
		change.Order = append(change.Order, vessel.ID)
		if _, ok := previous[vessel.ID]; !ok {
			change.Added = append(change.Added, vessel.ID)
		}
	}
	sort.Slice(change.Added, func(i, j int) bool { return change.Added[i].String() < change.Added[j].String() })
	sort.Slice(change.Removed, func(i, j int) bool { return change.Removed[i].String() < change.Removed[j].String() })
	return change, displaced, nil
}

// RestorePlanEntries is the compensating operation for ReplacePlan: it swaps
// the plan's current queue slots back for the displaced entries, preserving
// their original released flags.
func (q *Queue) RestorePlanEntries(chamberID string, planID uuid.UUID, displaced []QueueEntry) {
	if len(displaced) == 0 {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	entries := q.entries[chamberID]
	slots := make([]int, 0)
	for index, entry := range entries {
		if entry.PlanID == planID {
			slots = append(slots, index)
		}
	}
	rebuilt := make([]QueueEntry, 0, len(entries)+len(displaced))
	entryIndex := 0
	for index, entry := range entries {
		if entry.PlanID != planID {
			rebuilt = append(rebuilt, entry)
			continue
		}
		isLastSlot := len(slots) > 0 && index == slots[len(slots)-1]
		if entryIndex < len(displaced) {
			rebuilt = append(rebuilt, displaced[entryIndex])
			entryIndex++
		}
		if isLastSlot {
			for ; entryIndex < len(displaced); entryIndex++ {
				rebuilt = append(rebuilt, displaced[entryIndex])
			}
		}
	}
	if len(slots) == 0 {
		rebuilt = append(rebuilt, displaced...)
	}
	q.entries[chamberID] = rebuilt
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

package schedule

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/model"
	"github.com/wyw14/cry-96/internal/signal"
)

// RevisionErrorCode classifies why a revision request could not be applied.
type RevisionErrorCode string

const (
	RevisionNotFound    RevisionErrorCode = "not_found"
	RevisionInvalid     RevisionErrorCode = "invalid"
	RevisionNotEditable RevisionErrorCode = "not_editable"
	RevisionConflict    RevisionErrorCode = "version_conflict"
)

// RevisionError carries the machine-readable code and the current version so
// the console can show the rejection reason without a second round trip.
type RevisionError struct {
	Code           RevisionErrorCode
	Reason         string
	PlanID         uuid.UUID
	CurrentVersion uint32
}

func (e *RevisionError) Error() string { return e.Reason }

func revisionError(plan model.ConvoyPlan, code RevisionErrorCode, reason string) *RevisionError {
	return &RevisionError{Code: code, Reason: reason, PlanID: plan.ID, CurrentVersion: plan.Version}
}

// RevisionItem is one entry of the full replacement sequence. A non-nil
// VesselID references a vessel already on the plan (order/keep); a nil
// VesselID adds a new vessel described by Name/CallSign/LengthM.
type RevisionItem struct {
	VesselID uuid.UUID
	Name     string
	CallSign string
	LengthM  float64
}

// RevisionResult summarizes the committed version and its queue effect.
type RevisionResult struct {
	Plan        model.ConvoyPlan `json:"plan"`
	Version     uint32           `json:"version"`
	Added       int              `json:"added"`
	Removed     int              `json:"removed"`
	Reordered   bool             `json:"reordered"`
	QueueLength int              `json:"queue_length"`
}

type revisionChange struct {
	vessels   []model.Vessel
	added     int
	removed   []uuid.UUID
	reordered bool
}

// buildChange validates the full sequence against the current plan.
func buildChange(plan model.ConvoyPlan, items []RevisionItem) (revisionChange, error) {
	if len(items) == 0 {
		return revisionChange{}, errors.New("revised plan must contain at least one vessel")
	}
	current := make(map[uuid.UUID]model.Vessel, len(plan.Vessels))
	for _, vessel := range plan.Vessels {
		current[vessel.ID] = vessel
	}
	change := revisionChange{vessels: make([]model.Vessel, 0, len(items))}
	seen := make(map[uuid.UUID]struct{}, len(items))
	for index, item := range items {
		if item.VesselID != uuid.Nil {
			vessel, ok := current[item.VesselID]
			if !ok {
				return revisionChange{}, fmt.Errorf("vessel %s is not part of plan %s", item.VesselID, plan.ID)
			}
			if _, dup := seen[item.VesselID]; dup {
				return revisionChange{}, fmt.Errorf("vessel %s listed more than once", item.VesselID)
			}
			seen[item.VesselID] = struct{}{}
			change.vessels = append(change.vessels, vessel)
			continue
		}
		vessel, err := model.NewVessel(item.Name, item.CallSign, item.LengthM)
		if err != nil {
			return revisionChange{}, fmt.Errorf("new vessel at position %d: %w", index, err)
		}
		change.vessels = append(change.vessels, vessel)
		change.added++
	}
	newIDs := make(map[uuid.UUID]struct{}, len(change.vessels))
	for _, vessel := range change.vessels {
		newIDs[vessel.ID] = struct{}{}
	}
	for _, vessel := range plan.Vessels {
		if _, kept := newIDs[vessel.ID]; !kept {
			change.removed = append(change.removed, vessel.ID)
		}
	}
	// A reorder is any change in the relative order of the retained vessels,
	// independent of additions or removals.
	oldKept := make([]uuid.UUID, 0, len(plan.Vessels))
	for _, vessel := range plan.Vessels {
		if _, kept := newIDs[vessel.ID]; kept {
			oldKept = append(oldKept, vessel.ID)
		}
	}
	newKept := make([]uuid.UUID, 0, len(oldKept))
	for _, vessel := range change.vessels {
		if _, existing := current[vessel.ID]; existing {
			newKept = append(newKept, vessel.ID)
		}
	}
	for index := range oldKept {
		if oldKept[index] != newKept[index] {
			change.reordered = true
			break
		}
	}
	if change.added == 0 && len(change.removed) == 0 && !change.reordered {
		return revisionChange{}, errors.New("no changes to apply")
	}
	return change, nil
}

// Revise atomically rewrites a plan's vessel sequence while it is still
// pre-admission. Queue and unpublished signals are synced in one locked saga.
func (p *Planner) Revise(ctx context.Context, planID uuid.UUID, expectedVersion uint32, items []RevisionItem, note string, publisher *signal.Publisher) (RevisionResult, *RevisionError) {
	p.ops.Lock()
	defer p.ops.Unlock()

	plan, ok := p.Get(planID)
	if !ok {
		return RevisionResult{}, &RevisionError{Code: RevisionNotFound, Reason: "convoy plan not found", PlanID: planID}
	}
	change, err := buildChange(plan, items)
	if err != nil {
		return RevisionResult{}, revisionError(plan, RevisionInvalid, err.Error())
	}
	if allowed, reason := p.editable(plan); !allowed {
		return RevisionResult{}, revisionError(plan, RevisionNotEditable, reason)
	}
	if expectedVersion != plan.Version {
		return RevisionResult{}, revisionError(plan, RevisionConflict,
			fmt.Sprintf("plan version %d does not match expected version %d", plan.Version, expectedVersion))
	}

	previousBlock := p.admission.ReplacePlan(plan.ChamberID, plan.ID, change.vessels)
	dropped, dropErr := publisher.DropUnpublished(ctx, plan.ChamberID, change.removed)
	if dropErr != nil {
		p.admission.RestoreBlock(plan.ChamberID, plan.ID, previousBlock)
		return RevisionResult{}, revisionError(plan, RevisionNotEditable, dropErr.Error())
	}

	nextVersion := plan.Version + 1
	if _, journalErr := p.recorder.Record(ctx, plan.ChamberID, model.EventPlanRevised, plan.ID, uint64(nextVersion), map[string]any{
		"plan_id": plan.ID, "version": nextVersion,
		"added": change.added, "removed": change.removed, "reordered": change.reordered,
		"queue_length": len(p.admission.List(plan.ChamberID)),
	}); journalErr != nil {
		if compensateErr := publisher.Compensate(context.WithoutCancel(ctx), plan.ChamberID, dropped); compensateErr != nil {
			journalErr = errors.Join(journalErr, compensateErr)
		}
		p.admission.RestoreBlock(plan.ChamberID, plan.ID, previousBlock)
		return RevisionResult{}, revisionError(plan, RevisionInvalid, journalErr.Error())
	}

	plan.Vessels = append([]model.Vessel(nil), change.vessels...)
	plan.Version = nextVersion
	plan.UpdatedAt = p.now().UTC()
	p.replace(plan)
	p.appendHistory(plan.ID, model.PlanRevision{
		Version: nextVersion, Vessels: append([]model.Vessel(nil), plan.Vessels...),
		ChangedAt: plan.UpdatedAt, Note: note,
	})

	return RevisionResult{
		Plan: plan.Clone(), Version: nextVersion, Added: change.added,
		Removed: len(change.removed), Reordered: change.reordered,
		QueueLength: len(p.admission.List(plan.ChamberID)),
	}, nil
}

func (p *Planner) appendHistory(planID uuid.UUID, revision model.PlanRevision) {
	p.mu.Lock()
	p.history[planID] = append(p.history[planID], revision)
	p.mu.Unlock()
}

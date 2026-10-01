package schedule

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/admission"
	"github.com/wyw14/cry-96/internal/model"
	"github.com/wyw14/cry-96/internal/signal"
)

// TransitStatus reports the live transit generation for a chamber.
type TransitStatus interface {
	Current(string) (model.TransitCycle, bool)
}

// DeviceActivity reports whether a transit generation has already driven
// physical equipment (gate or valve commands).
type DeviceActivity interface {
	HasActivity(cycleID uuid.UUID, generation uint64) bool
}

// RevisionRejectedError is returned when a plan cannot be rewritten because the
// transit has already moved past the planning window. It carries the current
// version so operators can refresh against authoritative state.
type RevisionRejectedError struct {
	PlanID  uuid.UUID `json:"plan_id"`
	Version uint64    `json:"version"`
	Stage   string    `json:"stage"`
	Reason  string    `json:"reason"`
}

func (e *RevisionRejectedError) Error() string {
	return fmt.Sprintf("revision rejected for plan %s (version %d): %s", e.PlanID, e.Version, e.Reason)
}

// RevisionResult describes the committed revision and its downstream effects.
type RevisionResult struct {
	Plan             model.ConvoyPlan          `json:"plan"`
	FromVersion      uint64                    `json:"from_version"`
	ToVersion        uint64                    `json:"to_version"`
	Queue            admission.PlanQueueChange `json:"queue"`
	WithdrawnSignals []uuid.UUID               `json:"signals_withdrawn"`
	HeadReason       string                    `json:"head_reason,omitempty"`
}

// Revise rewrites a plan's vessel order and membership while the convoy has
// not yet entered the lock and no equipment has moved. The plan identity is
// preserved and every prior version is retained on the plan history.
func (p *Planner) Revise(
	ctx context.Context,
	planID uuid.UUID,
	expectedVersion uint64,
	vessels []model.Vessel,
	note string,
	publisher *signal.Publisher,
) (RevisionResult, error) {
	plan, err := p.require(planID)
	if err != nil {
		return RevisionResult{}, err
	}
	if len(vessels) == 0 {
		return RevisionResult{}, errors.New("revision requires at least one vessel")
	}

	if reason := p.rejectReason(plan, expectedVersion); reason != "" {
		rejected := &RevisionRejectedError{PlanID: plan.ID, Version: plan.Version, Reason: reason}
		if cycleValue, ok := p.cycles.Current(plan.ChamberID); ok {
			rejected.Stage = string(cycleValue.Stage)
		}
		_, _ = p.recorder.Record(context.WithoutCancel(ctx), plan.ChamberID, model.EventPlanRejected, plan.ID, plan.Version, map[string]any{
			"version": plan.Version, "stage": rejected.Stage, "reason": reason, "at": p.now().UTC(),
		})
		return RevisionResult{}, rejected
	}

	cycleValue, _ := p.cycles.Current(plan.ChamberID)
	now := p.now()
	revision, err := model.NewPlanRevision(plan.Version, plan.Vessels, now, note)
	if err != nil {
		return RevisionResult{}, err
	}
	updated := plan.Clone()
	updated.Vessels = append([]model.Vessel(nil), vessels...)
	updated.Version = plan.Version + 1
	updated.UpdatedAt = now.UTC()
	updated.History = append(append([]model.PlanRevision(nil), plan.History...), revision)

	// 1. Withdraw provisional approach signals for the live generation.
	signalRevision, err := publisher.ReviseSignals(ctx, plan.ChamberID, cycleValue.ID, cycleValue.Generation, plan.Vessels)
	if err != nil {
		return RevisionResult{}, errors.Join(errors.New("signal revision failed"), err)
	}
	// 2. Rewrite the waiting queue to the new order/membership.
	queueChange, displaced, err := p.admission.ReplacePlan(updated)
	if err != nil {
		_ = publisher.CompensateRevision(context.WithoutCancel(ctx), plan.ChamberID, signalRevision)
		return RevisionResult{}, err
	}
	// 3. Commit the revision; on journal failure restore queue and signals.
	if _, err := p.recorder.Record(ctx, plan.ChamberID, model.EventPlanRevised, plan.ID, updated.Version, map[string]any{
		"plan_id": plan.ID, "from_version": plan.Version, "to_version": updated.Version,
		"vessels": updated.Vessels, "queue": queueChange,
		"signals_withdrawn": signalRevision.Withdrawn, "note": note, "at": now.UTC(),
	}); err != nil {
		p.admission.RestorePlanEntries(plan, displaced)
		_ = publisher.CompensateRevision(context.WithoutCancel(ctx), plan.ChamberID, signalRevision)
		return RevisionResult{}, err
	}

	p.replace(updated)

	// 4. Re-grant the queue head its provisional approach authority, exactly
	// as admission evaluation does when a plan is first created.
	result := RevisionResult{
		Plan: updated, FromVersion: plan.Version, ToVersion: updated.Version,
		Queue: queueChange, WithdrawnSignals: signalRevision.Withdrawn,
	}
	if decision, admitErr := p.admission.Admit(context.WithoutCancel(ctx), cycleValue); admitErr != nil {
		return result, errors.Join(errors.New("revision committed but admission re-evaluation failed"), admitErr)
	} else if !decision.Allowed {
		result.HeadReason = decision.Reason
	}
	return result, nil
}

func (p *Planner) rejectReason(plan model.ConvoyPlan, expectedVersion uint64) string {
	if plan.Cancelled {
		return "plan is cancelled and cannot be revised"
	}
	if expectedVersion != 0 && expectedVersion != plan.Version {
		return fmt.Sprintf("plan version conflict: expected %d but current version is %d", expectedVersion, plan.Version)
	}
	cycleValue, ok := p.cycles.Current(plan.ChamberID)
	if !ok {
		return "no active transit for the plan"
	}
	if cycleValue.PlanID != plan.ID {
		return "plan is no longer bound to the active transit"
	}
	if cycleValue.Stage != model.StagePlanned {
		return fmt.Sprintf("convoy has already entered the lock (transit stage %s)", cycleValue.Stage)
	}
	if p.devices.HasActivity(cycleValue.ID, cycleValue.Generation) {
		return "equipment action has already started for this transit"
	}
	return ""
}

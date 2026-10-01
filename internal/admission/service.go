package admission

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/journal"
	"github.com/wyw14/cry-96/internal/model"
)

// Service records queue changes and delegates safe release decisions.
type Service struct {
	controller *Controller
	recorder   *journal.Recorder
	now        func() time.Time
}

func NewService(controller *Controller, recorder *journal.Recorder, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{controller: controller, recorder: recorder, now: now}
}

func (s *Service) Enqueue(ctx context.Context, plan model.ConvoyPlan) error {
	if err := s.controller.queue.Add(plan.ChamberID, plan.ID, plan.Vessels); err != nil {
		return err
	}
	_, err := s.recorder.Record(ctx, plan.ChamberID, model.EventPlanCreated, plan.ID, 0, map[string]any{
		"plan_id": plan.ID, "queued": len(plan.Vessels), "at": s.now().UTC(),
	})
	return err
}

func (s *Service) Admit(ctx context.Context, cycle model.TransitCycle) (Decision, error) {
	decision, err := s.controller.Evaluate(ctx, cycle)
	if err != nil || !decision.Allowed {
		return decision, err
	}
	_, err = s.recorder.Record(ctx, cycle.ChamberID, model.EventSignalChanged, cycle.ID, cycle.Generation, decision)
	return decision, err
}

func (s *Service) RemovePlan(chamberID string, plan model.ConvoyPlan) []QueueEntry {
	return s.controller.queue.RemovePlan(chamberID, plan.ID)
}

func (s *Service) Restore(chamberID string, entries []QueueEntry) {
	s.controller.queue.Restore(chamberID, entries)
}

// ReplacePlan swaps a plan's queued block for the supplied vessels at the same
// anchor. It returns the previous block verbatim (with Released flags) so a
// caller can restore it exactly by calling RestoreBlock on rollback.
func (s *Service) ReplacePlan(chamberID string, planID uuid.UUID, vessels []model.Vessel) []QueueEntry {
	block := make([]QueueEntry, 0, len(vessels))
	for _, vessel := range vessels {
		block = append(block, QueueEntry{Vessel: vessel, PlanID: planID})
	}
	return s.controller.queue.ReplacePlanEntries(chamberID, planID, block)
}

// RestoreBlock reinserts an exact queue block captured from ReplacePlan.
func (s *Service) RestoreBlock(chamberID string, planID uuid.UUID, block []QueueEntry) {
	s.controller.queue.ReplacePlanEntries(chamberID, planID, block)
}

func (s *Service) MarkReleased(chamberID string, vesselIDs []uuid.UUID) {
	s.controller.queue.MarkManyReleased(chamberID, vesselIDs)
}

func (s *Service) PlanReleased(chamberID string, planID uuid.UUID) bool {
	return s.controller.queue.PlanReleased(chamberID, planID)
}

func (s *Service) List(chamberID string) []QueueEntry {
	return s.controller.queue.List(chamberID)
}

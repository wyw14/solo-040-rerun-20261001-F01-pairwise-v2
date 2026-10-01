package schedule

import (
	"context"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/model"
	"github.com/wyw14/cry-96/internal/signal"
)

// Service exposes planning and signal-publication workflows to the API.
type Service struct {
	planner   *Planner
	publisher *signal.Publisher
}

func NewService(planner *Planner, publisher *signal.Publisher) *Service {
	return &Service{planner: planner, publisher: publisher}
}

func (s *Service) Create(ctx context.Context, chamberID string, direction model.Direction, vessels []model.Vessel) (model.ConvoyPlan, error) {
	return s.planner.Create(ctx, chamberID, direction, vessels)
}

func (s *Service) Publish(ctx context.Context, planID uuid.UUID, cycle model.TransitCycle) (signal.Publication, error) {
	return s.planner.Publish(ctx, planID, cycle, s.publisher)
}

// Revise rewrites a plan's vessel sequence as one atomic operation.
func (s *Service) Revise(ctx context.Context, planID uuid.UUID, expectedVersion uint32, items []RevisionItem, note string) (RevisionResult, *RevisionError) {
	return s.planner.Revise(ctx, planID, expectedVersion, items, note, s.publisher)
}

func (s *Service) Cancel(ctx context.Context, planID uuid.UUID, note string, publication signal.Publication) (Cancellation, error) {
	return s.planner.Cancel(ctx, planID, note, s.publisher, publication)
}

func (s *Service) Active(chamberID string) []model.ConvoyPlan {
	return s.planner.Active(chamberID)
}

func (s *Service) Get(planID uuid.UUID) (model.ConvoyPlan, bool) {
	return s.planner.Get(planID)
}

func (s *Service) Inspect(planID uuid.UUID) (PlanStatus, bool) {
	return s.planner.Inspect(planID)
}

func (s *Service) ActiveViews(chamberID string) []PlanStatus {
	return s.planner.ActiveViews(chamberID)
}

func (s *Service) History(planID uuid.UUID) []model.PlanRevision {
	return s.planner.History(planID)
}

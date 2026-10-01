package cycle

import (
	"context"
	"errors"

	"github.com/wyw14/cry-96/internal/admission"
	"github.com/wyw14/cry-96/internal/model"
)

// Service drives an operator-visible transit through its safe boundaries.
type Service struct {
	coordinator *Coordinator
	admission   *admission.Service
}

func NewService(coordinator *Coordinator, admissionService *admission.Service) *Service {
	return &Service{coordinator: coordinator, admission: admissionService}
}

// Plan registers the planned transit generation without admitting a vessel.
// Creation is thereby decoupled from release: no proceed signal is issued
// until the operator explicitly publishes it.
func (s *Service) Plan(ctx context.Context, plan model.ConvoyPlan) (model.TransitCycle, error) {
	return s.coordinator.Start(ctx, plan)
}

func (s *Service) Start(ctx context.Context, plan model.ConvoyPlan) (model.TransitCycle, error) {
	cycleValue, err := s.coordinator.Start(ctx, plan)
	if err != nil {
		return model.TransitCycle{}, err
	}
	decision, err := s.admission.Admit(ctx, cycleValue)
	if err != nil {
		return model.TransitCycle{}, err
	}
	if !decision.Allowed {
		return cycleValue, errors.New(decision.Reason)
	}
	return cycleValue, nil
}

func (s *Service) Current(chamberID string) (model.TransitCycle, bool) {
	return s.coordinator.Current(chamberID)
}

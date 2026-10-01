package schedule

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/admission"
	"github.com/wyw14/cry-96/internal/gate"
	"github.com/wyw14/cry-96/internal/journal"
	"github.com/wyw14/cry-96/internal/model"
	"github.com/wyw14/cry-96/internal/signal"
)

// CycleLookup is the planner's read-only view of active transit generations.
// It is bound after the cycle coordinator is constructed to avoid an
// import/construction cycle, mirroring signal.Service.BindTransitLookup.
type CycleLookup interface {
	Current(string) (model.TransitCycle, bool)
}

// Planner owns convoy plans, their revision history and queue membership.
type Planner struct {
	// ops serializes every multi-store write saga (revise, cancel, publish)
	// so their guard-then-act steps cannot interleave.
	ops       sync.Mutex
	mu        sync.RWMutex
	plans     map[uuid.UUID]model.ConvoyPlan
	history   map[uuid.UUID][]model.PlanRevision
	admission *admission.Service
	recorder  *journal.Recorder
	signals   *signal.Store
	commands  *gate.Commands
	cycles    CycleLookup
	now       func() time.Time
}

func NewPlanner(admissionService *admission.Service, recorder *journal.Recorder, now func() time.Time, signalStore *signal.Store, commands *gate.Commands) *Planner {
	if now == nil {
		now = time.Now
	}
	return &Planner{
		plans: make(map[uuid.UUID]model.ConvoyPlan), history: make(map[uuid.UUID][]model.PlanRevision),
		admission: admissionService, recorder: recorder,
		signals: signalStore, commands: commands, now: now,
	}
}

// BindCycleLookup wires the transit lookup once the coordinator exists.
func (p *Planner) BindCycleLookup(lookup CycleLookup) { p.cycles = lookup }

func (p *Planner) Create(ctx context.Context, chamberID string, direction model.Direction, vessels []model.Vessel) (model.ConvoyPlan, error) {
	plan, err := model.NewConvoyPlan(chamberID, direction, vessels, p.now())
	if err != nil {
		return model.ConvoyPlan{}, err
	}
	if err := p.admission.Enqueue(ctx, plan); err != nil {
		return model.ConvoyPlan{}, err
	}
	p.mu.Lock()
	p.plans[plan.ID] = plan
	p.mu.Unlock()
	if _, err := p.recorder.Record(ctx, chamberID, model.EventPlanCreated, plan.ID, 0, plan); err != nil {
		p.mu.Lock()
		delete(p.plans, plan.ID)
		p.mu.Unlock()
		p.admission.RemovePlan(chamberID, plan)
		return model.ConvoyPlan{}, err
	}
	p.mu.Lock()
	p.history[plan.ID] = []model.PlanRevision{{
		Version: plan.Version, Vessels: append([]model.Vessel(nil), plan.Vessels...), ChangedAt: plan.CreatedAt,
	}}
	p.mu.Unlock()
	return plan.Clone(), nil
}

func (p *Planner) Get(planID uuid.UUID) (model.ConvoyPlan, bool) {
	p.mu.RLock()
	plan, ok := p.plans[planID]
	p.mu.RUnlock()
	return plan.Clone(), ok
}

func (p *Planner) History(planID uuid.UUID) []model.PlanRevision {
	p.mu.RLock()
	revisions := p.history[planID]
	p.mu.RUnlock()
	result := make([]model.PlanRevision, len(revisions))
	for index := range revisions {
		result[index] = revisions[index]
		result[index].Vessels = append([]model.Vessel(nil), revisions[index].Vessels...)
	}
	return result
}

func (p *Planner) replace(plan model.ConvoyPlan) {
	p.mu.Lock()
	p.plans[plan.ID] = plan.Clone()
	p.mu.Unlock()
}

func (p *Planner) Active(chamberID string) []model.ConvoyPlan {
	p.mu.RLock()
	var result []model.ConvoyPlan
	for _, plan := range p.plans {
		if plan.ChamberID == chamberID && !plan.Cancelled {
			result = append(result, plan.Clone())
		}
	}
	p.mu.RUnlock()
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return result
}

func (p *Planner) require(planID uuid.UUID) (model.ConvoyPlan, error) {
	plan, ok := p.Get(planID)
	if !ok {
		return model.ConvoyPlan{}, errors.New("convoy plan not found")
	}
	return plan, nil
}

// editable is the single guard deciding whether a plan may still be rewritten.
// It performs only read checks and is safe to call for status display.
func (p *Planner) editable(plan model.ConvoyPlan) (bool, string) {
	if plan.Cancelled {
		return false, "plan is cancelled"
	}
	for _, vessel := range plan.Vessels {
		if state, ok := p.signals.Get(vessel.ID); ok && state.Aspect == model.SignalProceed {
			return false, "vessel already released (proceed signal)"
		}
	}
	if p.admission.PlanReleased(plan.ChamberID, plan.ID) {
		return false, "vessel already released (admission)"
	}
	if p.cycles == nil {
		return false, "transit status unavailable"
	}
	if cycleValue, ok := p.cycles.Current(plan.ChamberID); ok && cycleValue.PlanID == plan.ID {
		if cycleValue.Stage.Terminal() {
			return false, "transit already finished"
		}
		if cycleValue.Stage != model.StagePlanned {
			return false, "transit already admitted"
		}
		if p.commands != nil && p.commands.HasForCycle(cycleValue.ID) {
			return false, "device command already started"
		}
	}
	return true, ""
}

// PlanStatus exposes editability metadata for the operator console.
type PlanStatus struct {
	Plan           model.ConvoyPlan `json:"plan"`
	CurrentVersion uint32           `json:"current_version"`
	Editable       bool             `json:"editable"`
	RejectReason   string           `json:"reject_reason,omitempty"`
	Stage          string           `json:"stage,omitempty"`
	BoundCycle     bool             `json:"bound_cycle"`
	Revisions      int              `json:"revisions"`
}

func (p *Planner) Inspect(planID uuid.UUID) (PlanStatus, bool) {
	plan, ok := p.Get(planID)
	if !ok {
		return PlanStatus{}, false
	}
	return p.status(plan), true
}

func (p *Planner) ActiveViews(chamberID string) []PlanStatus {
	plans := p.Active(chamberID)
	views := make([]PlanStatus, 0, len(plans))
	for _, plan := range plans {
		views = append(views, p.status(plan))
	}
	return views
}

func (p *Planner) status(plan model.ConvoyPlan) PlanStatus {
	view := PlanStatus{
		Plan: plan.Clone(), CurrentVersion: plan.Version, Revisions: len(p.History(plan.ID)),
	}
	allowed, reason := p.editable(plan)
	view.Editable, view.RejectReason = allowed, reason
	if p.cycles != nil {
		if cycleValue, ok := p.cycles.Current(plan.ChamberID); ok && cycleValue.PlanID == plan.ID {
			view.BoundCycle = true
			view.Stage = string(cycleValue.Stage)
		}
	}
	return view
}

// Publish releases a plan's signals under the operation lock and marks the
// queue entries released, keeping both "released" indicators consistent.
func (p *Planner) Publish(ctx context.Context, planID uuid.UUID, cycleValue model.TransitCycle, publisher *signal.Publisher) (signal.Publication, error) {
	p.ops.Lock()
	defer p.ops.Unlock()
	plan, err := p.require(planID)
	if err != nil {
		return signal.Publication{}, err
	}
	if plan.Cancelled {
		return signal.Publication{}, errors.New("active convoy plan not found")
	}
	if plan.ChamberID != cycleValue.ChamberID || plan.ID != cycleValue.PlanID {
		return signal.Publication{}, errors.New("plan and transit do not match")
	}
	publication, err := publisher.Publish(ctx, cycleValue, plan.Vessels, model.SignalProceed)
	if err != nil {
		return publication, err
	}
	ids := make([]uuid.UUID, 0, len(plan.Vessels))
	for _, vessel := range plan.Vessels {
		ids = append(ids, vessel.ID)
	}
	p.admission.MarkReleased(plan.ChamberID, ids)
	return publication, nil
}

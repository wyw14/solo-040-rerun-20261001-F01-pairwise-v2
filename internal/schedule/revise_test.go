package schedule

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wyw14/cry-96/internal/admission"
	"github.com/wyw14/cry-96/internal/gate"
	"github.com/wyw14/cry-96/internal/journal"
	"github.com/wyw14/cry-96/internal/model"
	"github.com/wyw14/cry-96/internal/signal"
)

type fakeLookup struct {
	cycle model.TransitCycle
	ok    bool
}

func (f fakeLookup) Current(string) (model.TransitCycle, bool) { return f.cycle, f.ok }

type reviseFixture struct {
	planner   *Planner
	publisher *signal.Publisher
	store     *signal.Store
	commands  *gate.Commands
	adm       *admission.Service
	now       time.Time
}

func newReviseFixture(t *testing.T, stage model.TransitStage) (reviseFixture, model.ConvoyPlan, model.TransitCycle) {
	t.Helper()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	recorder := journal.NewRecorder(journal.NewMemoryStore(), clock)
	store := signal.NewStore()
	publisher := signal.NewPublisher(store, recorder, clock)
	queue := admission.NewQueue()
	gateState := gate.NewState("c1")
	controller := admission.NewController(queue, gateState, store, clock)
	adm := admission.NewService(controller, recorder, clock)
	commands := gate.NewCommands()
	planner := NewPlanner(adm, commands, recorder, clock)

	vessels := []model.Vessel{
		mustVessel(t, "River Star", "RS-1", 80),
		mustVessel(t, "Sea Bird", "SB-2", 70),
	}
	plan, err := planner.Create(context.Background(), "c1", model.DirectionUpstream, vessels)
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	cycleValue, err := model.NewTransitCycle("c1", plan.ID, model.DirectionUpstream, 1, now)
	if err != nil {
		t.Fatalf("new cycle: %v", err)
	}
	cycleValue.Stage = stage
	planner.BindTransitStatus(fakeLookup{cycle: cycleValue, ok: true})

	// Reproduce post-create admission: head vessel holds a provisional proceed.
	if stage == model.StagePlanned {
		if _, err := adm.Admit(context.Background(), cycleValue); err != nil {
			t.Fatalf("initial admit: %v", err)
		}
	}
	return reviseFixture{planner, publisher, store, commands, adm, now}, plan, cycleValue
}

func mustVessel(t *testing.T, name, callSign string, length float64) model.Vessel {
	t.Helper()
	vessel, err := model.NewVessel(name, callSign, length)
	if err != nil {
		t.Fatalf("new vessel: %v", err)
	}
	return vessel
}

func TestReviseReordersAndBumpsVersion(t *testing.T) {
	fixture, plan, _ := newReviseFixture(t, model.StagePlanned)
	ctx := context.Background()

	if _, ok := fixture.store.Get(plan.Vessels[0].ID); !ok {
		t.Fatal("head vessel should hold a provisional proceed signal")
	}

	reordered := []model.Vessel{plan.Vessels[1], plan.Vessels[0]}
	result, err := fixture.planner.Revise(ctx, plan.ID, plan.Version, reordered, "swap", fixture.publisher)
	if err != nil {
		t.Fatalf("revise: %v", err)
	}
	if result.ToVersion != 2 || result.Plan.Version != 2 {
		t.Fatalf("expected version 2, got %d", result.Plan.Version)
	}
	if len(result.Plan.History) != 1 || result.Plan.History[0].Version != 1 {
		t.Fatalf("expected history to retain v1, got %+v", result.Plan.History)
	}
	if result.Plan.ID != plan.ID {
		t.Fatal("plan identity must be preserved")
	}

	queue := fixture.adm.List("c1")
	if len(queue) != 2 || queue[0].Vessel.ID != plan.Vessels[1].ID || !queue[0].Released {
		t.Fatalf("new head should be Sea Bird and re-granted proceed, got %+v", queue)
	}
	if queue[1].Vessel.ID != plan.Vessels[0].ID {
		t.Fatalf("old head should be demoted to second slot, got %+v", queue)
	}
	if state, ok := fixture.store.Get(plan.Vessels[1].ID); !ok || state.Aspect != model.SignalProceed {
		t.Fatal("new queue head must hold a proceed signal")
	}
}

func TestReviseAddsAndRemovesVessels(t *testing.T) {
	fixture, plan, _ := newReviseFixture(t, model.StagePlanned)
	added := mustVessel(t, "Tide Runner", "TR-9", 55)
	next := []model.Vessel{plan.Vessels[0], added}

	result, err := fixture.planner.Revise(context.Background(), plan.ID, plan.Version, next, "edit", fixture.publisher)
	if err != nil {
		t.Fatalf("revise: %v", err)
	}
	if len(result.Queue.Added) != 1 || len(result.Queue.Removed) != 1 {
		t.Fatalf("expected one added and one removed, got %+v", result.Queue)
	}
	queue := fixture.adm.List("c1")
	if len(queue) != 2 || queue[1].Vessel.CallSign != "TR-9" {
		t.Fatalf("new vessel should be appended, got %+v", queue)
	}
	if _, ok := fixture.store.Get(plan.Vessels[1].ID); ok {
		t.Fatal("removed vessel signal must be withdrawn")
	}
}

func TestReviseRejectsAfterEquipmentAction(t *testing.T) {
	fixture, plan, cycleValue := newReviseFixture(t, model.StagePlanned)
	if _, err := fixture.commands.Issue(cycleValue, model.DeviceUpstreamGate, "open", 0, fixture.now); err != nil {
		t.Fatalf("issue command: %v", err)
	}
	_, err := fixture.planner.Revise(context.Background(), plan.ID, plan.Version, plan.Vessels, "x", fixture.publisher)
	var rejected *RevisionRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("expected RevisionRejectedError, got %v", err)
	}
	if rejected.Version != plan.Version || rejected.Stage != "planned" {
		t.Fatalf("rejection must carry version and stage, got %+v", rejected)
	}
	if got := fixture.planner.plans[plan.ID].Version; got != plan.Version {
		t.Fatalf("plan must not be mutated after rejection, version=%d", got)
	}
}

func TestReviseRejectsAfterAdmitting(t *testing.T) {
	fixture, plan, _ := newReviseFixture(t, model.StageAdmitting)
	_, err := fixture.planner.Revise(context.Background(), plan.ID, plan.Version, plan.Vessels, "x", fixture.publisher)
	var rejected *RevisionRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("expected RevisionRejectedError, got %v", err)
	}
	if rejected.Stage != "admitting" {
		t.Fatalf("expected admitting stage in reason payload, got %s", rejected.Stage)
	}
}

func TestReviseRejectsVersionConflict(t *testing.T) {
	fixture, plan, _ := newReviseFixture(t, model.StagePlanned)
	_, err := fixture.planner.Revise(context.Background(), plan.ID, plan.Version+99, plan.Vessels, "x", fixture.publisher)
	var rejected *RevisionRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("expected version conflict rejection, got %v", err)
	}
}

func TestReviseRejectsEmptyVessels(t *testing.T) {
	fixture, plan, _ := newReviseFixture(t, model.StagePlanned)
	if _, err := fixture.planner.Revise(context.Background(), plan.ID, plan.Version, nil, "x", fixture.publisher); err == nil {
		t.Fatal("expected error for empty revision")
	}
}

package schedule

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/admission"
	"github.com/wyw14/cry-96/internal/gate"
	"github.com/wyw14/cry-96/internal/journal"
	"github.com/wyw14/cry-96/internal/model"
	"github.com/wyw14/cry-96/internal/signal"
)

type fakeCycles struct {
	current map[string]model.TransitCycle
}

func (f *fakeCycles) Current(chamber string) (model.TransitCycle, bool) {
	c, ok := f.current[chamber]
	return c, ok
}

type harness struct {
	planner   *Planner
	publisher *signal.Publisher
	signals   *signal.Store
	commands  *gate.Commands
	cycles    *fakeCycles
	now       func() time.Time
}

func newHarness(t *testing.T, store journal.Store) harness {
	t.Helper()
	base := time.Date(2026, 9, 29, 8, 0, 0, 0, time.UTC)
	clock := base
	now := func() time.Time { return clock }
	recorder := journal.NewRecorder(store, now)
	signalStore := signal.NewStore()
	gateCommands := gate.NewCommands()
	gateState := gate.NewState("chamber-1")
	queue := admission.NewQueue()
	controller := admission.NewController(queue, gateState, signalStore, now)
	admissionService := admission.NewService(controller, recorder, now)
	planner := NewPlanner(admissionService, recorder, now, signalStore, gateCommands)
	fake := &fakeCycles{current: map[string]model.TransitCycle{}}
	planner.BindCycleLookup(fake)
	publisher := signal.NewPublisher(signalStore, recorder, now)
	return harness{planner: planner, publisher: publisher, signals: signalStore, commands: gateCommands, cycles: fake, now: now}
}

func mustVessels(t *testing.T, names ...string) []model.Vessel {
	t.Helper()
	result := make([]model.Vessel, 0, len(names))
	for _, name := range names {
		vessel, err := model.NewVessel(name, "CS-"+name, 50)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, vessel)
	}
	return result
}

func boundCycle(t *testing.T, plan model.ConvoyPlan, stage model.TransitStage, generation uint64) model.TransitCycle {
	t.Helper()
	cycleValue, err := model.NewTransitCycle(plan.ChamberID, plan.ID, plan.Direction, generation, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	order := []model.TransitStage{
		model.StageAdmitting, model.StageSealed, model.StageLeveling,
		model.StageReleasing, model.StageCompleted,
	}
	for _, next := range order {
		if cycleValue.Stage == stage {
			break
		}
		if err := cycleValue.Advance(next, "test", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if cycleValue.Stage != stage {
		t.Fatalf("could not reach stage %s (ended at %s)", stage, cycleValue.Stage)
	}
	return cycleValue
}

func queueNames(h harness, chamber string) []string {
	entries := h.planner.admission.List(chamber)
	result := make([]string, 0, len(entries))
	for _, entry := range entries {
		result = append(result, entry.Vessel.Name)
	}
	return result
}

func TestReviseReorderAddRemove(t *testing.T) {
	h := newHarness(t, journal.NewMemoryStore())
	ctx := context.Background()
	plan, err := h.planner.Create(ctx, "chamber-1", model.DirectionDownstream, mustVessels(t, "a", "b", "c"))
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]uuid.UUID{}
	for _, v := range plan.Vessels {
		byName[v.Name] = v.ID
	}

	items := []RevisionItem{
		{VesselID: byName["c"]},
		{VesselID: byName["a"]},
		{Name: "d", CallSign: "CS-d", LengthM: 40},
	}
	// A non-proceed draft signal on the removed vessel must be cleaned up.
	h.signals.Set(byName["b"], uuid.New(), 1, model.SignalHold, h.now())

	result, revErr := h.planner.Revise(ctx, plan.ID, 1, items, "operator fix", h.publisher)
	if revErr != nil {
		t.Fatalf("revise: %v", revErr)
	}
	if result.Version != 2 || result.Added != 1 || result.Removed != 1 || !result.Reordered {
		t.Fatalf("unexpected summary %+v", result)
	}
	if got := queueNames(h, plan.ChamberID); len(got) != 3 || got[0] != "c" || got[1] != "a" || got[2] != "d" {
		t.Fatalf("queue not synced: %v", got)
	}
	if _, ok := h.signals.Get(byName["b"]); ok {
		t.Fatal("unpublished signal of removed vessel was not dropped")
	}
	if history := h.planner.History(plan.ID); len(history) != 2 || history[1].Version != 2 || history[1].Note != "operator fix" {
		t.Fatalf("unexpected history %+v", history)
	}

	// Stale expected version is a conflict.
	if _, revErr = h.planner.Revise(ctx, plan.ID, 1, items, "again", h.publisher); revErr == nil || revErr.Code != RevisionConflict || revErr.CurrentVersion != 2 {
		t.Fatalf("expected version conflict, got %+v", revErr)
	}
}

func TestReviseValidation(t *testing.T) {
	h := newHarness(t, journal.NewMemoryStore())
	ctx := context.Background()
	plan, err := h.planner.Create(ctx, "chamber-1", model.DirectionDownstream, mustVessels(t, "a", "b"))
	if err != nil {
		t.Fatal(err)
	}
	keep := []RevisionItem{{VesselID: plan.Vessels[0].ID}, {VesselID: plan.Vessels[1].ID}}
	if _, revErr := h.planner.Revise(ctx, plan.ID, 1, keep, "", h.publisher); revErr == nil || revErr.Code != RevisionInvalid {
		t.Fatalf("identical sequence must be invalid, got %+v", revErr)
	}
	if _, revErr := h.planner.Revise(ctx, plan.ID, 1, nil, "", h.publisher); revErr == nil || revErr.Code != RevisionInvalid {
		t.Fatalf("empty sequence must be invalid, got %+v", revErr)
	}
	if _, revErr := h.planner.Revise(ctx, plan.ID, 1, []RevisionItem{{VesselID: uuid.New()}}, "", h.publisher); revErr == nil || revErr.Code != RevisionInvalid {
		t.Fatalf("foreign vessel must be invalid, got %+v", revErr)
	}
	if _, revErr := h.planner.Revise(ctx, uuid.New(), 1, keep, "", h.publisher); revErr == nil || revErr.Code != RevisionNotFound {
		t.Fatalf("missing plan must be not_found, got %+v", revErr)
	}
}

func TestReviseRejectedAfterRelease(t *testing.T) {
	h := newHarness(t, journal.NewMemoryStore())
	ctx := context.Background()
	plan, err := h.planner.Create(ctx, "chamber-1", model.DirectionDownstream, mustVessels(t, "a", "b"))
	if err != nil {
		t.Fatal(err)
	}
	cycleValue := boundCycle(t, plan, model.StagePlanned, 1)
	h.cycles.current[plan.ChamberID] = cycleValue
	if _, err := h.planner.Publish(ctx, plan.ID, cycleValue, h.publisher); err != nil {
		t.Fatal(err)
	}
	items := []RevisionItem{{VesselID: plan.Vessels[1].ID}, {VesselID: plan.Vessels[0].ID}}
	revErr := mustReviseErr(t, h, plan, items)
	if revErr.Code != RevisionNotEditable {
		t.Fatalf("expected not_editable, got %+v", revErr)
	}
}

func TestReviseRejectedByTransitState(t *testing.T) {
	cases := []struct {
		name   string
		stage  model.TransitStage
		reason string
	}{
		{"admitted", model.StageAdmitting, "admitted"},
		{"finished", model.StageCompleted, "finished"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, journal.NewMemoryStore())
			ctx := context.Background()
			plan, err := h.planner.Create(ctx, "chamber-1", model.DirectionDownstream, mustVessels(t, "a", "b"))
			if err != nil {
				t.Fatal(err)
			}
			h.cycles.current[plan.ChamberID] = boundCycle(t, plan, tc.stage, 1)
			revErr := mustReviseErr(t, h, plan, []RevisionItem{{VesselID: plan.Vessels[1].ID}, {VesselID: plan.Vessels[0].ID}})
			if revErr.Code != RevisionNotEditable {
				t.Fatalf("expected not_editable, got %+v", revErr)
			}
		})
	}
}

func TestReviseRejectedByDeviceCommand(t *testing.T) {
	h := newHarness(t, journal.NewMemoryStore())
	ctx := context.Background()
	plan, err := h.planner.Create(ctx, "chamber-1", model.DirectionDownstream, mustVessels(t, "a", "b"))
	if err != nil {
		t.Fatal(err)
	}
	cycleValue := boundCycle(t, plan, model.StagePlanned, 1)
	h.cycles.current[plan.ChamberID] = cycleValue
	if _, err := h.commands.Issue(cycleValue, model.DeviceUpstreamGate, "open", 0, h.now()); err != nil {
		t.Fatal(err)
	}
	revErr := mustReviseErr(t, h, plan, []RevisionItem{{VesselID: plan.Vessels[1].ID}, {VesselID: plan.Vessels[0].ID}})
	if revErr.Code != RevisionNotEditable || revErr.Reason != "device command already started" {
		t.Fatalf("expected device command rejection, got %+v", revErr)
	}
}

func TestReviseWaitingPlanAllowed(t *testing.T) {
	h := newHarness(t, journal.NewMemoryStore())
	ctx := context.Background()
	plan, err := h.planner.Create(ctx, "chamber-1", model.DirectionDownstream, mustVessels(t, "a", "b"))
	if err != nil {
		t.Fatal(err)
	}
	// Chamber is busy with a transit that belongs to a different plan.
	other, err := h.planner.Create(ctx, "chamber-1", model.DirectionDownstream, mustVessels(t, "z"))
	if err != nil {
		t.Fatal(err)
	}
	h.cycles.current[plan.ChamberID] = boundCycle(t, other, model.StageLeveling, 1)
	items := []RevisionItem{{VesselID: plan.Vessels[1].ID}, {VesselID: plan.Vessels[0].ID}}
	if _, revErr := h.planner.Revise(ctx, plan.ID, 1, items, "", h.publisher); revErr != nil {
		t.Fatalf("waiting plan should be editable, got %+v", revErr)
	}
}

type failOnReviseStore struct {
	*journal.MemoryStore
}

func (s *failOnReviseStore) Append(ctx context.Context, event model.Event) (model.Event, error) {
	if event.Type == model.EventPlanRevised {
		return model.Event{}, context.DeadlineExceeded
	}
	return s.MemoryStore.Append(ctx, event)
}

func TestReviseJournalFailureRollsBack(t *testing.T) {
	h := newHarness(t, &failOnReviseStore{MemoryStore: journal.NewMemoryStore()})
	ctx := context.Background()
	plan, err := h.planner.Create(ctx, "chamber-1", model.DirectionDownstream, mustVessels(t, "a", "b", "c"))
	if err != nil {
		t.Fatal(err)
	}
	removedID := plan.Vessels[1].ID
	h.signals.Set(removedID, uuid.New(), 1, model.SignalHold, h.now())
	items := []RevisionItem{
		{VesselID: plan.Vessels[2].ID},
		{VesselID: plan.Vessels[0].ID},
		{Name: "d", CallSign: "CS-d", LengthM: 30},
	}
	if _, revErr := h.planner.Revise(ctx, plan.ID, 1, items, "", h.publisher); revErr == nil {
		t.Fatal("expected revision failure")
	}
	stored, ok := h.planner.Get(plan.ID)
	if !ok || stored.Version != 1 || len(stored.Vessels) != 3 {
		t.Fatalf("plan not rolled back: %+v", stored)
	}
	if got := queueNames(h, plan.ChamberID); len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("queue not rolled back: %v", got)
	}
	state, ok := h.signals.Get(removedID)
	if !ok || state.Aspect != model.SignalHold {
		t.Fatalf("dropped signal not restored: %+v ok=%v", state, ok)
	}
}

func mustReviseErr(t *testing.T, h harness, plan model.ConvoyPlan, items []RevisionItem) *RevisionError {
	t.Helper()
	_, revErr := h.planner.Revise(context.Background(), plan.ID, plan.Version, items, "", h.publisher)
	if revErr == nil {
		t.Fatal("expected revision error")
	}
	return revErr
}

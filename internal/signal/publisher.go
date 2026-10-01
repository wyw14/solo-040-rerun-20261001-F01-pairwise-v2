package signal

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/journal"
	"github.com/wyw14/cry-96/internal/model"
)

type Publication struct {
	CycleID    uuid.UUID
	Generation uint64
	Published  []uuid.UUID
	Previous   map[uuid.UUID]model.SignalState
}

// Publisher applies a convoy signal change with a compensatable record.
type Publisher struct {
	store    *Store
	recorder *journal.Recorder
	now      func() time.Time
}

func NewPublisher(store *Store, recorder *journal.Recorder, now func() time.Time) *Publisher {
	if now == nil {
		now = time.Now
	}
	return &Publisher{store: store, recorder: recorder, now: now}
}

func (p *Publisher) Publish(ctx context.Context, cycle model.TransitCycle, vessels []model.Vessel, aspect model.SignalAspect) (Publication, error) {
	publication := Publication{
		CycleID: cycle.ID, Generation: cycle.Generation,
		Previous: make(map[uuid.UUID]model.SignalState),
	}
	for _, vessel := range vessels {
		if err := ctx.Err(); err != nil {
			p.Compensate(context.WithoutCancel(ctx), cycle.ChamberID, publication)
			return publication, err
		}
		if previous, ok := p.store.Get(vessel.ID); ok {
			publication.Previous[vessel.ID] = previous
		}
		state := p.store.Set(vessel.ID, cycle.ID, cycle.Generation, aspect, p.now())
		publication.Published = append(publication.Published, vessel.ID)
		if _, err := p.recorder.Record(ctx, cycle.ChamberID, model.EventSignalChanged, cycle.ID, cycle.Generation, state); err != nil {
			p.Compensate(context.WithoutCancel(ctx), cycle.ChamberID, publication)
			return publication, err
		}
	}
	return publication, nil
}

func (p *Publisher) Compensate(ctx context.Context, chamberID string, publication Publication) error {
	var combined error
	for index := len(publication.Published) - 1; index >= 0; index-- {
		vesselID := publication.Published[index]
		if previous, ok := publication.Previous[vesselID]; ok {
			p.store.Set(previous.VesselID, previous.CycleID, previous.Generation, previous.Aspect, p.now())
		} else {
			p.store.Remove(vesselID)
		}
		_, err := p.recorder.Record(ctx, chamberID, model.EventSignalChanged, publication.CycleID, publication.Generation, map[string]any{
			"vessel_id": vesselID, "compensated": true,
		})
		combined = errors.Join(combined, err)
	}
	return combined
}

// SignalRevision records which vessel signals were withdrawn by a plan
// revision, together with their previous aspects for rollback.
type SignalRevision struct {
	CycleID    uuid.UUID
	Generation uint64
	Withdrawn  []uuid.UUID
	Previous   map[uuid.UUID]model.SignalState
}

// ReviseSignals withdraws the provisional pre-admission signals that a plan's
// vessels currently hold for the live transit generation. Because the vessel
// order (and therefore which queue entry is entitled to "proceed") may
// change, all such signals are cleared; the caller re-evaluates admission
// afterwards to grant the new queue head. Signals belonging to another
// transit generation are untouched.
func (p *Publisher) ReviseSignals(ctx context.Context, chamberID string, cycleID uuid.UUID, generation uint64, oldVessels []model.Vessel) (SignalRevision, error) {
	revision := SignalRevision{
		CycleID: cycleID, Generation: generation,
		Previous: make(map[uuid.UUID]model.SignalState),
	}
	for _, vessel := range oldVessels {
		state, ok := p.store.Get(vessel.ID)
		if !ok || state.CycleID != cycleID || state.Generation != generation {
			continue
		}
		revision.Previous[vessel.ID] = state
		p.store.Remove(vessel.ID)
		revision.Withdrawn = append(revision.Withdrawn, vessel.ID)
		if _, err := p.recorder.Record(ctx, chamberID, model.EventSignalChanged, cycleID, generation, map[string]any{
			"vessel_id": vessel.ID, "revised": true, "withdrawn": true,
		}); err != nil {
			p.CompensateRevision(context.WithoutCancel(ctx), chamberID, revision)
			return SignalRevision{}, err
		}
	}
	return revision, nil
}

// CompensateRevision restores signals withdrawn by ReviseSignals.
func (p *Publisher) CompensateRevision(ctx context.Context, chamberID string, revision SignalRevision) error {
	var combined error
	for index := len(revision.Withdrawn) - 1; index >= 0; index-- {
		vesselID := revision.Withdrawn[index]
		previous, ok := revision.Previous[vesselID]
		if !ok {
			continue
		}
		p.store.Set(previous.VesselID, previous.CycleID, previous.Generation, previous.Aspect, p.now())
		_, err := p.recorder.Record(ctx, chamberID, model.EventSignalChanged, revision.CycleID, revision.Generation, map[string]any{
			"vessel_id": vesselID, "compensated": true, "revised": true,
		})
		combined = errors.Join(combined, err)
	}
	return combined
}

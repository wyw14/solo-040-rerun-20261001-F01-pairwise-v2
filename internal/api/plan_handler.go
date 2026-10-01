package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/model"
	"github.com/wyw14/cry-96/internal/schedule"
	"github.com/wyw14/cry-96/internal/signal"
)

type vesselInput struct {
	Name     string  `json:"name"`
	CallSign string  `json:"call_sign"`
	LengthM  float64 `json:"length_m"`
}

type planInput struct {
	ChamberID string          `json:"chamber_id"`
	Direction model.Direction `json:"direction"`
	Vessels   []vesselInput   `json:"vessels"`
}

func createPlanHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input planInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		vessels := make([]model.Vessel, 0, len(input.Vessels))
		for _, item := range input.Vessels {
			vessel, err := model.NewVessel(item.Name, item.CallSign, item.LengthM)
			if err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			vessels = append(vessels, vessel)
		}
		plan, err := runtime.Schedule.Create(r.Context(), input.ChamberID, input.Direction, vessels)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		// Creation is decoupled from release: register a planned transit only
		// when the chamber is idle. If another transit is active the plan waits
		// in the queue and succeeds without a cycle.
		current, hasCycle := runtime.Cycles.Current(input.ChamberID)
		if hasCycle && !current.Stage.Terminal() {
			writeJSON(w, http.StatusCreated, map[string]any{"plan": plan, "queued": true})
			return
		}
		cycleValue, cycleErr := runtime.CycleService.Plan(r.Context(), plan)
		if cycleErr != nil {
			writeJSON(w, http.StatusCreated, map[string]any{"plan": plan, "queued": true, "warning": cycleErr.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"plan": plan, "cycle": cycleValue})
	}
}

func plansHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		result := make(map[string]any, len(runtime.Chambers))
		for _, chamberID := range runtime.Chambers {
			result[chamberID] = runtime.Schedule.ActiveViews(chamberID)
		}
		writeJSON(w, http.StatusOK, result)
	}
}

func getPlanHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		planID, err := uuid.Parse(chi.URLParam(r, "planID"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		status, ok := runtime.Schedule.Inspect(planID)
		if !ok {
			writeError(w, http.StatusNotFound, errors.New("plan not found"))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status": status, "plan": status.Plan, "history": runtime.Schedule.History(planID),
		})
	}
}

type reviseVesselInput struct {
	VesselID string  `json:"vessel_id"`
	Name     string  `json:"name"`
	CallSign string  `json:"call_sign"`
	LengthM  float64 `json:"length_m"`
}

type reviseInput struct {
	ExpectedVersion uint32              `json:"expected_version"`
	Note            string              `json:"note"`
	Vessels         []reviseVesselInput `json:"vessels"`
}

func revisePlanHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		planID, err := uuid.Parse(chi.URLParam(r, "planID"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		var input reviseInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		items := make([]schedule.RevisionItem, 0, len(input.Vessels))
		for index, vessel := range input.Vessels {
			if vessel.VesselID == "" || vessel.VesselID == uuid.Nil.String() {
				items = append(items, schedule.RevisionItem{
					Name: vessel.Name, CallSign: vessel.CallSign, LengthM: vessel.LengthM,
				})
				continue
			}
			vesselID, parseErr := uuid.Parse(vessel.VesselID)
			if parseErr != nil {
				writeError(w, http.StatusBadRequest, errors.New("invalid vessel_id at position "+strconv.Itoa(index)))
				return
			}
			if vesselID == uuid.Nil {
				items = append(items, schedule.RevisionItem{
					Name: vessel.Name, CallSign: vessel.CallSign, LengthM: vessel.LengthM,
				})
				continue
			}
			items = append(items, schedule.RevisionItem{VesselID: vesselID})
		}
		result, revisionErr := runtime.Schedule.Revise(r.Context(), planID, input.ExpectedVersion, items, input.Note)
		if revisionErr != nil {
			writeRevisionError(w, revisionErr)
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

func writeRevisionError(w http.ResponseWriter, revisionErr *schedule.RevisionError) {
	status := http.StatusConflict
	switch revisionErr.Code {
	case schedule.RevisionNotFound:
		status = http.StatusNotFound
	case schedule.RevisionInvalid:
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]any{
		"error":           revisionErr.Reason,
		"code":            revisionErr.Code,
		"current_version": revisionErr.CurrentVersion,
		"plan_id":         revisionErr.PlanID,
	})
}

func publishSignalsHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		planID, err := uuid.Parse(chi.URLParam(r, "planID"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		plan, ok := runtime.Planner.Get(planID)
		if !ok {
			writeError(w, http.StatusNotFound, errors.New("plan not found"))
			return
		}
		current, hasCycle := runtime.Cycles.Current(plan.ChamberID)
		if hasCycle && !current.Stage.Terminal() {
			if current.PlanID != plan.ID {
				writeError(w, http.StatusConflict, errors.New("plan is waiting on an active transit"))
				return
			}
		} else {
			// Chamber is idle or its last transit is terminal: arm this plan
			// before publishing the release signals.
			cycleValue, cycleErr := runtime.CycleService.Plan(r.Context(), plan)
			if cycleErr != nil {
				writeError(w, http.StatusConflict, cycleErr)
				return
			}
			current = cycleValue
		}
		publication, err := runtime.Schedule.Publish(r.Context(), planID, current)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusAccepted, publication)
	}
}

func cancelPlanHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		planID, err := uuid.Parse(chi.URLParam(r, "planID"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		plan, ok := runtime.Planner.Get(planID)
		if !ok {
			writeError(w, http.StatusNotFound, errors.New("plan not found"))
			return
		}
		cycleValue, bound := runtime.Cycles.Current(plan.ChamberID)
		publication := signal.Publication{Previous: make(map[uuid.UUID]model.SignalState)}
		if bound && cycleValue.PlanID == plan.ID {
			publication.CycleID = cycleValue.ID
			publication.Generation = cycleValue.Generation
			for _, vessel := range plan.Vessels {
				if _, exists := runtime.SignalStore.Get(vessel.ID); exists {
					publication.Published = append(publication.Published, vessel.ID)
				}
			}
		}
		result, err := runtime.Schedule.Cancel(r.Context(), planID, "operator cancellation", publication)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		// Only cancel the transit that belongs to this plan; a waiting plan
		// must not cancel another plan's active transit.
		if bound && cycleValue.PlanID == plan.ID {
			_, _ = runtime.Cycles.Cancel(r.Context(), plan.ChamberID, "operator cancellation")
		}
		writeJSON(w, http.StatusOK, result)
	}
}

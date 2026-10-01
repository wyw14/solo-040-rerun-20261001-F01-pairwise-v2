package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/model"
	"github.com/wyw14/cry-96/internal/schedule"
	"github.com/wyw14/cry-96/internal/signal"
)

type vesselInput struct {
	ID       string  `json:"id,omitempty"`
	Name     string  `json:"name"`
	CallSign string  `json:"call_sign"`
	LengthM  float64 `json:"length_m"`
}

type planInput struct {
	ChamberID string          `json:"chamber_id"`
	Direction model.Direction `json:"direction"`
	Vessels   []vesselInput   `json:"vessels"`
}

type planRevisionInput struct {
	ExpectedVersion uint64        `json:"expected_version"`
	Note            string        `json:"note"`
	Vessels         []vesselInput `json:"vessels"`
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
		cycleValue, err := runtime.CycleService.Start(r.Context(), plan)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"plan": plan, "cycle": cycleValue})
	}
}

func plansHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		result := make(map[string]any, len(runtime.Chambers))
		for _, chamberID := range runtime.Chambers {
			result[chamberID] = runtime.Schedule.Active(chamberID)
		}
		writeJSON(w, http.StatusOK, result)
	}
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
		cycleValue, ok := runtime.Cycles.Current(plan.ChamberID)
		if !ok {
			writeError(w, http.StatusConflict, errors.New("active transit not found"))
			return
		}
		publication, err := runtime.Schedule.Publish(r.Context(), planID, cycleValue)
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
		cycleValue, cycleOK := runtime.Cycles.Current(plan.ChamberID)
		publication := signal.Publication{
			CycleID: cycleValue.ID, Generation: cycleValue.Generation,
			Previous: make(map[uuid.UUID]model.SignalState),
		}
		if cycleOK {
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
		if cycleOK {
			_, _ = runtime.Cycles.Cancel(r.Context(), plan.ChamberID, "operator cancellation")
		}
		writeJSON(w, http.StatusOK, result)
	}
}

// revisePlanHandler rewrites a not-yet-admitted plan. Existing vessels may be
// referenced by id for reordering/removal; entries without id are added.
func revisePlanHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		planID, err := uuid.Parse(chi.URLParam(r, "planID"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		var input planRevisionInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		plan, ok := runtime.Planner.Get(planID)
		if !ok {
			writeError(w, http.StatusNotFound, errors.New("plan not found"))
			return
		}
		existing := make(map[uuid.UUID]model.Vessel, len(plan.Vessels))
		for _, vessel := range plan.Vessels {
			existing[vessel.ID] = vessel
		}
		vessels := make([]model.Vessel, 0, len(input.Vessels))
		seen := make(map[uuid.UUID]struct{}, len(input.Vessels))
		for _, item := range input.Vessels {
			if item.ID != "" {
				vesselID, parseErr := uuid.Parse(item.ID)
				if parseErr != nil {
					writeError(w, http.StatusBadRequest, errors.New("invalid vessel id: "+item.ID))
					return
				}
				vessel, found := existing[vesselID]
				if !found {
					writeError(w, http.StatusBadRequest, errors.New("vessel "+vesselID.String()+" is not part of plan "+planID.String()))
					return
				}
				if _, dup := seen[vesselID]; dup {
					writeError(w, http.StatusBadRequest, errors.New("duplicate vessel in revision: "+vesselID.String()))
					return
				}
				seen[vesselID] = struct{}{}
				vessels = append(vessels, vessel)
				continue
			}
			vessel, buildErr := model.NewVessel(item.Name, item.CallSign, item.LengthM)
			if buildErr != nil {
				writeError(w, http.StatusBadRequest, buildErr)
				return
			}
			if _, dup := seen[vessel.ID]; dup {
				writeError(w, http.StatusBadRequest, errors.New("duplicate vessel in revision"))
				return
			}
			seen[vessel.ID] = struct{}{}
			vessels = append(vessels, vessel)
		}
		result, err := runtime.Schedule.Revise(r.Context(), planID, input.ExpectedVersion, vessels, input.Note)
		if err != nil {
			var rejected *schedule.RevisionRejectedError
			if errors.As(err, &rejected) {
				writeJSON(w, http.StatusConflict, map[string]any{
					"error":    rejected.Reason,
					"plan_id":  rejected.PlanID,
					"version":  rejected.Version,
					"stage":    rejected.Stage,
					"reason":   rejected.Reason,
					"rejected": true,
				})
				return
			}
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

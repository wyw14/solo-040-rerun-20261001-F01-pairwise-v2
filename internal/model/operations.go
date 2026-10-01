package model

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

type Vessel struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	CallSign string    `json:"call_sign"`
	LengthM  float64   `json:"length_m"`
}

func NewVessel(name, callSign string, lengthM float64) (Vessel, error) {
	if name == "" || callSign == "" || lengthM <= 0 {
		return Vessel{}, errors.New("vessel name, call sign and positive length are required")
	}
	return Vessel{ID: uuid.New(), Name: name, CallSign: callSign, LengthM: lengthM}, nil
}

type ConvoyPlan struct {
	ID         uuid.UUID      `json:"id"`
	ChamberID  string         `json:"chamber_id"`
	Direction  Direction      `json:"direction"`
	Vessels    []Vessel       `json:"vessels"`
	Version    uint64         `json:"version"`
	History    []PlanRevision `json:"history,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
	Cancelled  bool           `json:"cancelled"`
	CancelNote string         `json:"cancel_note,omitempty"`
}

func NewConvoyPlan(chamberID string, direction Direction, vessels []Vessel, now time.Time) (ConvoyPlan, error) {
	if chamberID == "" || !direction.Valid() || len(vessels) == 0 {
		return ConvoyPlan{}, errors.New("plan requires chamber, direction and vessels")
	}
	copyOfVessels := append([]Vessel(nil), vessels...)
	timestamp := now.UTC()
	return ConvoyPlan{
		ID: uuid.New(), ChamberID: chamberID, Direction: direction,
		Vessels: copyOfVessels, Version: 1,
		CreatedAt: timestamp, UpdatedAt: timestamp,
	}, nil
}

func (p ConvoyPlan) Clone() ConvoyPlan {
	p.Vessels = append([]Vessel(nil), p.Vessels...)
	if p.History != nil {
		history := make([]PlanRevision, len(p.History))
		for index, revision := range p.History {
			history[index] = revision.Clone()
		}
		p.History = history
	}
	return p
}

// PlanRevision is an immutable snapshot of one published plan version.
type PlanRevision struct {
	Version   uint64    `json:"version"`
	Vessels   []Vessel  `json:"vessels"`
	RevisedAt time.Time `json:"revised_at"`
	Note      string    `json:"note,omitempty"`
}

func NewPlanRevision(version uint64, vessels []Vessel, revisedAt time.Time, note string) (PlanRevision, error) {
	if version == 0 {
		return PlanRevision{}, errors.New("revision version must be positive")
	}
	if len(vessels) == 0 {
		return PlanRevision{}, errors.New("revision requires at least one vessel")
	}
	return PlanRevision{
		Version: version, Vessels: append([]Vessel(nil), vessels...),
		RevisedAt: revisedAt.UTC(), Note: note,
	}, nil
}

func (r PlanRevision) Clone() PlanRevision {
	r.Vessels = append([]Vessel(nil), r.Vessels...)
	return r
}

type Lease struct {
	VesselID     uuid.UUID `json:"vessel_id"`
	Owner        string    `json:"owner"`
	FencingToken uint64    `json:"fencing_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func (l Lease) Active(now time.Time) bool {
	return l.Owner != "" && l.FencingToken > 0 && now.Before(l.ExpiresAt)
}

type SignalAspect string

const (
	SignalStop    SignalAspect = "stop"
	SignalProceed SignalAspect = "proceed"
	SignalHold    SignalAspect = "hold"
)

type SignalState struct {
	VesselID   uuid.UUID    `json:"vessel_id"`
	CycleID    uuid.UUID    `json:"cycle_id"`
	Generation uint64       `json:"generation"`
	Aspect     SignalAspect `json:"aspect"`
	UpdatedAt  time.Time    `json:"updated_at"`
}

type EmergencyStatus string

const (
	EmergencyIdle      EmergencyStatus = "idle"
	EmergencyDraining  EmergencyStatus = "draining"
	EmergencyCompleted EmergencyStatus = "completed"
	EmergencyFailed    EmergencyStatus = "failed"
)

type EmergencyRecord struct {
	ID          uuid.UUID       `json:"id"`
	ChamberID   string          `json:"chamber_id"`
	Status      EmergencyStatus `json:"status"`
	RequestedAt time.Time       `json:"requested_at"`
	CompletedAt *time.Time      `json:"completed_at,omitempty"`
	Message     string          `json:"message,omitempty"`
}

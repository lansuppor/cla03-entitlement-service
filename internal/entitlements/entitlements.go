// Package entitlements defines the team service entitlement domain: the
// allocation, reservation and consumption (settlement) lifecycle of seat and
// credit quotas, together with the stable error codes shared by the storage
// layer and the HTTP layer.
package entitlements

import (
	"context"
	"fmt"
	"time"
)

// Entitlement lifecycle status derived from the effective window.
type Status string

const (
	// StatusPending means now is before EffectiveFrom.
	StatusPending Status = "pending"
	// StatusActive means EffectiveFrom <= now < EffectiveTo.
	StatusActive Status = "active"
	// StatusExpired means now >= EffectiveTo.
	StatusExpired Status = "expired"
)

// ReservationStatus is the settlement state of a single reservation.
type ReservationStatus string

const (
	ReservationReserved  ReservationStatus = "reserved"
	ReservationConfirmed ReservationStatus = "confirmed"
	ReservationReleased  ReservationStatus = "released"
)

// maxIdentifierLength bounds entitlement and business reservation identifiers.
const maxIdentifierLength = 128

// EntitlementInput carries the parameters supplied when an entitlement is
// created. All fields are caller provided and validated before any data is
// written.
type EntitlementInput struct {
	ID            string
	SeatTotal     int64
	CreditTotal   int64
	EffectiveFrom time.Time
	EffectiveTo   time.Time
}

// Validate checks creation parameters independently of the transport. It
// returns an invalid-request error describing the first offending field.
func (in EntitlementInput) Validate() error {
	if err := validateIdentifier("entitlement_id", in.ID); err != nil {
		return err
	}
	if in.SeatTotal < 0 {
		return InvalidRequest("seat_total must be greater than or equal to 0")
	}
	if in.CreditTotal < 0 {
		return InvalidRequest("credit_total must be greater than or equal to 0")
	}
	if in.EffectiveFrom.IsZero() {
		return InvalidRequest("effective_from is required and must be an RFC3339 timestamp")
	}
	if in.EffectiveTo.IsZero() {
		return InvalidRequest("effective_to is required and must be an RFC3339 timestamp")
	}
	if !in.EffectiveFrom.Before(in.EffectiveTo) {
		return InvalidRequest("effective_from must be earlier than effective_to")
	}
	return nil
}

// ReservationInput carries a reservation request. The business reservation
// identifier is unique within one entitlement and is the idempotency key.
type ReservationInput struct {
	ID     string
	Credit int64
}

// Validate checks reservation parameters. Credits must be positive.
func (in ReservationInput) Validate() error {
	if err := validateIdentifier("reservation_id", in.ID); err != nil {
		return err
	}
	if in.Credit <= 0 {
		return InvalidRequest("credit must be greater than 0")
	}
	return nil
}

func validateIdentifier(field, value string) error {
	if value == "" {
		return InvalidRequest(field + " is required")
	}
	if len(value) > maxIdentifierLength {
		return InvalidRequest(fmt.Sprintf("%s must be at most %d characters", field, maxIdentifierLength))
	}
	return nil
}

// ReservationView is one reservation as returned by mutating calls and in the
// entitlement view. Result is nil while the reservation is unsettled and
// carries the settlement outcome afterwards.
type ReservationView struct {
	ReservationID string             `json:"reservation_id"`
	Credit        int64              `json:"credit"`
	Status        ReservationStatus  `json:"status"`
	Result        *ReservationStatus `json:"result"`
}

// View is the consistent, point-in-time projection of one entitlement.
type View struct {
	EntitlementID   string            `json:"entitlement_id"`
	SeatTotal       int64             `json:"seat_total"`
	CreditTotal     int64             `json:"credit_total"`
	CreditUsed      int64             `json:"credit_used"`
	CreditReserved  int64             `json:"credit_reserved"`
	CreditAvailable int64             `json:"credit_available"`
	Status          Status            `json:"status"`
	EffectiveFrom   time.Time         `json:"effective_from"`
	EffectiveTo     time.Time         `json:"effective_to"`
	Reservations    []ReservationView `json:"reservations"`
}

// StatusAt derives the lifecycle status for an effective window at time now.
func StatusAt(effectiveFrom, effectiveTo, now time.Time) Status {
	if now.Before(effectiveFrom) {
		return StatusPending
	}
	if !now.Before(effectiveTo) {
		return StatusExpired
	}
	return StatusActive
}

// Service is the entitlement use-case boundary. Every method is atomic:
// failures leave no partial state behind.
type Service interface {
	// CreateEntitlement validates and persists a new entitlement.
	CreateEntitlement(ctx context.Context, in EntitlementInput) (*View, error)
	// EntitlementView returns a consistent projection, including all
	// reservations and derived totals.
	EntitlementView(ctx context.Context, entitlementID string) (*View, error)
	// Reserve creates a reservation or replays the original result when the
	// same business reservation identifier is submitted again with identical
	// parameters.
	Reserve(ctx context.Context, entitlementID string, in ReservationInput) (*ReservationView, error)
	// Confirm settles a reservation: reserved credit becomes used credit.
	Confirm(ctx context.Context, entitlementID, reservationID string) (*ReservationView, error)
	// Release settles a reservation: reserved credit returns to available.
	Release(ctx context.Context, entitlementID, reservationID string) (*ReservationView, error)
}

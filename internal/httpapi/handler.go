package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/lansolocoder/cla03-entitlement-service/internal/entitlements"
)

// Service is the entitlement domain surface the HTTP layer needs.
type Service interface {
	CreateEntitlement(ctx context.Context, in entitlements.CreateEntitlementInput) (entitlements.Entitlement, error)
	CreateReservation(ctx context.Context, entitlementID, reservationID string, amount int64) (entitlements.Reservation, bool, error)
	SettleReservation(ctx context.Context, entitlementID, reservationID, action string) (entitlements.Reservation, error)
	ReverseReservation(ctx context.Context, entitlementID, reservationID, reversalID string) (entitlements.Reversal, bool, error)
	CreateAdjustment(ctx context.Context, entitlementID, adjustmentID string, delta int64, effectiveAt time.Time) (entitlements.Adjustment, bool, error)
	RescheduleEntitlement(ctx context.Context, entitlementID, rescheduleID string, validFrom, validTo time.Time) (entitlements.Reschedule, bool, error)
	GetView(ctx context.Context, entitlementID string) (entitlements.View, error)
}

func New(checkDatabase func(context.Context) error, service Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		respond(w, http.StatusOK, `{"status":"ok"}`)
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if err := checkDatabase(ctx); err != nil {
			respond(w, http.StatusServiceUnavailable, `{"status":"unavailable"}`)
			return
		}
		respond(w, http.StatusOK, `{"status":"ready"}`)
	})

	h := &handlers{service: service}
	mux.HandleFunc("POST /entitlements", h.createEntitlement)
	mux.HandleFunc("GET /entitlements/{entitlementID}", h.getEntitlement)
	mux.HandleFunc("POST /entitlements/{entitlementID}/reservations", h.createReservation)
	mux.HandleFunc("POST /entitlements/{entitlementID}/reservations/{reservationID}/confirm", h.settle(entitlements.ActionConfirm))
	mux.HandleFunc("POST /entitlements/{entitlementID}/reservations/{reservationID}/release", h.settle(entitlements.ActionRelease))
	mux.HandleFunc("POST /entitlements/{entitlementID}/reservations/{reservationID}/reverse", h.reverseReservation)
	mux.HandleFunc("POST /entitlements/{entitlementID}/adjustments", h.createAdjustment)
	mux.HandleFunc("POST /entitlements/{entitlementID}/reschedules", h.rescheduleEntitlement)
	return mux
}

type handlers struct {
	service Service
}

type createEntitlementRequest struct {
	EntitlementID string `json:"entitlement_id"`
	SeatsTotal    *int64 `json:"seats_total"`
	QuotaTotal    *int64 `json:"quota_total"`
	ValidFrom     string `json:"valid_from"`
	ValidTo       string `json:"valid_to"`
}

func (h *handlers) createEntitlement(w http.ResponseWriter, r *http.Request) {
	var req createEntitlementRequest
	if !decode(w, r, &req) {
		return
	}
	validFrom, errFrom := time.Parse(time.RFC3339, req.ValidFrom)
	validTo, errTo := time.Parse(time.RFC3339, req.ValidTo)
	if req.ValidFrom == "" || req.ValidTo == "" || errFrom != nil || errTo != nil {
		respondError(w, &entitlements.Error{
			Code:    entitlements.CodeInvalidRequest,
			Message: "valid_from and valid_to must be RFC 3339 timestamps, e.g. 2026-01-01T00:00:00Z",
		})
		return
	}
	ent, err := h.service.CreateEntitlement(r.Context(), entitlements.CreateEntitlementInput{
		EntitlementID: req.EntitlementID,
		SeatsTotal:    req.SeatsTotal,
		QuotaTotal:    req.QuotaTotal,
		ValidFrom:     validFrom,
		ValidTo:       validTo,
	})
	if err != nil {
		respondError(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, ent)
}

type createReservationRequest struct {
	ReservationID string `json:"reservation_id"`
	Amount        *int64 `json:"amount"`
}

func (h *handlers) createReservation(w http.ResponseWriter, r *http.Request) {
	var req createReservationRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Amount == nil {
		respondError(w, &entitlements.Error{
			Code:    entitlements.CodeInvalidRequest,
			Message: "amount is required and must be a positive integer",
		})
		return
	}
	reservation, created, err := h.service.CreateReservation(
		r.Context(), r.PathValue("entitlementID"), req.ReservationID, *req.Amount)
	if err != nil {
		respondError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	respondJSON(w, status, reservation)
}

func (h *handlers) settle(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reservation, err := h.service.SettleReservation(
			r.Context(), r.PathValue("entitlementID"), r.PathValue("reservationID"), action)
		if err != nil {
			respondError(w, err)
			return
		}
		respondJSON(w, http.StatusOK, reservation)
	}
}

type reverseReservationRequest struct {
	ReversalID string `json:"reversal_id"`
}

func (h *handlers) reverseReservation(w http.ResponseWriter, r *http.Request) {
	var req reverseReservationRequest
	if !decode(w, r, &req) {
		return
	}
	reversal, created, err := h.service.ReverseReservation(
		r.Context(), r.PathValue("entitlementID"), r.PathValue("reservationID"), req.ReversalID)
	if err != nil {
		respondError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	respondJSON(w, status, reversal)
}

type createAdjustmentRequest struct {
	AdjustmentID string `json:"adjustment_id"`
	Delta        *int64 `json:"delta"`
	EffectiveAt  string `json:"effective_at"`
}

func (h *handlers) createAdjustment(w http.ResponseWriter, r *http.Request) {
	var req createAdjustmentRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Delta == nil || *req.Delta == 0 {
		respondError(w, &entitlements.Error{
			Code:    entitlements.CodeInvalidRequest,
			Message: "delta is required and must be a non-zero integer",
		})
		return
	}
	effectiveAt, err := time.Parse(time.RFC3339, req.EffectiveAt)
	if req.EffectiveAt == "" || err != nil {
		respondError(w, &entitlements.Error{
			Code:    entitlements.CodeInvalidRequest,
			Message: "effective_at must be an RFC 3339 timestamp, e.g. 2026-06-01T00:00:00Z",
		})
		return
	}
	adjustment, created, err := h.service.CreateAdjustment(
		r.Context(), r.PathValue("entitlementID"), req.AdjustmentID, *req.Delta, effectiveAt)
	if err != nil {
		respondError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	respondJSON(w, status, adjustment)
}

type rescheduleRequest struct {
	RescheduleID string `json:"reschedule_id"`
	ValidFrom    string `json:"valid_from"`
	ValidTo      string `json:"valid_to"`
}

func (h *handlers) rescheduleEntitlement(w http.ResponseWriter, r *http.Request) {
	var req rescheduleRequest
	if !decode(w, r, &req) {
		return
	}
	validFrom, errFrom := time.Parse(time.RFC3339, req.ValidFrom)
	validTo, errTo := time.Parse(time.RFC3339, req.ValidTo)
	if req.ValidFrom == "" || req.ValidTo == "" || errFrom != nil || errTo != nil {
		respondError(w, &entitlements.Error{
			Code:    entitlements.CodeInvalidRequest,
			Message: "valid_from and valid_to must be RFC 3339 timestamps, e.g. 2026-01-01T00:00:00Z",
		})
		return
	}
	reschedule, created, err := h.service.RescheduleEntitlement(
		r.Context(), r.PathValue("entitlementID"), req.RescheduleID, validFrom, validTo)
	if err != nil {
		respondError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	respondJSON(w, status, reschedule)
}

func (h *handlers) getEntitlement(w http.ResponseWriter, r *http.Request) {
	view, err := h.service.GetView(r.Context(), r.PathValue("entitlementID"))
	if err != nil {
		respondError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, view)
}

// decode parses a JSON request body strictly; on failure it writes the error
// response and returns false.
func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		respondError(w, &entitlements.Error{
			Code:    entitlements.CodeInvalidRequest,
			Message: "request body must be a JSON object with the documented fields",
		})
		return false
	}
	return true
}

// errorStatus maps stable domain error codes to HTTP statuses.
var errorStatus = map[string]int{
	entitlements.CodeInvalidRequest:          http.StatusBadRequest,
	entitlements.CodeEntitlementExists:       http.StatusConflict,
	entitlements.CodeEntitlementNotFound:     http.StatusNotFound,
	entitlements.CodeEntitlementNotActive:    http.StatusUnprocessableEntity,
	entitlements.CodeInsufficientQuota:       http.StatusUnprocessableEntity,
	entitlements.CodeReservationNotFound:     http.StatusNotFound,
	entitlements.CodeReservationParamChanged: http.StatusConflict,
	entitlements.CodeReservationSettled:      http.StatusConflict,
	entitlements.CodeReservationNotConfirmed: http.StatusUnprocessableEntity,
	entitlements.CodeReservationReversed:     http.StatusConflict,
	entitlements.CodeAdjustmentParamChanged:  http.StatusConflict,
	entitlements.CodeRescheduleParamChanged:  http.StatusConflict,
}

func respondError(w http.ResponseWriter, err error) {
	var domainErr *entitlements.Error
	if !errors.As(err, &domainErr) {
		respondJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]string{"code": "internal", "message": "internal server error"},
		})
		return
	}
	status, ok := errorStatus[domainErr.Code]
	if !ok {
		respondJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]string{"code": "internal", "message": "internal server error"},
		})
		return
	}
	respondJSON(w, status, map[string]any{
		"error": map[string]string{"code": domainErr.Code, "message": domainErr.Message},
	})
}

func respondJSON(w http.ResponseWriter, status int, body any) {
	encoded, err := json.Marshal(body)
	if err != nil {
		respond(w, http.StatusInternalServerError, `{"error":{"code":"internal","message":"internal server error"}}`)
		return
	}
	respond(w, status, string(encoded))
}

func respond(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body + "\n"))
}

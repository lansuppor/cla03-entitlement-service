package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/lansolocoder/cla03-entitlement-service/internal/entitlements"
)

// New builds the HTTP handler. checkDatabase powers the readiness probe and
// service implements the entitlement use cases; the health endpoints behave
// exactly as they did before entitlement routes were added.
func New(checkDatabase func(context.Context) error, service entitlements.Service) http.Handler {
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

	mux.HandleFunc("POST /v1/entitlements", createEntitlement(service))
	mux.HandleFunc("GET /v1/entitlements/{entitlement_id}", getEntitlement(service))
	mux.HandleFunc("PUT /v1/entitlements/{entitlement_id}/reservations/{reservation_id}", reserve(service))
	mux.HandleFunc("POST /v1/entitlements/{entitlement_id}/reservations/{reservation_id}/confirm", settle(service, true))
	mux.HandleFunc("POST /v1/entitlements/{entitlement_id}/reservations/{reservation_id}/release", settle(service, false))
	return mux
}

// createEntitlementRequest is the JSON body of POST /v1/entitlements.
// Pointers distinguish an absent field from a zero value.
type createEntitlementRequest struct {
	EntitlementID *string    `json:"entitlement_id"`
	SeatTotal     *int64     `json:"seat_total"`
	CreditTotal   *int64     `json:"credit_total"`
	EffectiveFrom *time.Time `json:"effective_from"`
	EffectiveTo   *time.Time `json:"effective_to"`
}

type reserveRequest struct {
	Credit *int64 `json:"credit"`
}

func createEntitlement(service entitlements.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req createEntitlementRequest
		if !decode(w, r, &req) {
			return
		}
		in, err := buildEntitlementInput(req)
		if err != nil {
			writeError(w, err)
			return
		}
		view, err := service.CreateEntitlement(r.Context(), in)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, view)
	}
}

func buildEntitlementInput(req createEntitlementRequest) (entitlements.EntitlementInput, error) {
	var in entitlements.EntitlementInput
	if req.EntitlementID == nil || strings.TrimSpace(*req.EntitlementID) == "" {
		return in, entitlements.InvalidRequest("entitlement_id is required")
	}
	if req.SeatTotal == nil {
		return in, entitlements.InvalidRequest("seat_total is required")
	}
	if req.CreditTotal == nil {
		return in, entitlements.InvalidRequest("credit_total is required")
	}
	if req.EffectiveFrom == nil {
		return in, entitlements.InvalidRequest("effective_from is required and must be an RFC3339 timestamp")
	}
	if req.EffectiveTo == nil {
		return in, entitlements.InvalidRequest("effective_to is required and must be an RFC3339 timestamp")
	}
	in = entitlements.EntitlementInput{
		ID:            *req.EntitlementID,
		SeatTotal:     *req.SeatTotal,
		CreditTotal:   *req.CreditTotal,
		EffectiveFrom: *req.EffectiveFrom,
		EffectiveTo:   *req.EffectiveTo,
	}
	return in, in.Validate()
}

func getEntitlement(service entitlements.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		view, err := service.EntitlementView(r.Context(), r.PathValue("entitlement_id"))
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, view)
	}
}

func reserve(service entitlements.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req reserveRequest
		if !decode(w, r, &req) {
			return
		}
		if req.Credit == nil {
			writeError(w, entitlements.InvalidRequest("credit is required"))
			return
		}
		in := entitlements.ReservationInput{
			ID:     r.PathValue("reservation_id"),
			Credit: *req.Credit,
		}
		if err := in.Validate(); err != nil {
			writeError(w, err)
			return
		}
		view, err := service.Reserve(r.Context(), r.PathValue("entitlement_id"), in)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, view)
	}
}

func settle(service entitlements.Service, confirm bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		entitlementID := r.PathValue("entitlement_id")
		reservationID := r.PathValue("reservation_id")
		var (
			view *entitlements.ReservationView
			err  error
		)
		if confirm {
			view, err = service.Confirm(r.Context(), entitlementID, reservationID)
		} else {
			view, err = service.Release(r.Context(), entitlementID, reservationID)
		}
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, view)
	}
}

// decode parses a strictly-shaped JSON body. It writes the error response and
// reports false so callers can return immediately.
func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, entitlements.InvalidRequest("request body must be valid JSON: "+
			cleanJSONError(err)))
		return false
	}
	if dec.More() {
		writeError(w, entitlements.InvalidRequest("request body must contain a single JSON object"))
		return false
	}
	return true
}

func cleanJSONError(err error) string {
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return "malformed JSON"
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return "field " + typeErr.Field + " has the wrong type"
	}
	if strings.HasPrefix(err.Error(), "json: unknown field ") {
		return strings.TrimPrefix(err.Error(), "json: ")
	}
	return "malformed JSON"
}

type errorBody struct {
	Error errorPayload `json:"error"`
}

type errorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, err error) {
	var coded *entitlements.Error
	if !errors.As(err, &coded) {
		// Never leak internal details (connection strings, SQL errors, ...).
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: errorPayload{
			Code: "internal_error", Message: "internal server error",
		}})
		return
	}
	writeJSON(w, entitlements.HTTPStatus(coded.Code), errorBody{Error: errorPayload{
		Code: string(coded.Code), Message: coded.Message,
	}})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		respond(w, http.StatusInternalServerError, `{"error":{"code":"internal_error","message":"internal server error"}}`)
		return
	}
	respond(w, status, string(body))
}

func respond(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body + "\n"))
}

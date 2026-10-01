package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lansolocoder/cla03-entitlement-service/internal/entitlements"
)

type fakeService struct {
	view         *entitlements.View
	reservation  *entitlements.ReservationView
	err          error
	reserveCalls []entitlements.ReservationInput
}

func (f *fakeService) CreateEntitlement(_ context.Context, in entitlements.EntitlementInput) (*entitlements.View, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}
	return f.view, f.err
}

func (f *fakeService) EntitlementView(context.Context, string) (*entitlements.View, error) {
	return f.view, f.err
}

func (f *fakeService) Reserve(_ context.Context, _ string, in entitlements.ReservationInput) (*entitlements.ReservationView, error) {
	f.reserveCalls = append(f.reserveCalls, in)
	return f.reservation, f.err
}

func (f *fakeService) Confirm(context.Context, string, string) (*entitlements.ReservationView, error) {
	return f.reservation, f.err
}

func (f *fakeService) Release(context.Context, string, string) (*entitlements.ReservationView, error) {
	return f.reservation, f.err
}

func newTestHandler(svc entitlements.Service) http.Handler {
	return New(func(context.Context) error { return nil }, svc)
}

func TestCreateEntitlementValidation(t *testing.T) {
	svc := &fakeService{view: &entitlements.View{EntitlementID: "e1"}}
	handler := newTestHandler(svc)

	validBody := `{"entitlement_id":"e1","seat_total":10,"credit_total":100,"effective_from":"2026-01-01T00:00:00Z","effective_to":"2026-01-02T00:00:00Z"}`

	for _, tc := range []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{"malformed json", `{not json`, 400, "invalid_request"},
		{"unknown field", `{"entitlement_id":"e1","seat_total":10,"credit_total":100,"effective_from":"2026-01-01T00:00:00Z","effective_to":"2026-01-02T00:00:00Z","bogus":1}`, 400, "invalid_request"},
		{"missing id", `{"seat_total":10,"credit_total":100,"effective_from":"2026-01-01T00:00:00Z","effective_to":"2026-01-02T00:00:00Z"}`, 400, "invalid_request"},
		{"missing seats", `{"entitlement_id":"e1","credit_total":100,"effective_from":"2026-01-01T00:00:00Z","effective_to":"2026-01-02T00:00:00Z"}`, 400, "invalid_request"},
		{"negative credits", `{"entitlement_id":"e1","seat_total":10,"credit_total":-1,"effective_from":"2026-01-01T00:00:00Z","effective_to":"2026-01-02T00:00:00Z"}`, 400, "invalid_request"},
		{"bad window", `{"entitlement_id":"e1","seat_total":10,"credit_total":100,"effective_from":"2026-01-02T00:00:00Z","effective_to":"2026-01-01T00:00:00Z"}`, 400, "invalid_request"},
		{"wrong type", `{"entitlement_id":"e1","seat_total":"x","credit_total":100,"effective_from":"2026-01-01T00:00:00Z","effective_to":"2026-01-02T00:00:00Z"}`, 400, "invalid_request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/entitlements", strings.NewReader(tc.body))
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			assertErrorCode(t, rec.Body.String(), tc.wantCode)
		})
	}

	t.Run("success", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/entitlements", strings.NewReader(validBody))
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"entitlement_id":"e1"`) {
			t.Fatalf("unexpected body: %s", rec.Body.String())
		}
	})
}

func TestReserve(t *testing.T) {
	svc := &fakeService{reservation: &entitlements.ReservationView{
		ReservationID: "r1", Credit: 30, Status: entitlements.ReservationReserved,
	}}
	handler := newTestHandler(svc)

	t.Run("ok uses path id and body credit", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut,
			"/v1/entitlements/e1/reservations/r1", strings.NewReader(`{"credit":30}`))
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || len(svc.reserveCalls) != 1 {
			t.Fatalf("status=%d calls=%d body=%s", rec.Code, len(svc.reserveCalls), rec.Body.String())
		}
		if got := svc.reserveCalls[0]; got.ID != "r1" || got.Credit != 30 {
			t.Fatalf("unexpected input: %+v", got)
		}
	})

	for _, tc := range []struct {
		name       string
		body       string
		wantStatus int
	}{
		{"missing credit", `{}`, 400},
		{"zero credit", `{"credit":0}`, 400},
		{"negative credit", `{"credit":-5}`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPut,
				"/v1/entitlements/e1/reservations/r1", strings.NewReader(tc.body))
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			assertErrorCode(t, rec.Body.String(), "invalid_request")
		})
	}
}

func TestSettle(t *testing.T) {
	confirmed := entitlements.ReservationConfirmed
	svc := &fakeService{reservation: &entitlements.ReservationView{
		ReservationID: "r1", Credit: 30, Status: entitlements.ReservationConfirmed, Result: &confirmed,
	}}
	handler := newTestHandler(svc)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/v1/entitlements/e1/reservations/r1/confirm", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		ReservationID string  `json:"reservation_id"`
		Status        string  `json:"status"`
		Result        *string `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ReservationID != "r1" || body.Status != "confirmed" || body.Result == nil || *body.Result != "confirmed" {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}

func TestErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		name       string
		code       entitlements.Code
		wantStatus int
	}{
		{"not found", entitlements.CodeEntitlementNotFound, 404},
		{"reservation not found", entitlements.CodeReservationNotFound, 404},
		{"exists", entitlements.CodeEntitlementExists, 409},
		{"not effective", entitlements.CodeNotEffective, 409},
		{"expired", entitlements.CodeExpired, 409},
		{"insufficient", entitlements.CodeInsufficientCredit, 409},
		{"mismatch", entitlements.CodeIdempotencyMismatch, 409},
		{"already settled", entitlements.CodeAlreadySettled, 409},
		{"invalid", entitlements.CodeInvalidRequest, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &fakeService{
				err:  entitlements.NewError(tc.code, "boom"),
				view: nil,
			}
			handler := newTestHandler(svc)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/v1/entitlements/e1", nil)
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			assertErrorCode(t, rec.Body.String(), string(tc.code))
		})
	}
}

func TestInternalErrorNotLeaked(t *testing.T) {
	svc := &fakeService{err: context.DeadlineExceeded}
	handler := newTestHandler(svc)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/entitlements/e1", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(strings.ToLower(rec.Body.String()), "deadline") {
		t.Fatalf("internal error leaked: %s", rec.Body.String())
	}
	assertErrorCode(t, rec.Body.String(), "internal_error")
}

func TestEntitlementRoutesMethodNotAllowed(t *testing.T) {
	handler := newTestHandler(&fakeService{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/entitlements/e1", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d", rec.Code)
	}
}

func assertErrorCode(t *testing.T, body, want string) {
	t.Helper()
	var parsed struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("body is not JSON: %s", body)
	}
	if parsed.Error.Code != want {
		t.Fatalf("code=%q want=%q body=%s", parsed.Error.Code, want, body)
	}
}

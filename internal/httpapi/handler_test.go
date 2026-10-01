package httpapi

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lansolocoder/cla03-entitlement-service/internal/entitlements"
)

func TestHealthAndReadiness(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, body string
		dbError                  error
		status                   int
		calls                    int
	}{
		{"liveness", "GET", "/healthz", "ok", errors.New("database offline"), 200, 0},
		{"ready", "GET", "/readyz", "ready", nil, 200, 1},
		{"unavailable", "GET", "/readyz", "unavailable", errors.New("private connection details"), 503, 1},
		{"unknown path", "GET", "/nope", "404", nil, 404, 0},
		{"wrong method", "POST", "/readyz", "Method Not Allowed", nil, 405, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			handler := New(func(ctx context.Context) error {
				calls++
				if _, ok := ctx.Deadline(); !ok {
					t.Error("readiness database check must have a deadline")
				}
				return tc.dbError
			}, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(tc.method, tc.path, nil))
			if response.Code != tc.status || !strings.Contains(response.Body.String(), tc.body) || calls != tc.calls {
				t.Fatalf("status=%d body=%q database calls=%d", response.Code, response.Body.String(), calls)
			}
			if strings.Contains(response.Body.String(), "private connection details") {
				t.Fatal("database error leaked into public response")
			}
		})
	}
}

// fakeService records calls and returns scripted results.
type fakeService struct {
	entitlement  entitlements.Entitlement
	reservation  entitlements.Reservation
	created      bool
	view         entitlements.View
	err          error
	settleAction string
}

func (f *fakeService) CreateEntitlement(_ context.Context, _ entitlements.CreateEntitlementInput) (entitlements.Entitlement, error) {
	return f.entitlement, f.err
}

func (f *fakeService) CreateReservation(_ context.Context, _, _ string, _ int64) (entitlements.Reservation, bool, error) {
	return f.reservation, f.created, f.err
}

func (f *fakeService) SettleReservation(_ context.Context, _, _, action string) (entitlements.Reservation, error) {
	f.settleAction = action
	return f.reservation, f.err
}

func (f *fakeService) ReverseReservation(_ context.Context, _, _, _ string) (entitlements.Reservation, bool, error) {
	return f.reservation, f.created, f.err
}

func (f *fakeService) GetView(_ context.Context, _ string) (entitlements.View, error) {
	return f.view, f.err
}

func serve(t *testing.T, service Service, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	handler := New(func(context.Context) error { return nil }, service)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}

func TestCreateEntitlementValidation(t *testing.T) {
	service := &fakeService{}
	for _, tc := range []struct {
		name, body string
		wantCode   string
	}{
		{"malformed json", `{`, "invalid_request"},
		{"unknown field", `{"entitlement_id":"a","quota_total":1,"valid_from":"2026-01-01T00:00:00Z","valid_to":"2027-01-01T00:00:00Z","extra":1}`, "invalid_request"},
		{"bad timestamp", `{"entitlement_id":"a","quota_total":1,"valid_from":"soon","valid_to":"2027-01-01T00:00:00Z"}`, "invalid_request"},
		{"missing timestamps", `{"entitlement_id":"a","quota_total":1}`, "invalid_request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := serve(t, service, "POST", "/entitlements", tc.body)
			if response.Code != 400 || !strings.Contains(response.Body.String(), `"`+tc.wantCode+`"`) {
				t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
			}
		})
	}
}

func TestErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		name       string
		code       string
		wantStatus int
	}{
		{"invalid", entitlements.CodeInvalidRequest, 400},
		{"duplicate entitlement", entitlements.CodeEntitlementExists, 409},
		{"missing entitlement", entitlements.CodeEntitlementNotFound, 404},
		{"inactive entitlement", entitlements.CodeEntitlementNotActive, 422},
		{"insufficient quota", entitlements.CodeInsufficientQuota, 422},
		{"missing reservation", entitlements.CodeReservationNotFound, 404},
		{"param mismatch", entitlements.CodeReservationParamChanged, 409},
		{"already settled", entitlements.CodeReservationSettled, 409},
		{"internal", "unmapped", 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := &fakeService{err: &entitlements.Error{Code: tc.code, Message: "boom"}}
			response := serve(t, service, "POST", "/entitlements/ent-1/reservations", `{"reservation_id":"r1","amount":1}`)
			if response.Code != tc.wantStatus {
				t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), `"`+tc.code+`"`) && tc.code != "unmapped" {
				t.Fatalf("error code %q missing from body %q", tc.code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "boom") && tc.code == "unmapped" {
				t.Fatal("internal error detail leaked")
			}
		})
	}
}

func TestReservationReplayStatus(t *testing.T) {
	reservation := entitlements.Reservation{ReservationID: "r1", Amount: 5, Status: "pending", CreatedAt: time.Now()}
	created := serve(t, &fakeService{reservation: reservation, created: true},
		"POST", "/entitlements/ent-1/reservations", `{"reservation_id":"r1","amount":5}`)
	if created.Code != 201 {
		t.Fatalf("new reservation: status=%d body=%q", created.Code, created.Body.String())
	}
	replayed := serve(t, &fakeService{reservation: reservation, created: false},
		"POST", "/entitlements/ent-1/reservations", `{"reservation_id":"r1","amount":5}`)
	if replayed.Code != 200 {
		t.Fatalf("replayed reservation: status=%d body=%q", replayed.Code, replayed.Body.String())
	}
}

func TestSettleRoutes(t *testing.T) {
	service := &fakeService{reservation: entitlements.Reservation{ReservationID: "r1", Status: "confirmed"}}
	response := serve(t, service, "POST", "/entitlements/ent-1/reservations/r1/confirm", "")
	if response.Code != 200 || service.settleAction != entitlements.ActionConfirm {
		t.Fatalf("confirm: status=%d action=%q", response.Code, service.settleAction)
	}
	response = serve(t, service, "POST", "/entitlements/ent-1/reservations/r1/release", "")
	if response.Code != 200 || service.settleAction != entitlements.ActionRelease {
		t.Fatalf("release: status=%d action=%q", response.Code, service.settleAction)
	}
}

func TestReverseRoute(t *testing.T) {
	service := &fakeService{
		reservation: entitlements.Reservation{
			Kind: entitlements.StatusReversed, ReversalID: "corr-1",
			ReversalOf: "r1", Amount: -5, Status: entitlements.StatusReversed,
		},
		created: true,
	}
	response := serve(t, service, "POST", "/entitlements/ent-1/reservations/r1/reverse", `{"reversal_id":"corr-1"}`)
	if response.Code != 201 || !strings.Contains(response.Body.String(), `"reversal_id":"corr-1"`) ||
		!strings.Contains(response.Body.String(), `"amount":-5`) {
		t.Fatalf("new reversal: status=%d body=%q", response.Code, response.Body.String())
	}

	// A replay returns 200.
	service.created = false
	response = serve(t, service, "POST", "/entitlements/ent-1/reservations/r1/reverse", `{"reversal_id":"corr-1"}`)
	if response.Code != 200 {
		t.Fatalf("replayed reversal: status=%d body=%q", response.Code, response.Body.String())
	}

	for _, tc := range []struct {
		name, body string
	}{
		{"missing reversal_id", `{}`},
		{"malformed json", `{`},
		{"unknown field", `{"reversal_id":"corr-1","amount":5}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := serve(t, &fakeService{}, "POST", "/entitlements/ent-1/reservations/r1/reverse", tc.body)
			if response.Code != 400 || !strings.Contains(response.Body.String(), `"invalid_request"`) {
				t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
			}
		})
	}
}

func TestGetEntitlementView(t *testing.T) {
	service := &fakeService{view: entitlements.View{
		EntitlementID: "ent-1", QuotaTotal: 100, UsedAmount: 10,
		ReservedAmount: 20, AvailableAmount: 70, Status: "active",
		Reservations: []entitlements.Reservation{},
	}}
	response := serve(t, service, "GET", "/entitlements/ent-1", "")
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"available_amount":70`) {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}

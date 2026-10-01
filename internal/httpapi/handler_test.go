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
	reversal     entitlements.Reversal
	adjustment   entitlements.Adjustment
	created      bool
	view         entitlements.View
	err          error
	settleAction string
	reversalID   string
	adjustmentID string
	delta        int64
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

func (f *fakeService) ReverseReservation(_ context.Context, _, _, reversalID string) (entitlements.Reversal, bool, error) {
	f.reversalID = reversalID
	return f.reversal, f.created, f.err
}

func (f *fakeService) CreateAdjustment(_ context.Context, _, adjustmentID string, delta int64, _ time.Time) (entitlements.Adjustment, bool, error) {
	f.adjustmentID = adjustmentID
	f.delta = delta
	return f.adjustment, f.created, f.err
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
		{"not confirmed", entitlements.CodeReservationNotConfirmed, 422},
		{"already reversed", entitlements.CodeReservationReversed, 409},
		{"adjustment param mismatch", entitlements.CodeAdjustmentParamChanged, 409},
		{"adjustment quota nonpositive", entitlements.CodeAdjustmentNonPositive, 422},
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

func TestReverseRoute(t *testing.T) {
	reversal := entitlements.Reversal{ReversalID: "corr-1", ReservationID: "r1", Amount: 5, Status: "reversed", CreatedAt: time.Now()}
	service := &fakeService{reversal: reversal, created: true}
	created := serve(t, service, "POST", "/entitlements/ent-1/reservations/r1/reverse", `{"reversal_id":"corr-1"}`)
	if created.Code != 201 || service.reversalID != "corr-1" {
		t.Fatalf("new reversal: status=%d reversal_id=%q body=%q", created.Code, service.reversalID, created.Body.String())
	}
	if !strings.Contains(created.Body.String(), `"reversal_id":"corr-1"`) {
		t.Fatalf("reversal body: %q", created.Body.String())
	}
	service.created = false
	replayed := serve(t, service, "POST", "/entitlements/ent-1/reservations/r1/reverse", `{"reversal_id":"corr-1"}`)
	if replayed.Code != 200 {
		t.Fatalf("replayed reversal: status=%d body=%q", replayed.Code, replayed.Body.String())
	}
	// Unknown fields (a caller-supplied amount is not accepted) and malformed
	// JSON are rejected before the domain is called.
	for _, body := range []string{`{"reversal_id":"corr-1","amount":5}`, `{`} {
		response := serve(t, service, "POST", "/entitlements/ent-1/reservations/r1/reverse", body)
		if response.Code != 400 || !strings.Contains(response.Body.String(), `"invalid_request"`) {
			t.Fatalf("body %q: status=%d body=%q", body, response.Code, response.Body.String())
		}
	}
	// A missing reversal_id reaches the domain, which rejects it.
	invalid := &fakeService{err: &entitlements.Error{Code: entitlements.CodeInvalidRequest, Message: "bad reversal_id"}}
	response := serve(t, invalid, "POST", "/entitlements/ent-1/reservations/r1/reverse", `{}`)
	if response.Code != 400 || !strings.Contains(response.Body.String(), `"invalid_request"`) {
		t.Fatalf("missing reversal_id: status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestAdjustmentRoute(t *testing.T) {
	adjustment := entitlements.Adjustment{
		AdjustmentID: "adj-1", Delta: 20, Status: "applied",
		EffectiveAt: time.Now(), CreatedAt: time.Now(),
	}
	service := &fakeService{adjustment: adjustment, created: true}
	created := serve(t, service, "POST", "/entitlements/ent-1/adjustments",
		`{"adjustment_id":"adj-1","delta":20,"effective_at":"2026-02-01T00:00:00Z"}`)
	if created.Code != 201 || service.adjustmentID != "adj-1" || service.delta != 20 {
		t.Fatalf("new adjustment: status=%d adjustment_id=%q delta=%d body=%q",
			created.Code, service.adjustmentID, service.delta, created.Body.String())
	}
	if !strings.Contains(created.Body.String(), `"adjustment_id":"adj-1"`) ||
		!strings.Contains(created.Body.String(), `"delta":20`) {
		t.Fatalf("adjustment body: %q", created.Body.String())
	}
	service.created = false
	replayed := serve(t, service, "POST", "/entitlements/ent-1/adjustments",
		`{"adjustment_id":"adj-1","delta":20,"effective_at":"2026-02-01T00:00:00Z"}`)
	if replayed.Code != 200 {
		t.Fatalf("replayed adjustment: status=%d body=%q", replayed.Code, replayed.Body.String())
	}
	// Missing delta, a bad effective_at, unknown fields and malformed JSON are
	// rejected before the domain is called.
	for _, body := range []string{
		`{"adjustment_id":"adj-1","effective_at":"2026-02-01T00:00:00Z"}`,
		`{"adjustment_id":"adj-1","delta":20,"effective_at":"soon"}`,
		`{"adjustment_id":"adj-1","delta":20,"effective_at":"2026-02-01T00:00:00Z","amount":1}`,
		`{`,
	} {
		response := serve(t, service, "POST", "/entitlements/ent-1/adjustments", body)
		if response.Code != 400 || !strings.Contains(response.Body.String(), `"invalid_request"`) {
			t.Fatalf("body %q: status=%d body=%q", body, response.Code, response.Body.String())
		}
	}
	// A zero delta reaches the domain, which rejects it.
	invalid := &fakeService{err: &entitlements.Error{Code: entitlements.CodeInvalidRequest, Message: "zero delta"}}
	response := serve(t, invalid, "POST", "/entitlements/ent-1/adjustments",
		`{"adjustment_id":"adj-1","delta":0,"effective_at":"2026-02-01T00:00:00Z"}`)
	if response.Code != 400 || !strings.Contains(response.Body.String(), `"invalid_request"`) {
		t.Fatalf("zero delta: status=%d body=%q", response.Code, response.Body.String())
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

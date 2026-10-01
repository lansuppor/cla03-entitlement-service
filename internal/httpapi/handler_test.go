package httpapi

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
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
		{"unknown path", "GET", "/entitlements", "404", nil, 404, 0},
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

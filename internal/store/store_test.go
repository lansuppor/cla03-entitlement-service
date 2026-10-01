package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lansolocoder/cla03-entitlement-service/internal/entitlements"
)

// openTestPool opens the pool named by DATABASE_URL (defaulting to the local
// setup documented in the README) and skips the integration tests when no
// database is reachable.
func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://127.0.0.1:15432/entitlements?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Skipf("no PostgreSQL available: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("no PostgreSQL available: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func newTestStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	pool := openTestPool(t)
	s := New(pool)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Clean slate for any schema the migration created.
	if _, err := pool.Exec(ctx, `TRUNCATE entitlements CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return s, pool
}

func mustCreate(t *testing.T, s *Store, id string, seats, credits int64, from, to time.Time) *entitlements.View {
	t.Helper()
	view, err := s.CreateEntitlement(context.Background(), entitlements.EntitlementInput{
		ID: id, SeatTotal: seats, CreditTotal: credits,
		EffectiveFrom: from, EffectiveTo: to,
	})
	if err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
	return view
}

func TestCreateEntitlementLifecycle(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	// Active window straddles now; creation itself ignores the window.
	active := mustCreate(t, s, "e-active", 5, 100, now.Add(-time.Hour), now.Add(time.Hour))
	if active.Status != entitlements.StatusActive {
		t.Fatalf("new active entitlement status=%s", active.Status)
	}
	if active.CreditAvailable != 100 || active.CreditReserved != 0 || active.CreditUsed != 0 {
		t.Fatalf("unexpected initial totals: %+v", active)
	}
	if len(active.Reservations) != 0 {
		t.Fatalf("new entitlement must have no reservations")
	}

	pending := mustCreate(t, s, "e-pending", 5, 100, now.Add(time.Hour), now.Add(2*time.Hour))
	if pending.Status != entitlements.StatusPending {
		t.Fatalf("status=%s", pending.Status)
	}
	expired := mustCreate(t, s, "e-expired", 5, 100, now.Add(-2*time.Hour), now.Add(-time.Hour))
	if expired.Status != entitlements.StatusExpired {
		t.Fatalf("status=%s", expired.Status)
	}

	// Duplicate identifier fails without altering the original.
	_, err := s.CreateEntitlement(ctx, entitlements.EntitlementInput{
		ID: "e-active", SeatTotal: 9, CreditTotal: 9,
		EffectiveFrom: now, EffectiveTo: now.Add(time.Hour),
	})
	if !codeIs(err, entitlements.CodeEntitlementExists) {
		t.Fatalf("want entitlement_exists, got %v", err)
	}
	view, err := s.EntitlementView(ctx, "e-active")
	if err != nil || view.CreditTotal != 100 || view.SeatTotal != 5 {
		t.Fatalf("original mutated after duplicate create: %+v err=%v", view, err)
	}
}

func TestCreateValidationPersistsNothing(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	for _, in := range []entitlements.EntitlementInput{
		{ID: "", SeatTotal: 1, CreditTotal: 1, EffectiveFrom: now, EffectiveTo: now.Add(time.Hour)},
		{ID: "bad", SeatTotal: -1, CreditTotal: 1, EffectiveFrom: now, EffectiveTo: now.Add(time.Hour)},
		{ID: "bad", SeatTotal: 1, CreditTotal: -1, EffectiveFrom: now, EffectiveTo: now.Add(time.Hour)},
		{ID: "bad", SeatTotal: 1, CreditTotal: 1, EffectiveFrom: now.Add(time.Hour), EffectiveTo: now},
	} {
		if _, err := s.CreateEntitlement(ctx, in); !codeIs(err, entitlements.CodeInvalidRequest) {
			t.Fatalf("input %+v want invalid_request, got %v", in, err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM entitlements WHERE id = 'bad'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("invalid create persisted %d rows", n)
	}
}

func TestReserveIdempotencyAndMismatch(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	mustCreate(t, s, "e1", 10, 100, now.Add(-time.Hour), now.Add(time.Hour))

	first, err := s.Reserve(ctx, "e1", entitlements.ReservationInput{ID: "biz-1", Credit: 30})
	if err != nil || first.Status != entitlements.ReservationReserved {
		t.Fatalf("first reserve: %+v %v", first, err)
	}
	second, err := s.Reserve(ctx, "e1", entitlements.ReservationInput{ID: "biz-1", Credit: 30})
	if err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	if second.Status != first.Status || second.Credit != 30 {
		t.Fatalf("retry changed result: %+v", second)
	}
	view, _ := s.EntitlementView(ctx, "e1")
	if view.CreditReserved != 30 {
		t.Fatalf("retry must not double-deduct, reserved=%d", view.CreditReserved)
	}

	// Different parameters with the same key fail and change nothing.
	if _, err := s.Reserve(ctx, "e1", entitlements.ReservationInput{ID: "biz-1", Credit: 40}); !codeIs(err, entitlements.CodeIdempotencyMismatch) {
		t.Fatalf("want mismatch, got %v", err)
	}
	view, _ = s.EntitlementView(ctx, "e1")
	if view.CreditReserved != 30 || len(view.Reservations) != 1 {
		t.Fatalf("mismatch mutated state: %+v", view)
	}
}

func TestReserveValidityWindowAndQuota(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	mustCreate(t, s, "pending", 10, 100, now.Add(time.Hour), now.Add(2*time.Hour))
	mustCreate(t, s, "expired", 10, 100, now.Add(-2*time.Hour), now.Add(-time.Hour))

	if _, err := s.Reserve(ctx, "pending", entitlements.ReservationInput{ID: "r", Credit: 1}); !codeIs(err, entitlements.CodeNotEffective) {
		t.Fatalf("pending reserve: %v", err)
	}
	if _, err := s.Reserve(ctx, "expired", entitlements.ReservationInput{ID: "r", Credit: 1}); !codeIs(err, entitlements.CodeExpired) {
		t.Fatalf("expired reserve: %v", err)
	}
	if _, err := s.Reserve(ctx, "missing", entitlements.ReservationInput{ID: "r", Credit: 1}); !codeIs(err, entitlements.CodeEntitlementNotFound) {
		t.Fatalf("missing entitlement: %v", err)
	}

	mustCreate(t, s, "tight", 10, 10, now.Add(-time.Hour), now.Add(time.Hour))
	if _, err := s.Reserve(ctx, "tight", entitlements.ReservationInput{ID: "a", Credit: 6}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reserve(ctx, "tight", entitlements.ReservationInput{ID: "b", Credit: 5}); !codeIs(err, entitlements.CodeInsufficientCredit) {
		t.Fatalf("over-reserve: %v", err)
	}
	// Exactly the remaining available credit succeeds.
	if _, err := s.Reserve(ctx, "tight", entitlements.ReservationInput{ID: "c", Credit: 4}); err != nil {
		t.Fatalf("reserve to the limit: %v", err)
	}
	view, _ := s.EntitlementView(ctx, "tight")
	if view.CreditReserved != 10 || view.CreditAvailable != 0 || len(view.Reservations) != 2 {
		t.Fatalf("quota accounting wrong: %+v", view)
	}
}

func TestConfirmReleaseSettlement(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	mustCreate(t, s, "e1", 10, 100, now.Add(-time.Hour), now.Add(time.Hour))
	if _, err := s.Reserve(ctx, "e1", entitlements.ReservationInput{ID: "a", Credit: 30}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reserve(ctx, "e1", entitlements.ReservationInput{ID: "b", Credit: 20}); err != nil {
		t.Fatal(err)
	}

	confirmed, err := s.Confirm(ctx, "e1", "a")
	if err != nil || confirmed.Status != entitlements.ReservationConfirmed ||
		confirmed.Result == nil || *confirmed.Result != entitlements.ReservationConfirmed {
		t.Fatalf("confirm: %+v %v", confirmed, err)
	}
	view, _ := s.EntitlementView(ctx, "e1")
	if view.CreditUsed != 30 || view.CreditReserved != 20 || view.CreditAvailable != 50 {
		t.Fatalf("post-confirm totals: %+v", view)
	}

	released, err := s.Release(ctx, "e1", "b")
	if err != nil || released.Status != entitlements.ReservationReleased {
		t.Fatalf("release: %+v %v", released, err)
	}
	view, _ = s.EntitlementView(ctx, "e1")
	if view.CreditUsed != 30 || view.CreditReserved != 0 || view.CreditAvailable != 70 {
		t.Fatalf("post-release totals: %+v", view)
	}

	// Double settlement and confirm-after-release both fail without effect.
	if _, err := s.Confirm(ctx, "e1", "a"); !codeIs(err, entitlements.CodeAlreadySettled) {
		t.Fatalf("double confirm: %v", err)
	}
	if _, err := s.Release(ctx, "e1", "a"); !codeIs(err, entitlements.CodeAlreadySettled) {
		t.Fatalf("release after confirm: %v", err)
	}
	if _, err := s.Confirm(ctx, "e1", "b"); !codeIs(err, entitlements.CodeAlreadySettled) {
		t.Fatalf("confirm after release: %v", err)
	}
	if _, err := s.Confirm(ctx, "e1", "missing"); !codeIs(err, entitlements.CodeReservationNotFound) {
		t.Fatalf("confirm unknown: %v", err)
	}
	view, _ = s.EntitlementView(ctx, "e1")
	if view.CreditUsed != 30 || view.CreditReserved != 0 {
		t.Fatalf("failed settlement mutated state: %+v", view)
	}
	// Released credit is reusable.
	if _, err := s.Reserve(ctx, "e1", entitlements.ReservationInput{ID: "c", Credit: 70}); err != nil {
		t.Fatalf("reuse released credit: %v", err)
	}
}

func TestExpiredEntitlementStillSettles(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	// Window that expires during the test: settle after expiry is allowed.
	mustCreate(t, s, "e1", 10, 100, now.Add(-time.Hour), now.Add(40*time.Millisecond))
	if _, err := s.Reserve(ctx, "e1", entitlements.ReservationInput{ID: "a", Credit: 40}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	view, _ := s.EntitlementView(ctx, "e1")
	if view.Status != entitlements.StatusExpired {
		t.Fatalf("expected expiry, status=%s", view.Status)
	}
	if _, err := s.Confirm(ctx, "e1", "a"); err != nil {
		t.Fatalf("confirm after expiry should succeed: %v", err)
	}
	if _, err := s.Reserve(ctx, "e1", entitlements.ReservationInput{ID: "b", Credit: 1}); !codeIs(err, entitlements.CodeExpired) {
		t.Fatalf("reserve after expiry: %v", err)
	}
}

func TestConcurrentReservationsSerialize(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	mustCreate(t, s, "e1", 100, 100, now.Add(-time.Hour), now.Add(time.Hour))

	const workers = 20
	var wg sync.WaitGroup
	var success, rejected int64
	var mu sync.Mutex
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		i := i
		go func() {
			defer wg.Done()
			credit := int64(10)
			_, err := s.Reserve(ctx, "e1", entitlements.ReservationInput{
				ID: fmt.Sprintf("w-%d", i), Credit: credit,
			})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				success++
			} else if codeIs(err, entitlements.CodeInsufficientCredit) {
				rejected++
			} else {
				t.Errorf("worker %d unexpected error: %v", i, err)
			}
		}()
	}
	wg.Wait()
	if success != 10 || rejected != 10 {
		t.Fatalf("success=%d rejected=%d (want 10/10)", success, rejected)
	}
	view, err := s.EntitlementView(ctx, "e1")
	if err != nil {
		t.Fatal(err)
	}
	if view.CreditReserved != 100 || view.CreditUsed != 0 || view.CreditAvailable != 0 {
		t.Fatalf("invariant after concurrency: %+v", view)
	}
	if len(view.Reservations) != 10 {
		t.Fatalf("reservation count=%d", len(view.Reservations))
	}
}

func TestConcurrentMixedOpsSerialize(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	mustCreate(t, s, "e1", 100, 100, now.Add(-time.Hour), now.Add(time.Hour))
	for i := 0; i < 10; i++ {
		if _, err := s.Reserve(ctx, "e1", entitlements.ReservationInput{
			ID: fmt.Sprintf("r-%d", i), Credit: 10,
		}); err != nil {
			t.Fatal(err)
		}
	}

	const workers = 30
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		i := i
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("r-%d", i%10)
			switch i % 3 {
			case 0:
				_, _ = s.Confirm(ctx, "e1", id)
			case 1:
				_, _ = s.Release(ctx, "e1", id)
			case 2:
				// Duplicate settlement attempts must not corrupt totals.
				_, _ = s.Confirm(ctx, "e1", id)
			}
		}()
	}
	wg.Wait()
	view, err := s.EntitlementView(ctx, "e1")
	if err != nil {
		t.Fatal(err)
	}
	if view.CreditUsed+view.CreditReserved > view.CreditTotal {
		t.Fatalf("invariant violated: used=%d reserved=%d total=%d",
			view.CreditUsed, view.CreditReserved, view.CreditTotal)
	}
	if view.CreditUsed+view.CreditReserved+view.CreditAvailable != view.CreditTotal {
		t.Fatalf("totals do not balance: %+v", view)
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	mustCreate(t, s, "e1", 7, 100, now.Add(-time.Hour), now.Add(time.Hour))
	if _, err := s.Reserve(ctx, "e1", entitlements.ReservationInput{ID: "a", Credit: 40}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reserve(ctx, "e1", entitlements.ReservationInput{ID: "b", Credit: 30}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Confirm(ctx, "e1", "a"); err != nil {
		t.Fatal(err)
	}

	// Simulate a restart: a brand-new pool and store against the same database.
	pool.Close()
	freshPool := openTestPool(t)
	fresh := New(freshPool)
	if err := fresh.Migrate(ctx); err != nil {
		t.Fatalf("restart migrate: %v", err)
	}
	view, err := fresh.EntitlementView(ctx, "e1")
	if err != nil {
		t.Fatal(err)
	}
	if view.SeatTotal != 7 || view.CreditTotal != 100 || view.CreditUsed != 40 ||
		view.CreditReserved != 30 || view.CreditAvailable != 30 {
		t.Fatalf("state after restart inconsistent: %+v", view)
	}
	if len(view.Reservations) != 2 {
		t.Fatalf("reservations after restart: %d", len(view.Reservations))
	}
	// The settled and unsettled statuses survive too.
	byID := map[string]entitlements.ReservationView{}
	for _, r := range view.Reservations {
		byID[r.ReservationID] = r
	}
	if byID["a"].Status != entitlements.ReservationConfirmed {
		t.Fatalf("a status after restart=%s", byID["a"].Status)
	}
	if byID["b"].Status != entitlements.ReservationReserved {
		t.Fatalf("b status after restart=%s", byID["b"].Status)
	}
	// Unsettled reservation can still settle after restart.
	if _, err := fresh.Release(ctx, "e1", "b"); err != nil {
		t.Fatalf("release after restart: %v", err)
	}
}

func codeIs(err error, code entitlements.Code) bool {
	var target *entitlements.Error
	if errors.As(err, &target) {
		return target.Code == code
	}
	return false
}

package entitlements

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Integration tests run against a real PostgreSQL when TEST_DATABASE_URL is
// set, e.g.:
//
//	TEST_DATABASE_URL='postgres://127.0.0.1:15432/entitlements?sslmode=disable' go test ./...
func testStore(t *testing.T) *Store {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	store := NewStore(pool)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store
}

func ptr(v int64) *int64 { return &v }

// uniqueID generates a distinct entitlement ID per test so tests can share
// one database.
func uniqueID(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("ent-%d", time.Now().UnixNano())
}

func mustCreate(t *testing.T, store *Store, id string, quota int64, from, to time.Time) {
	t.Helper()
	_, err := store.CreateEntitlement(context.Background(), CreateEntitlementInput{
		EntitlementID: id, QuotaTotal: ptr(quota), ValidFrom: from, ValidTo: to,
	})
	if err != nil {
		t.Fatalf("create entitlement: %v", err)
	}
}

func activeWindow() (time.Time, time.Time) {
	return time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var domainErr *Error
	if !errors.As(err, &domainErr) || domainErr.Code != code {
		t.Fatalf("want error code %q, got %v", code, err)
	}
}

func TestCreateEntitlementValidation(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	for _, tc := range []struct {
		name string
		in   CreateEntitlementInput
	}{
		{"empty id", CreateEntitlementInput{QuotaTotal: ptr(10), ValidFrom: from, ValidTo: to}},
		{"bad id chars", CreateEntitlementInput{EntitlementID: "bad id!", QuotaTotal: ptr(10), ValidFrom: from, ValidTo: to}},
		{"missing quota", CreateEntitlementInput{EntitlementID: id, ValidFrom: from, ValidTo: to}},
		{"zero quota", CreateEntitlementInput{EntitlementID: id, QuotaTotal: ptr(0), ValidFrom: from, ValidTo: to}},
		{"negative seats", CreateEntitlementInput{EntitlementID: id, QuotaTotal: ptr(10), SeatsTotal: ptr(-1), ValidFrom: from, ValidTo: to}},
		{"empty window", CreateEntitlementInput{EntitlementID: id, QuotaTotal: ptr(10), ValidFrom: to, ValidTo: from}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantCode(t, store.createForTest(tc.in), CodeInvalidRequest)
			if _, err := store.GetView(context.Background(), tc.in.EntitlementID); err == nil && tc.in.EntitlementID != "" {
				t.Fatalf("invalid create wrote entitlement %q", tc.in.EntitlementID)
			}
		})
	}
}

// createForTest keeps the table-driven test readable.
func (s *Store) createForTest(in CreateEntitlementInput) error {
	_, err := s.CreateEntitlement(context.Background(), in)
	return err
}

func TestCreateEntitlementDefaultsAndDuplicate(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	ent, err := store.CreateEntitlement(context.Background(), CreateEntitlementInput{
		EntitlementID: id, QuotaTotal: ptr(50), ValidFrom: from, ValidTo: to,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if ent.SeatsTotal != 50 {
		t.Fatalf("seats_total should default to quota_total, got %d", ent.SeatsTotal)
	}
	wantCode(t, store.createForTest(CreateEntitlementInput{
		EntitlementID: id, QuotaTotal: ptr(50), ValidFrom: from, ValidTo: to,
	}), CodeEntitlementExists)
}

func TestReservationIdempotency(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	mustCreate(t, store, id, 100, from, to)
	ctx := context.Background()

	first, created, err := store.CreateReservation(ctx, id, "res-1", 30)
	if err != nil || !created || first.Status != StatusPending {
		t.Fatalf("first create: %+v created=%v err=%v", first, created, err)
	}
	replay, created, err := store.CreateReservation(ctx, id, "res-1", 30)
	if err != nil || created || replay != first {
		t.Fatalf("replay should return the original: %+v created=%v err=%v", replay, created, err)
	}
	view, err := store.GetView(ctx, id)
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	if view.ReservedAmount != 30 || view.AvailableAmount != 70 || len(view.Reservations) != 1 {
		t.Fatalf("replay consumed quota again: %+v", view)
	}

	_, _, err = store.CreateReservation(ctx, id, "res-1", 40)
	wantCode(t, err, CodeReservationParamChanged)
	view, _ = store.GetView(ctx, id)
	if view.ReservedAmount != 30 {
		t.Fatalf("param mismatch changed state: %+v", view)
	}
}

func TestInsufficientQuotaLeavesNoPartialState(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	mustCreate(t, store, id, 50, from, to)
	ctx := context.Background()

	if _, _, err := store.CreateReservation(ctx, id, "res-1", 40); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	_, _, err := store.CreateReservation(ctx, id, "res-2", 20)
	wantCode(t, err, CodeInsufficientQuota)
	view, _ := store.GetView(ctx, id)
	if view.ReservedAmount != 40 || view.AvailableAmount != 10 || len(view.Reservations) != 1 {
		t.Fatalf("failed reservation left partial state: %+v", view)
	}
}

func TestValidityWindow(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Now()

	futureID := uniqueID(t)
	mustCreate(t, store, futureID, 10, now.Add(time.Hour), now.Add(2*time.Hour))
	_, _, err := store.CreateReservation(ctx, futureID, "res-1", 1)
	wantCode(t, err, CodeEntitlementNotActive)

	expiredID := uniqueID(t)
	mustCreate(t, store, expiredID, 10, now.Add(-2*time.Hour), now.Add(-time.Hour))
	_, _, err = store.CreateReservation(ctx, expiredID, "res-1", 1)
	wantCode(t, err, CodeEntitlementNotActive)

	// A reservation made while active can still settle after expiry.
	liveID := uniqueID(t)
	mustCreate(t, store, liveID, 10, now.Add(-time.Hour), now.Add(time.Hour))
	if _, _, err := store.CreateReservation(ctx, liveID, "res-1", 5); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE entitlements SET valid_to = now() - interval '1 minute' WHERE entitlement_id = $1`, liveID); err != nil {
		t.Fatalf("expire entitlement: %v", err)
	}
	_, _, err = store.CreateReservation(ctx, liveID, "res-2", 1)
	wantCode(t, err, CodeEntitlementNotActive)
	settled, err := store.SettleReservation(ctx, liveID, "res-1", ActionConfirm)
	if err != nil || settled.Status != StatusConfirmed {
		t.Fatalf("confirm after expiry: %+v err=%v", settled, err)
	}
	view, _ := store.GetView(ctx, liveID)
	if view.Status != "expired" || view.UsedAmount != 5 {
		t.Fatalf("view after expiry: %+v", view)
	}
}

func TestSettleExactlyOnce(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	mustCreate(t, store, id, 100, from, to)
	ctx := context.Background()

	if _, _, err := store.CreateReservation(ctx, id, "res-1", 30); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	confirmed, err := store.SettleReservation(ctx, id, "res-1", ActionConfirm)
	if err != nil || confirmed.Status != StatusConfirmed || confirmed.SettledAt == nil {
		t.Fatalf("confirm: %+v err=%v", confirmed, err)
	}
	_, err = store.SettleReservation(ctx, id, "res-1", ActionConfirm)
	wantCode(t, err, CodeReservationSettled)
	_, err = store.SettleReservation(ctx, id, "res-1", ActionRelease)
	wantCode(t, err, CodeReservationSettled)

	if _, _, err := store.CreateReservation(ctx, id, "res-2", 20); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	released, err := store.SettleReservation(ctx, id, "res-2", ActionRelease)
	if err != nil || released.Status != StatusReleased {
		t.Fatalf("release: %+v err=%v", released, err)
	}
	_, err = store.SettleReservation(ctx, id, "res-2", ActionConfirm)
	wantCode(t, err, CodeReservationSettled)

	_, err = store.SettleReservation(ctx, id, "res-404", ActionConfirm)
	wantCode(t, err, CodeReservationNotFound)
	_, err = store.SettleReservation(ctx, "ent-404", "res-1", ActionConfirm)
	wantCode(t, err, CodeEntitlementNotFound)

	view, _ := store.GetView(ctx, id)
	if view.UsedAmount != 30 || view.ReservedAmount != 0 || view.AvailableAmount != 70 {
		t.Fatalf("quota after settlements: %+v", view)
	}
	if len(view.Reservations) != 2 ||
		view.Reservations[0].Status != StatusConfirmed || view.Reservations[1].Status != StatusReleased {
		t.Fatalf("reservation list: %+v", view.Reservations)
	}
}

func TestConcurrentReservationsNeverExceedQuota(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	const quota = 100
	mustCreate(t, store, id, quota, from, to)
	ctx := context.Background()

	const workers = 16
	var wg sync.WaitGroup
	errs := make(chan error, workers*4)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 4; i++ {
				rid := fmt.Sprintf("res-%d-%d", w, i)
				if _, _, err := store.CreateReservation(ctx, id, rid, 10); err != nil {
					errs <- err
					continue
				}
				// Mix confirmations and releases into the race.
				if i%2 == 0 {
					_, _ = store.SettleReservation(ctx, id, rid, ActionConfirm)
				} else {
					_, _ = store.SettleReservation(ctx, id, rid, ActionRelease)
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		var domainErr *Error
		if !errors.As(err, &domainErr) || domainErr.Code != CodeInsufficientQuota {
			t.Fatalf("unexpected concurrent failure: %v", err)
		}
	}
	view, err := store.GetView(ctx, id)
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	if view.UsedAmount+view.ReservedAmount > quota {
		t.Fatalf("invariant violated: used=%d reserved=%d quota=%d",
			view.UsedAmount, view.ReservedAmount, quota)
	}
	if view.AvailableAmount != quota-view.UsedAmount-view.ReservedAmount {
		t.Fatalf("inconsistent view: %+v", view)
	}
}

func TestReverseReservation(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	mustCreate(t, store, id, 100, from, to)
	ctx := context.Background()

	if _, _, err := store.CreateReservation(ctx, id, "res-1", 30); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := store.SettleReservation(ctx, id, "res-1", ActionConfirm); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	reversal, created, err := store.ReverseReservation(ctx, id, "res-1", "corr-1")
	if err != nil || !created {
		t.Fatalf("reverse: %+v created=%v err=%v", reversal, created, err)
	}
	if reversal.ReversalID != "corr-1" || reversal.ReservationID != "res-1" ||
		reversal.Amount != 30 || reversal.Status != StatusReversed || reversal.SettledAt == nil {
		t.Fatalf("reversal record: %+v", reversal)
	}
	view, err := store.GetView(ctx, id)
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	if view.UsedAmount != 0 || view.ReservedAmount != 0 || view.AvailableAmount != 100 {
		t.Fatalf("quota after reversal: %+v", view)
	}
	// Both the original reservation and the reversal appear in the list.
	if len(view.Reservations) != 2 {
		t.Fatalf("reservation list: %+v", view.Reservations)
	}
	original, entry := view.Reservations[0], view.Reservations[1]
	if original.ReservationID != "res-1" || original.Status != StatusConfirmed || original.SettledAt == nil {
		t.Fatalf("original reservation altered: %+v", original)
	}
	if entry.ReservationID != "corr-1" || entry.Amount != 30 ||
		entry.Status != StatusReversed || entry.SettledAt == nil {
		t.Fatalf("reversal entry: %+v", entry)
	}

	// Replaying the same correction identifier returns the original result
	// and does not refund again.
	replay, created, err := store.ReverseReservation(ctx, id, "res-1", "corr-1")
	if err != nil || created {
		t.Fatalf("replay: %+v created=%v err=%v", replay, created, err)
	}
	if replay.ReversalID != reversal.ReversalID || replay.ReservationID != reversal.ReservationID ||
		replay.Amount != reversal.Amount || replay.Status != reversal.Status ||
		!replay.CreatedAt.Equal(reversal.CreatedAt) || !replay.SettledAt.Equal(*reversal.SettledAt) {
		t.Fatalf("replay should return the original: %+v vs %+v", replay, reversal)
	}
	view, _ = store.GetView(ctx, id)
	if view.UsedAmount != 0 || view.AvailableAmount != 100 || len(view.Reservations) != 2 {
		t.Fatalf("replay refunded again: %+v", view)
	}

	// The same correction identifier targeting another reservation fails.
	if _, _, err := store.CreateReservation(ctx, id, "res-2", 10); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := store.SettleReservation(ctx, id, "res-2", ActionConfirm); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	_, _, err = store.ReverseReservation(ctx, id, "res-2", "corr-1")
	wantCode(t, err, CodeReservationParamChanged)
	view, _ = store.GetView(ctx, id)
	if view.UsedAmount != 10 || len(view.Reservations) != 3 {
		t.Fatalf("param mismatch changed state: %+v", view)
	}

	// The same reservation cannot be reversed twice.
	_, _, err = store.ReverseReservation(ctx, id, "res-1", "corr-2")
	wantCode(t, err, CodeReservationReversed)
	view, _ = store.GetView(ctx, id)
	if view.UsedAmount != 10 || view.AvailableAmount != 90 || len(view.Reservations) != 3 {
		t.Fatalf("second reversal changed state: %+v", view)
	}
}

func TestReverseValidation(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	mustCreate(t, store, id, 100, from, to)
	ctx := context.Background()

	// Pending and released reservations cannot be reversed.
	if _, _, err := store.CreateReservation(ctx, id, "res-pending", 10); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	_, _, err := store.ReverseReservation(ctx, id, "res-pending", "corr-1")
	wantCode(t, err, CodeReservationNotConfirmed)
	if _, _, err := store.CreateReservation(ctx, id, "res-released", 10); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := store.SettleReservation(ctx, id, "res-released", ActionRelease); err != nil {
		t.Fatalf("release: %v", err)
	}
	_, _, err = store.ReverseReservation(ctx, id, "res-released", "corr-2")
	wantCode(t, err, CodeReservationNotConfirmed)

	// Unknown reservation, unknown entitlement, illegal identifier.
	_, _, err = store.ReverseReservation(ctx, id, "res-404", "corr-3")
	wantCode(t, err, CodeReservationNotFound)
	_, _, err = store.ReverseReservation(ctx, "ent-404", "res-pending", "corr-3")
	wantCode(t, err, CodeEntitlementNotFound)
	_, _, err = store.ReverseReservation(ctx, id, "res-pending", "bad id!")
	wantCode(t, err, CodeInvalidRequest)
	_, _, err = store.ReverseReservation(ctx, id, "res-pending", "")
	wantCode(t, err, CodeInvalidRequest)

	// Failed attempts leave no partial state.
	view, _ := store.GetView(ctx, id)
	if view.UsedAmount != 0 || view.ReservedAmount != 10 || view.AvailableAmount != 90 ||
		len(view.Reservations) != 2 {
		t.Fatalf("failed reversals left partial state: %+v", view)
	}
}

func TestReverseAfterExpiry(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	mustCreate(t, store, id, 50, from, to)
	ctx := context.Background()
	if _, _, err := store.CreateReservation(ctx, id, "res-1", 20); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := store.SettleReservation(ctx, id, "res-1", ActionConfirm); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE entitlements SET valid_to = now() - interval '1 minute' WHERE entitlement_id = $1`, id); err != nil {
		t.Fatalf("expire entitlement: %v", err)
	}
	// New reservations are still rejected, but reversal stays allowed.
	_, _, err := store.CreateReservation(ctx, id, "res-2", 1)
	wantCode(t, err, CodeEntitlementNotActive)
	reversal, created, err := store.ReverseReservation(ctx, id, "res-1", "corr-1")
	if err != nil || !created || reversal.Amount != 20 {
		t.Fatalf("reverse after expiry: %+v created=%v err=%v", reversal, created, err)
	}
	view, _ := store.GetView(ctx, id)
	if view.Status != "expired" || view.UsedAmount != 0 || view.AvailableAmount != 50 {
		t.Fatalf("view after expiry reversal: %+v", view)
	}
}

func TestConcurrentReversalsSettleOnce(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	const quota = 100
	mustCreate(t, store, id, quota, from, to)
	ctx := context.Background()
	if _, _, err := store.CreateReservation(ctx, id, "res-1", 40); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := store.SettleReservation(ctx, id, "res-1", ActionConfirm); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	const workers = 8
	var wg sync.WaitGroup
	results := make(chan error, workers*2)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rid := fmt.Sprintf("corr-%d", w)
			// Each worker reverses once and replays the same correction.
			_, _, err := store.ReverseReservation(ctx, id, "res-1", rid)
			results <- err
			_, _, err = store.ReverseReservation(ctx, id, "res-1", rid)
			results <- err
		}(w)
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		var domainErr *Error
		if !errors.As(err, &domainErr) || domainErr.Code != CodeReservationReversed {
			t.Fatalf("unexpected concurrent failure: %v", err)
		}
	}
	// Exactly one correction took effect; its replay succeeded too.
	if successes != 2 {
		t.Fatalf("want 2 successful calls (one reversal + its replay), got %d", successes)
	}
	view, err := store.GetView(ctx, id)
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	if view.UsedAmount != 0 || view.AvailableAmount != quota || len(view.Reservations) != 2 {
		t.Fatalf("concurrent reversals broke state: %+v", view)
	}
}

func TestStateSurvivesReconnection(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	mustCreate(t, store, id, 100, from, to)
	ctx := context.Background()
	if _, _, err := store.CreateReservation(ctx, id, "res-1", 30); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := store.SettleReservation(ctx, id, "res-1", ActionConfirm); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if _, _, err := store.ReverseReservation(ctx, id, "res-1", "corr-1"); err != nil {
		t.Fatalf("reverse: %v", err)
	}

	// A fresh pool over the same database simulates a service restart.
	pool, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	defer pool.Close()
	restarted := NewStore(pool)
	view, err := restarted.GetView(ctx, id)
	if err != nil {
		t.Fatalf("view after restart: %v", err)
	}
	if view.UsedAmount != 0 || view.AvailableAmount != 100 || len(view.Reservations) != 2 ||
		view.Reservations[0].Status != StatusConfirmed || view.Reservations[1].Status != StatusReversed {
		t.Fatalf("state lost across restart: %+v", view)
	}
	// The correction identifier is still replayed, not re-applied.
	replay, created, err := restarted.ReverseReservation(ctx, id, "res-1", "corr-1")
	if err != nil || created || replay.Amount != 30 {
		t.Fatalf("reversal replay after restart: %+v created=%v err=%v", replay, created, err)
	}
}

func TestAdjustmentValidation(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	mustCreate(t, store, id, 100, from, to)
	ctx := context.Background()
	now := time.Now()

	for _, tc := range []struct {
		name         string
		adjustmentID string
		delta        int64
		effectiveAt  time.Time
		wantCode     string
	}{
		{"empty id", "", 10, now, CodeInvalidRequest},
		{"bad id chars", "bad id!", 10, now, CodeInvalidRequest},
		{"zero delta", "adj-1", 0, now, CodeInvalidRequest},
		{"missing effective time", "adj-1", 10, time.Time{}, CodeInvalidRequest},
		{"delta empties the quota", "adj-1", -100, now, CodeAdjustmentNonPositive},
		{"delta exceeds the quota", "adj-1", -101, now, CodeAdjustmentNonPositive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := store.CreateAdjustment(ctx, id, tc.adjustmentID, tc.delta, tc.effectiveAt)
			wantCode(t, err, tc.wantCode)
		})
	}
	_, _, err := store.CreateAdjustment(ctx, "ent-404", "adj-1", 10, now)
	wantCode(t, err, CodeEntitlementNotFound)

	// Failed attempts leave no partial state.
	view, _ := store.GetView(ctx, id)
	if view.QuotaTotal != 100 || len(view.Adjustments) != 0 {
		t.Fatalf("failed adjustments left partial state: %+v", view)
	}
}

func TestAdjustmentIdempotency(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	mustCreate(t, store, id, 100, from, to)
	ctx := context.Background()
	effectiveAt := time.Now().Add(-time.Minute)

	first, created, err := store.CreateAdjustment(ctx, id, "adj-1", 40, effectiveAt)
	if err != nil || !created || first.Status != StatusApplied || first.AppliedAt == nil {
		t.Fatalf("first create: %+v created=%v err=%v", first, created, err)
	}
	view, _ := store.GetView(ctx, id)
	if view.QuotaTotal != 140 || view.AvailableAmount != 140 {
		t.Fatalf("increase not counted: %+v", view)
	}

	// Replaying the same parameters returns the original and changes nothing.
	replay, created, err := store.CreateAdjustment(ctx, id, "adj-1", 40, effectiveAt)
	if err != nil || created {
		t.Fatalf("replay: %+v created=%v err=%v", replay, created, err)
	}
	if replay.AdjustmentID != first.AdjustmentID || replay.Delta != first.Delta ||
		!replay.EffectiveAt.Equal(first.EffectiveAt) || !replay.CreatedAt.Equal(first.CreatedAt) ||
		replay.Status != first.Status || !replay.AppliedAt.Equal(*first.AppliedAt) {
		t.Fatalf("replay should return the original: %+v vs %+v", replay, first)
	}
	view, _ = store.GetView(ctx, id)
	if view.QuotaTotal != 140 || len(view.Adjustments) != 1 {
		t.Fatalf("replay counted the adjustment again: %+v", view)
	}

	// The same identifier with a different delta or effective time fails and
	// changes nothing.
	_, _, err = store.CreateAdjustment(ctx, id, "adj-1", 50, effectiveAt)
	wantCode(t, err, CodeAdjustmentParamChanged)
	_, _, err = store.CreateAdjustment(ctx, id, "adj-1", 40, effectiveAt.Add(time.Hour))
	wantCode(t, err, CodeAdjustmentParamChanged)
	view, _ = store.GetView(ctx, id)
	if view.QuotaTotal != 140 || len(view.Adjustments) != 1 {
		t.Fatalf("param mismatch changed state: %+v", view)
	}
}

func TestAdjustmentTakesEffectAtEffectiveTime(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	mustCreate(t, store, id, 100, from, to)
	ctx := context.Background()

	// A future adjustment is recorded and visible but not yet counted.
	adjustment, created, err := store.CreateAdjustment(ctx, id, "adj-future", 25, time.Now().Add(time.Hour))
	if err != nil || !created || adjustment.Status != StatusPending || adjustment.AppliedAt != nil {
		t.Fatalf("future adjustment: %+v created=%v err=%v", adjustment, created, err)
	}
	view, _ := store.GetView(ctx, id)
	if view.QuotaTotal != 100 || view.AvailableAmount != 100 || len(view.Adjustments) != 1 ||
		view.Adjustments[0].Status != StatusPending {
		t.Fatalf("pending adjustment changed the quota: %+v", view)
	}

	// Once the effective time arrives, the next operation counts it exactly
	// once. The SQL update simulates the clock reaching the effective time.
	if _, err := store.pool.Exec(ctx, `
UPDATE adjustments SET effective_at = now() - interval '1 minute'
WHERE entitlement_id = $1 AND adjustment_id = 'adj-future'`, id); err != nil {
		t.Fatalf("reach effective time: %v", err)
	}
	view, _ = store.GetView(ctx, id)
	if view.QuotaTotal != 125 || view.AvailableAmount != 125 ||
		view.Adjustments[0].Status != StatusApplied || view.Adjustments[0].AppliedAt == nil {
		t.Fatalf("due adjustment not counted: %+v", view)
	}
	// A late replay of the same request does not count it again. (The test
	// moved the effective time in the database, so the replay mismatches the
	// stored parameters and is rejected without touching the quota.)
	_, _, err = store.CreateAdjustment(ctx, id, "adj-future", 25, time.Now().Add(time.Hour))
	wantCode(t, err, CodeAdjustmentParamChanged)
	view, _ = store.GetView(ctx, id)
	if view.QuotaTotal != 125 || len(view.Adjustments) != 1 {
		t.Fatalf("adjustment counted twice: %+v", view)
	}
}

func TestAdjustmentDecreaseWaitsForOccupiedQuota(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	mustCreate(t, store, id, 100, from, to)
	ctx := context.Background()

	// 60 of 100 is occupied: 30 confirmed (used) plus 30 held (reserved).
	if _, _, err := store.CreateReservation(ctx, id, "res-used", 30); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := store.SettleReservation(ctx, id, "res-used", ActionConfirm); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if _, _, err := store.CreateReservation(ctx, id, "res-held", 30); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	// A decrease to 50 cannot take effect while 60 is occupied: it stays
	// pending and the quota total is unchanged.
	effectiveAt := time.Now().Add(-time.Minute)
	adjustment, created, err := store.CreateAdjustment(ctx, id, "adj-cut", -50, effectiveAt)
	if err != nil || !created || adjustment.Status != StatusPending {
		t.Fatalf("blocked decrease: %+v created=%v err=%v", adjustment, created, err)
	}
	view, _ := store.GetView(ctx, id)
	if view.QuotaTotal != 100 || view.UsedAmount != 30 || view.ReservedAmount != 30 ||
		len(view.Adjustments) != 1 || view.Adjustments[0].Status != StatusPending {
		t.Fatalf("blocked decrease changed the quota: %+v", view)
	}
	// Replaying the blocked decrease returns the pending record unchanged.
	replay, created, err := store.CreateAdjustment(ctx, id, "adj-cut", -50, effectiveAt)
	if err != nil || created || replay.Status != StatusPending {
		t.Fatalf("replay of blocked decrease: %+v created=%v err=%v", replay, created, err)
	}

	// Releasing the hold drops the occupied amount to 30 (used only), which
	// fits the decreased total of 50: the next operation applies the decrease
	// automatically.
	if _, err := store.SettleReservation(ctx, id, "res-held", ActionRelease); err != nil {
		t.Fatalf("release: %v", err)
	}
	view, _ = store.GetView(ctx, id)
	if view.QuotaTotal != 50 || view.Adjustments[0].Status != StatusApplied {
		t.Fatalf("decrease should apply once occupancy fits: %+v", view)
	}
	if view.UsedAmount+view.ReservedAmount > view.QuotaTotal {
		t.Fatalf("invariant violated: %+v", view)
	}
	// The decrease was counted exactly once and the available quota reflects
	// the new total.
	if view.UsedAmount != 30 || view.AvailableAmount != 20 {
		t.Fatalf("quota after decrease: %+v", view)
	}
	// New reservations are bounded by the decreased total.
	_, _, err = store.CreateReservation(ctx, id, "res-over", 21)
	wantCode(t, err, CodeInsufficientQuota)
	if _, _, err := store.CreateReservation(ctx, id, "res-fit", 20); err != nil {
		t.Fatalf("reserve within decreased total: %v", err)
	}
}

func TestAdjustmentDecreaseNeverPartiallyApplies(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	mustCreate(t, store, id, 100, from, to)
	ctx := context.Background()
	if _, _, err := store.CreateReservation(ctx, id, "res-1", 80); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := store.SettleReservation(ctx, id, "res-1", ActionConfirm); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	// 80 used: a decrease to 70 stays pending, a later increase to 130 lets
	// it apply as 130 - 30 = 100 >= 80.
	if _, _, err := store.CreateAdjustment(ctx, id, "adj-cut", -30, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("decrease: %v", err)
	}
	view, _ := store.GetView(ctx, id)
	if view.QuotaTotal != 100 || view.Adjustments[0].Status != StatusPending {
		t.Fatalf("decrease should wait for occupancy: %+v", view)
	}
	if _, _, err := store.CreateAdjustment(ctx, id, "adj-up", 30, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("increase: %v", err)
	}
	view, _ = store.GetView(ctx, id)
	if view.QuotaTotal != 100 || view.Adjustments[0].Status != StatusApplied ||
		view.Adjustments[1].Status != StatusApplied {
		t.Fatalf("increase should unblock the pending decrease: %+v", view)
	}
	if view.UsedAmount != 80 || view.AvailableAmount != 20 {
		t.Fatalf("quota after both adjustments: %+v", view)
	}
}

func TestConcurrentAdjustmentsCountOnce(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	const quota = 100
	mustCreate(t, store, id, quota, from, to)
	ctx := context.Background()

	const workers = 8
	effectiveAt := time.Now().Add(-time.Minute)
	var wg sync.WaitGroup
	errs := make(chan error, workers*2)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			aid := fmt.Sprintf("adj-%d", w)
			// Each worker submits one increase and replays it; the replay must
			// not count the delta again.
			if _, _, err := store.CreateAdjustment(ctx, id, aid, 10, effectiveAt); err != nil {
				errs <- err
			}
			if _, _, err := store.CreateAdjustment(ctx, id, aid, 10, effectiveAt); err != nil {
				errs <- err
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("unexpected concurrent failure: %v", err)
	}
	view, err := store.GetView(ctx, id)
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	if view.QuotaTotal != quota+workers*10 || len(view.Adjustments) != workers {
		t.Fatalf("concurrent adjustments miscounted: %+v", view)
	}
	for _, a := range view.Adjustments {
		if a.Status != StatusApplied {
			t.Fatalf("adjustment left pending: %+v", a)
		}
	}
}

func TestAdjustmentSurvivesReconnection(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	mustCreate(t, store, id, 100, from, to)
	ctx := context.Background()
	effectiveAt := time.Now().Add(-time.Minute)
	if _, _, err := store.CreateAdjustment(ctx, id, "adj-applied", 20, effectiveAt); err != nil {
		t.Fatalf("increase: %v", err)
	}
	if _, _, err := store.CreateAdjustment(ctx, id, "adj-pending", 10, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("future increase: %v", err)
	}

	// A fresh pool over the same database simulates a service restart.
	pool, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	defer pool.Close()
	restarted := NewStore(pool)
	view, err := restarted.GetView(ctx, id)
	if err != nil {
		t.Fatalf("view after restart: %v", err)
	}
	if view.QuotaTotal != 120 || len(view.Adjustments) != 2 ||
		view.Adjustments[0].Status != StatusApplied || view.Adjustments[1].Status != StatusPending {
		t.Fatalf("adjustment state lost across restart: %+v", view)
	}
	// The applied adjustment is replayed, not re-applied.
	replay, created, err := restarted.CreateAdjustment(ctx, id, "adj-applied", 20, effectiveAt)
	if err != nil || created || replay.Status != StatusApplied {
		t.Fatalf("adjustment replay after restart: %+v created=%v err=%v", replay, created, err)
	}
	view, _ = restarted.GetView(ctx, id)
	if view.QuotaTotal != 120 {
		t.Fatalf("restart replay counted the adjustment again: %+v", view)
	}
}

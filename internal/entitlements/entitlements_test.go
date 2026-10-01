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
	past := time.Now().Add(-time.Minute)

	// Zero delta, illegal identifier, missing effective time, unknown
	// entitlement.
	_, _, err := store.CreateAdjustment(ctx, id, "adj-1", 0, past)
	wantCode(t, err, CodeInvalidRequest)
	_, _, err = store.CreateAdjustment(ctx, id, "bad id!", 10, past)
	wantCode(t, err, CodeInvalidRequest)
	_, _, err = store.CreateAdjustment(ctx, id, "adj-1", 10, time.Time{})
	wantCode(t, err, CodeInvalidRequest)
	_, _, err = store.CreateAdjustment(ctx, "ent-404", "adj-1", 10, past)
	wantCode(t, err, CodeEntitlementNotFound)

	// A decrease that would drop the quota total to zero or below fails and
	// writes nothing.
	_, _, err = store.CreateAdjustment(ctx, id, "adj-zero", -100, past)
	wantCode(t, err, CodeInsufficientQuota)
	_, _, err = store.CreateAdjustment(ctx, id, "adj-negative", -150, past)
	wantCode(t, err, CodeInsufficientQuota)

	view, err := store.GetView(ctx, id)
	if err != nil {
		t.Fatalf("view: %v", err)
	}
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
	effectiveAt := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)

	// A future adjustment is recorded as pending and does not change the
	// quota total yet.
	first, created, err := store.CreateAdjustment(ctx, id, "adj-1", 40, effectiveAt)
	if err != nil || !created || first.Status != StatusPending || first.AppliedAt != nil {
		t.Fatalf("first create: %+v created=%v err=%v", first, created, err)
	}
	view, err := store.GetView(ctx, id)
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	if view.QuotaTotal != 100 || view.AvailableAmount != 100 || len(view.Adjustments) != 1 {
		t.Fatalf("pending adjustment changed quota: %+v", view)
	}
	entry := view.Adjustments[0]
	if entry.AdjustmentID != "adj-1" || entry.Delta != 40 ||
		!entry.EffectiveAt.Equal(effectiveAt) || entry.Status != StatusPending {
		t.Fatalf("pending adjustment entry: %+v", entry)
	}

	// Replaying the same identifier with the same parameters returns the
	// original record and changes nothing.
	replay, created, err := store.CreateAdjustment(ctx, id, "adj-1", 40, effectiveAt)
	if err != nil || created {
		t.Fatalf("replay: %+v created=%v err=%v", replay, created, err)
	}
	if replay.AdjustmentID != first.AdjustmentID || replay.Delta != first.Delta ||
		!replay.EffectiveAt.Equal(first.EffectiveAt) || replay.Status != first.Status ||
		!replay.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("replay should return the original: %+v vs %+v", replay, first)
	}
	view, _ = store.GetView(ctx, id)
	if view.QuotaTotal != 100 || len(view.Adjustments) != 1 {
		t.Fatalf("replay changed state: %+v", view)
	}

	// The same identifier with a different delta or effective time fails
	// without changing state.
	_, _, err = store.CreateAdjustment(ctx, id, "adj-1", 50, effectiveAt)
	wantCode(t, err, CodeAdjustmentParamChanged)
	_, _, err = store.CreateAdjustment(ctx, id, "adj-1", 40, effectiveAt.Add(time.Hour))
	wantCode(t, err, CodeAdjustmentParamChanged)
	view, _ = store.GetView(ctx, id)
	if view.QuotaTotal != 100 || len(view.Adjustments) != 1 || view.Adjustments[0].Delta != 40 {
		t.Fatalf("param mismatch changed state: %+v", view)
	}
}

func TestAdjustmentAppliesExactlyOnceWhenDue(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	mustCreate(t, store, id, 100, from, to)
	ctx := context.Background()

	// An adjustment whose effective time has already arrived applies within
	// the create call.
	applied, created, err := store.CreateAdjustment(ctx, id, "adj-now", 50, time.Now().Add(-time.Minute))
	if err != nil || !created || applied.Status != StatusApplied || applied.AppliedAt == nil {
		t.Fatalf("immediate adjustment: %+v created=%v err=%v", applied, created, err)
	}
	view, _ := store.GetView(ctx, id)
	if view.QuotaTotal != 150 || view.AvailableAmount != 150 {
		t.Fatalf("quota after immediate adjustment: %+v", view)
	}

	// A future adjustment applies once its effective time arrives, even if
	// nothing but a read touches the entitlement.
	laterAt := time.Now().Add(1200 * time.Millisecond)
	if _, _, err := store.CreateAdjustment(ctx, id, "adj-later", 30, laterAt); err != nil {
		t.Fatalf("create future adjustment: %v", err)
	}
	view, _ = store.GetView(ctx, id)
	if view.QuotaTotal != 150 || view.Adjustments[1].Status != StatusPending {
		t.Fatalf("adjustment applied before its effective time: %+v", view)
	}
	time.Sleep(1500 * time.Millisecond)
	view, _ = store.GetView(ctx, id)
	if view.QuotaTotal != 180 || view.AvailableAmount != 180 || len(view.Adjustments) != 2 {
		t.Fatalf("due adjustment not applied by view: %+v", view)
	}
	for _, a := range view.Adjustments {
		if a.Status != StatusApplied || a.AppliedAt == nil {
			t.Fatalf("adjustment should be applied: %+v", a)
		}
	}
	// Further reads and late replays never apply it again.
	view, _ = store.GetView(ctx, id)
	if view.QuotaTotal != 180 {
		t.Fatalf("adjustment applied twice: %+v", view)
	}
	replay, created, err := store.CreateAdjustment(ctx, id, "adj-later", 30, laterAt)
	if err != nil || created || replay.Status != StatusApplied {
		t.Fatalf("late replay: %+v created=%v err=%v", replay, created, err)
	}
	view, _ = store.GetView(ctx, id)
	if view.QuotaTotal != 180 || view.AvailableAmount != 180 {
		t.Fatalf("late replay applied again: %+v", view)
	}

	// The grown total is what reservations are checked against.
	if _, _, err := store.CreateReservation(ctx, id, "res-1", 180); err != nil {
		t.Fatalf("reserve against adjusted quota: %v", err)
	}
	_, _, err = store.CreateReservation(ctx, id, "res-2", 1)
	wantCode(t, err, CodeInsufficientQuota)
}

func TestDecreaseAdjustmentBlockedByOccupancy(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	mustCreate(t, store, id, 100, from, to)
	ctx := context.Background()

	// Occupy 80 of 100: 60 confirmed, 20 still reserved.
	if _, _, err := store.CreateReservation(ctx, id, "res-confirm", 60); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := store.SettleReservation(ctx, id, "res-confirm", ActionConfirm); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if _, _, err := store.CreateReservation(ctx, id, "res-pending", 20); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	// A decrease to 50 is due immediately but 80 is occupied: it stays
	// pending and the quota total keeps its old value.
	decrease, created, err := store.CreateAdjustment(ctx, id, "adj-cut", -50, time.Now().Add(-time.Minute))
	if err != nil || !created || decrease.Status != StatusPending {
		t.Fatalf("blocked decrease: %+v created=%v err=%v", decrease, created, err)
	}
	view, _ := store.GetView(ctx, id)
	if view.QuotaTotal != 100 || view.UsedAmount != 60 || view.ReservedAmount != 20 ||
		len(view.Adjustments) != 1 || view.Adjustments[0].Status != StatusPending {
		t.Fatalf("blocked decrease changed quota: %+v", view)
	}

	// Releasing 20 is not enough (60 used still exceeds 50).
	if _, err := store.SettleReservation(ctx, id, "res-pending", ActionRelease); err != nil {
		t.Fatalf("release: %v", err)
	}
	view, _ = store.GetView(ctx, id)
	if view.QuotaTotal != 100 || view.Adjustments[0].Status != StatusPending {
		t.Fatalf("decrease applied while occupancy too high: %+v", view)
	}

	// Reversing the confirmed 60 frees the occupancy; the blocked decrease
	// takes effect within the same operation, exactly once.
	if _, _, err := store.ReverseReservation(ctx, id, "res-confirm", "corr-1"); err != nil {
		t.Fatalf("reverse: %v", err)
	}
	view, _ = store.GetView(ctx, id)
	if view.QuotaTotal != 50 || view.UsedAmount != 0 || view.AvailableAmount != 50 ||
		view.Adjustments[0].Status != StatusApplied || view.Adjustments[0].AppliedAt == nil {
		t.Fatalf("decrease did not apply once occupancy allowed: %+v", view)
	}
	// It never applies twice and never lets occupancy exceed the total.
	view, _ = store.GetView(ctx, id)
	if view.QuotaTotal != 50 || view.UsedAmount+view.ReservedAmount > view.QuotaTotal {
		t.Fatalf("decrease applied twice: %+v", view)
	}
}

func TestDecreaseNeverCrowdsOutOccupiedQuota(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	mustCreate(t, store, id, 100, from, to)
	ctx := context.Background()

	// Two decreases are registered while nothing is occupied; both are due
	// immediately. The first applies, the second would crowd out the quota
	// later confirmed against the reduced total.
	if _, _, err := store.CreateAdjustment(ctx, id, "adj-cut-1", -40, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("first decrease: %v", err)
	}
	second, _, err := store.CreateAdjustment(ctx, id, "adj-cut-2", -40, time.Now().Add(-time.Minute))
	if err != nil || second.Status != StatusApplied {
		t.Fatalf("second decrease should apply while empty: %+v err=%v", second, err)
	}
	view, _ := store.GetView(ctx, id)
	if view.QuotaTotal != 20 {
		t.Fatalf("quota after two decreases: %+v", view)
	}

	// Occupy the reduced total fully, then register another decrease: it
	// must stay pending while used plus reserved exceeds the adjusted total.
	if _, _, err := store.CreateReservation(ctx, id, "res-1", 20); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := store.SettleReservation(ctx, id, "res-1", ActionConfirm); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	third, _, err := store.CreateAdjustment(ctx, id, "adj-cut-3", -10, time.Now().Add(-time.Minute))
	if err != nil || third.Status != StatusPending {
		t.Fatalf("third decrease should be blocked: %+v err=%v", third, err)
	}
	view, _ = store.GetView(ctx, id)
	if view.QuotaTotal != 20 || view.UsedAmount != 20 || view.AvailableAmount != 0 {
		t.Fatalf("blocked decrease crowded out occupied quota: %+v", view)
	}
}

func TestConcurrentAdjustmentsApplyOnce(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	const quota = 100
	mustCreate(t, store, id, quota, from, to)
	ctx := context.Background()
	past := time.Now().Add(-time.Minute)

	const workers = 8
	var wg sync.WaitGroup
	results := make(chan error, workers*2)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			aid := fmt.Sprintf("adj-%d", w)
			// Each worker submits one increase and replays it; the replay
			// races the first submission of other workers.
			_, _, err := store.CreateAdjustment(ctx, id, aid, 5, past)
			results <- err
			_, _, err = store.CreateAdjustment(ctx, id, aid, 5, past)
			results <- err
		}(w)
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("unexpected concurrent failure: %v", err)
		}
	}
	view, err := store.GetView(ctx, id)
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	if view.QuotaTotal != quota+workers*5 || len(view.Adjustments) != workers {
		t.Fatalf("concurrent adjustments broke state: %+v", view)
	}
	for _, a := range view.Adjustments {
		if a.Status != StatusApplied {
			t.Fatalf("adjustment not applied: %+v", a)
		}
	}
}

func TestAdjustmentsSurviveReconnection(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	mustCreate(t, store, id, 100, from, to)
	ctx := context.Background()
	appliedAt := time.Now().Add(-time.Minute)
	if _, _, err := store.CreateAdjustment(ctx, id, "adj-applied", 20, appliedAt); err != nil {
		t.Fatalf("create applied adjustment: %v", err)
	}
	pendingAt := time.Now().Add(1200 * time.Millisecond)
	if _, _, err := store.CreateAdjustment(ctx, id, "adj-pending", 30, pendingAt); err != nil {
		t.Fatalf("create pending adjustment: %v", err)
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

	// A replay through the restarted service does not apply again, and a
	// pending adjustment that came due during the downtime applies once.
	replay, created, err := restarted.CreateAdjustment(ctx, id, "adj-applied", 20, appliedAt)
	if err != nil || created || replay.Status != StatusApplied {
		t.Fatalf("replay after restart: %+v created=%v err=%v", replay, created, err)
	}
	time.Sleep(1500 * time.Millisecond)
	view, _ = restarted.GetView(ctx, id)
	if view.QuotaTotal != 150 || view.Adjustments[1].Status != StatusApplied {
		t.Fatalf("due adjustment not applied after restart: %+v", view)
	}
}

func TestRescheduleValidation(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	mustCreate(t, store, id, 100, from, to)
	ctx := context.Background()
	newFrom := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	newTo := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	baseline, err := store.GetView(ctx, id)
	if err != nil {
		t.Fatalf("baseline view: %v", err)
	}

	// Illegal identifier, missing timestamp, reversed window, unknown
	// entitlement.
	_, _, err = store.RescheduleEntitlement(ctx, id, "bad id!", newFrom, newTo)
	wantCode(t, err, CodeInvalidRequest)
	_, _, err = store.RescheduleEntitlement(ctx, id, "move-1", time.Time{}, newTo)
	wantCode(t, err, CodeInvalidRequest)
	_, _, err = store.RescheduleEntitlement(ctx, id, "move-1", newFrom, time.Time{})
	wantCode(t, err, CodeInvalidRequest)
	_, _, err = store.RescheduleEntitlement(ctx, id, "move-1", newTo, newFrom)
	wantCode(t, err, CodeInvalidRequest)
	_, _, err = store.RescheduleEntitlement(ctx, id, "move-1", newFrom, newFrom)
	wantCode(t, err, CodeInvalidRequest)
	_, _, err = store.RescheduleEntitlement(ctx, "ent-404", "move-1", newFrom, newTo)
	wantCode(t, err, CodeEntitlementNotFound)

	// Failed attempts write neither a reschedule nor a new window.
	view, _ := store.GetView(ctx, id)
	if len(view.Reschedules) != 0 ||
		!view.ValidFrom.Equal(baseline.ValidFrom) || !view.ValidTo.Equal(baseline.ValidTo) {
		t.Fatalf("failed reschedule left partial state: %+v", view)
	}
}

func TestRescheduleIdempotency(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	mustCreate(t, store, id, 100, from, to)
	ctx := context.Background()
	newFrom := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Microsecond)
	newTo := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Microsecond)

	// First submission creates the reschedule and rewrites the window.
	first, created, err := store.RescheduleEntitlement(ctx, id, "move-1", newFrom, newTo)
	if err != nil || !created {
		t.Fatalf("first reschedule: %+v created=%v err=%v", first, created, err)
	}
	if !first.NewValidFrom.Equal(newFrom) || !first.NewValidTo.Equal(newTo) || first.CreatedAt.IsZero() {
		t.Fatalf("reschedule record: %+v", first)
	}
	view, _ := store.GetView(ctx, id)
	if !view.ValidFrom.Equal(newFrom) || !view.ValidTo.Equal(newTo) || view.Status != "pending" {
		t.Fatalf("window not rewritten: %+v", view)
	}
	if len(view.Reschedules) != 1 {
		t.Fatalf("reschedule list: %+v", view.Reschedules)
	}

	// Replaying the same identifier with the same window returns the
	// original record and does not touch the window again.
	before := view.Reschedules[0].CreatedAt
	replay, created, err := store.RescheduleEntitlement(ctx, id, "move-1", newFrom, newTo)
	if err != nil || created {
		t.Fatalf("replay: %+v created=%v err=%v", replay, created, err)
	}
	if replay.RescheduleID != first.RescheduleID || !replay.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("replay should return the original: %+v vs %+v", replay, first)
	}
	view, _ = store.GetView(ctx, id)
	if len(view.Reschedules) != 1 || !view.Reschedules[0].CreatedAt.Equal(before) {
		t.Fatalf("replay rewrote the reschedule: %+v", view.Reschedules)
	}

	// The same identifier with a different window fails and changes nothing.
	_, _, err = store.RescheduleEntitlement(ctx, id, "move-1", newFrom.Add(time.Hour), newTo)
	wantCode(t, err, CodeRescheduleParamChanged)
	_, _, err = store.RescheduleEntitlement(ctx, id, "move-1", newFrom, newTo.Add(time.Hour))
	wantCode(t, err, CodeRescheduleParamChanged)
	view, _ = store.GetView(ctx, id)
	if !view.ValidFrom.Equal(newFrom) || !view.ValidTo.Equal(newTo) || len(view.Reschedules) != 1 {
		t.Fatalf("param mismatch changed state: %+v", view)
	}
}

func TestRescheduleChangesActiveStateNotQuota(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Now()
	id := uniqueID(t)
	// Starts in the future: nothing can be reserved yet.
	mustCreate(t, store, id, 100, now.Add(time.Hour), now.Add(3*time.Hour))
	if _, _, err := store.CreateReservation(ctx, id, "res-1", 30); err != nil {
		wantCode(t, err, CodeEntitlementNotActive)
	}

	// Occupy quota after pulling the window forward over the current time.
	enterFrom := now.Add(-time.Hour).UTC().Truncate(time.Microsecond)
	enterTo := now.Add(time.Hour).UTC().Truncate(time.Microsecond)
	if _, _, err := store.RescheduleEntitlement(ctx, id, "move-enter", enterFrom, enterTo); err != nil {
		t.Fatalf("reschedule into window: %v", err)
	}
	if _, _, err := store.CreateReservation(ctx, id, "res-1", 30); err != nil {
		t.Fatalf("reserve after entering new window: %v", err)
	}
	if _, _, err := store.CreateReservation(ctx, id, "res-2", 20); err != nil {
		t.Fatalf("reserve pending amount: %v", err)
	}
	if _, err := store.SettleReservation(ctx, id, "res-1", ActionConfirm); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	view, _ := store.GetView(ctx, id)
	if view.Status != "active" || view.QuotaTotal != 100 || view.UsedAmount != 30 ||
		view.ReservedAmount != 20 || view.AvailableAmount != 50 {
		t.Fatalf("state after entering window: %+v", view)
	}

	// Reschedule the end to before now: new reservations are rejected, but
	// the unsettled hold still settles and the confirmed one still reverses.
	expireFrom := now.Add(-2 * time.Hour).UTC().Truncate(time.Microsecond)
	expireTo := now.Add(-time.Minute).UTC().Truncate(time.Microsecond)
	if _, _, err := store.RescheduleEntitlement(ctx, id, "move-expire", expireFrom, expireTo); err != nil {
		t.Fatalf("reschedule into expiry: %v", err)
	}
	_, _, err := store.CreateReservation(ctx, id, "res-3", 1)
	wantCode(t, err, CodeEntitlementNotActive)
	if _, err := store.SettleReservation(ctx, id, "res-2", ActionRelease); err != nil {
		t.Fatalf("release after new expiry: %v", err)
	}
	if _, _, err := store.ReverseReservation(ctx, id, "res-1", "corr-1"); err != nil {
		t.Fatalf("reverse after new expiry: %v", err)
	}
	view, _ = store.GetView(ctx, id)
	if view.Status != "expired" || !view.ValidTo.Equal(expireTo) {
		t.Fatalf("status after new expiry: %+v", view)
	}
	// No quota figure moved because of the reschedules themselves.
	if view.QuotaTotal != 100 || view.UsedAmount != 0 || view.ReservedAmount != 0 ||
		view.AvailableAmount != 100 {
		t.Fatalf("reschedule altered quota figures: %+v", view)
	}
	if len(view.Reschedules) != 2 {
		t.Fatalf("reschedule list should keep both records: %+v", view.Reschedules)
	}

	// Pulling the window back open makes new reservations possible again.
	reopenFrom := now.Add(-time.Hour).UTC().Truncate(time.Microsecond)
	reopenTo := now.Add(time.Hour).UTC().Truncate(time.Microsecond)
	if _, _, err := store.RescheduleEntitlement(ctx, id, "move-reopen", reopenFrom, reopenTo); err != nil {
		t.Fatalf("reschedule reopen: %v", err)
	}
	if _, _, err := store.CreateReservation(ctx, id, "res-4", 10); err != nil {
		t.Fatalf("reserve after reopening window: %v", err)
	}
	view, _ = store.GetView(ctx, id)
	if view.Status != "active" || view.ReservedAmount != 10 || view.AvailableAmount != 90 {
		t.Fatalf("state after reopening: %+v", view)
	}
}

func TestRescheduleDoesNotInteractWithAdjustments(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	mustCreate(t, store, id, 100, from, to)
	ctx := context.Background()

	// Register a pending future increase, then reschedule into a future
	// window: the adjustment stays pending and keeps its own effective time.
	futureAdj := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Microsecond)
	if _, _, err := store.CreateAdjustment(ctx, id, "adj-later", 40, futureAdj); err != nil {
		t.Fatalf("create pending adjustment: %v", err)
	}
	newFrom := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	newTo := time.Now().Add(4 * time.Hour).UTC().Truncate(time.Microsecond)
	if _, _, err := store.RescheduleEntitlement(ctx, id, "move-1", newFrom, newTo); err != nil {
		t.Fatalf("reschedule: %v", err)
	}
	view, _ := store.GetView(ctx, id)
	if view.QuotaTotal != 100 || len(view.Adjustments) != 1 ||
		view.Adjustments[0].Status != StatusPending || !view.Adjustments[0].EffectiveAt.Equal(futureAdj) {
		t.Fatalf("reschedule disturbed the pending adjustment: %+v", view)
	}
	if view.Status != "pending" {
		t.Fatalf("entitlement should be pending in its new window: %+v", view)
	}

	// An applied adjustment's counting is driven by its own effective time,
	// not by the rescheduled window: a past increase applies even though the
	// entitlement now lies in the future.
	past := time.Now().Add(-time.Minute).UTC().Truncate(time.Microsecond)
	applied, created, err := store.CreateAdjustment(ctx, id, "adj-now", 10, past)
	if err != nil || !created || applied.Status != StatusApplied {
		t.Fatalf("past adjustment after reschedule: %+v created=%v err=%v", applied, created, err)
	}
	view, _ = store.GetView(ctx, id)
	if view.QuotaTotal != 110 || view.AvailableAmount != 110 {
		t.Fatalf("applied adjustment not counted: %+v", view)
	}

	// Moving the window back open leaves the applied total intact and the
	// still-future adjustment still pending; reservations check against 110.
	reopenFrom := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	reopenTo := time.Now().Add(5 * time.Hour).UTC().Truncate(time.Microsecond)
	if _, _, err := store.RescheduleEntitlement(ctx, id, "move-2", reopenFrom, reopenTo); err != nil {
		t.Fatalf("reschedule reopen: %v", err)
	}
	if _, _, err := store.CreateReservation(ctx, id, "res-1", 110); err != nil {
		t.Fatalf("reserve against adjusted total: %v", err)
	}
	view, _ = store.GetView(ctx, id)
	if view.QuotaTotal != 110 || view.ReservedAmount != 110 || view.AvailableAmount != 0 ||
		view.Adjustments[0].Status != StatusPending {
		t.Fatalf("state after reopening: %+v", view)
	}
}

func TestReschedulesOrderedInView(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	mustCreate(t, store, id, 100, from, to)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Microsecond)
	for i, rc := range []struct {
		id             string
		fromOff, toOff time.Duration
	}{
		{"move-1", time.Hour, 2 * time.Hour},
		{"move-2", -time.Hour, 3 * time.Hour},
		{"move-3", -2 * time.Hour, -time.Minute},
	} {
		if _, _, err := store.RescheduleEntitlement(ctx, id, rc.id, base.Add(rc.fromOff), base.Add(rc.toOff)); err != nil {
			t.Fatalf("reschedule %d: %v", i, err)
		}
	}
	view, _ := store.GetView(ctx, id)
	if len(view.Reschedules) != 3 {
		t.Fatalf("reschedule list: %+v", view.Reschedules)
	}
	for i, wantID := range []string{"move-1", "move-2", "move-3"} {
		if view.Reschedules[i].RescheduleID != wantID ||
			view.Reschedules[i].NewValidFrom.IsZero() || view.Reschedules[i].NewValidTo.IsZero() ||
			view.Reschedules[i].CreatedAt.IsZero() {
			t.Fatalf("reschedule entry %d: %+v", i, view.Reschedules[i])
		}
	}
	// The view's window reflects the latest reschedule.
	last := view.Reschedules[2]
	if !view.ValidFrom.Equal(last.NewValidFrom) || !view.ValidTo.Equal(last.NewValidTo) || view.Status != "expired" {
		t.Fatalf("view window should reflect latest reschedule: %+v", view)
	}
}

func TestConcurrentReschedulesRewriteOnce(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	mustCreate(t, store, id, 100, from, to)
	ctx := context.Background()
	newFrom := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	newTo := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Microsecond)

	// Many workers submit the same reschedule identifier and also hammer the
	// entitlement with quota operations; all serialize on the row lock.
	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers*4)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			if _, _, err := store.RescheduleEntitlement(ctx, id, "move-1", newFrom, newTo); err != nil {
				errs <- err
			}
			if _, _, err := store.RescheduleEntitlement(ctx, id, "move-1", newFrom, newTo); err != nil {
				errs <- err
			}
			rid := fmt.Sprintf("res-%d", w)
			if _, _, err := store.CreateReservation(ctx, id, rid, 5); err != nil {
				errs <- err
				return
			}
			_, _ = store.SettleReservation(ctx, id, rid, ActionConfirm)
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
	if len(view.Reschedules) != 1 || !view.ValidFrom.Equal(newFrom) || !view.ValidTo.Equal(newTo) {
		t.Fatalf("reschedule applied more than once: %+v", view)
	}
	if view.UsedAmount != workers*5 || view.QuotaTotal != 100 ||
		view.AvailableAmount != 100-view.UsedAmount {
		t.Fatalf("quota state after concurrent reschedules: %+v", view)
	}
}

func TestReschedulesSurviveReconnection(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	mustCreate(t, store, id, 100, from, to)
	ctx := context.Background()
	newFrom := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	newTo := time.Now().Add(-time.Minute).UTC().Truncate(time.Microsecond)
	if _, _, err := store.RescheduleEntitlement(ctx, id, "move-1", newFrom, newTo); err != nil {
		t.Fatalf("reschedule: %v", err)
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
	if !view.ValidFrom.Equal(newFrom) || !view.ValidTo.Equal(newTo) ||
		view.Status != "expired" || len(view.Reschedules) != 1 {
		t.Fatalf("rescheduled window lost across restart: %+v", view)
	}
	// The reschedule identifier is still replayed, not re-applied.
	replay, created, err := restarted.RescheduleEntitlement(ctx, id, "move-1", newFrom, newTo)
	if err != nil || created || !replay.NewValidFrom.Equal(newFrom) {
		t.Fatalf("reschedule replay after restart: %+v created=%v err=%v", replay, created, err)
	}
	// New reservations stay blocked by the persisted, already-expired window.
	_, _, err = restarted.CreateReservation(ctx, id, "res-1", 1)
	wantCode(t, err, CodeEntitlementNotActive)
}

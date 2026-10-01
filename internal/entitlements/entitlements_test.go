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

func confirmRes(t *testing.T, store *Store, id, rid string, amount int64) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := store.CreateReservation(ctx, id, rid, amount); err != nil {
		t.Fatalf("reserve %s: %v", rid, err)
	}
	if _, err := store.SettleReservation(ctx, id, rid, ActionConfirm); err != nil {
		t.Fatalf("confirm %s: %v", rid, err)
	}
}

func TestReverseConfirmedReservation(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	mustCreate(t, store, id, 100, from, to)
	ctx := context.Background()
	confirmRes(t, store, id, "res-1", 30)

	reversal, created, err := store.ReverseReservation(ctx, id, "res-1", "corr-1")
	if err != nil || !created {
		t.Fatalf("reverse: %+v created=%v err=%v", reversal, created, err)
	}
	if reversal.Kind != StatusReversed || reversal.Status != StatusReversed ||
		reversal.ReversalID != "corr-1" || reversal.ReversalOf != "res-1" || reversal.Amount != -30 ||
		reversal.SettledAt == nil {
		t.Fatalf("reversal record: %+v", reversal)
	}

	// The original confirmation is untouched and both rows appear in the list.
	view, _ := store.GetView(ctx, id)
	if view.UsedAmount != 0 || view.ReservedAmount != 0 || view.AvailableAmount != 100 {
		t.Fatalf("quota after reversal: %+v", view)
	}
	if len(view.Reservations) != 2 {
		t.Fatalf("ledger should keep both rows: %+v", view.Reservations)
	}
	original := view.Reservations[0]
	reversed := view.Reservations[1]
	if original.ReservationID != "res-1" || original.Status != StatusConfirmed || original.Amount != 30 {
		t.Fatalf("original confirmation altered: %+v", original)
	}
	if reversed.ReversalID != "corr-1" || reversed.ReversalOf != "res-1" ||
		reversed.Status != StatusReversed || reversed.Amount != -30 {
		t.Fatalf("reversal ledger row: %+v", reversed)
	}
}

func TestReverseIdempotencyAndConflicts(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	mustCreate(t, store, id, 100, from, to)
	ctx := context.Background()
	confirmRes(t, store, id, "res-1", 30)
	confirmRes(t, store, id, "res-2", 20)

	first, created, err := store.ReverseReservation(ctx, id, "res-1", "corr-1")
	if err != nil || !created || first.Amount != -30 {
		t.Fatalf("first reverse: %+v created=%v err=%v", first, created, err)
	}
	// Identical replay returns the same result and does not refund again.
	replay, created, err := store.ReverseReservation(ctx, id, "res-1", "corr-1")
	if err != nil || created ||
		replay.ReversalID != first.ReversalID || replay.ReversalOf != first.ReversalOf ||
		replay.Amount != first.Amount || !replay.CreatedAt.Equal(first.CreatedAt) ||
		replay.SettledAt == nil || !replay.SettledAt.Equal(*first.SettledAt) {
		t.Fatalf("replay should return original: %+v created=%v err=%v", replay, created, err)
	}
	view, _ := store.GetView(ctx, id)
	if view.UsedAmount != 20 || view.AvailableAmount != 80 || len(view.Reservations) != 3 {
		t.Fatalf("replay refunded twice: %+v", view)
	}

	// Same correction id aimed at another reservation fails and changes nothing.
	_, _, err = store.ReverseReservation(ctx, id, "res-2", "corr-1")
	wantCode(t, err, CodeReservationParamChanged)

	// A reservation can be reversed at most once, even with a new id.
	_, _, err = store.ReverseReservation(ctx, id, "res-1", "corr-2")
	wantCode(t, err, CodeReservationSettled)
	view, _ = store.GetView(ctx, id)
	if view.UsedAmount != 20 || view.AvailableAmount != 80 {
		t.Fatalf("failed re-reversal changed state: %+v", view)
	}

	// The second reservation can still be corrected with its own id.
	if _, _, err := store.ReverseReservation(ctx, id, "res-2", "corr-3"); err != nil {
		t.Fatalf("reverse res-2: %v", err)
	}
	view, _ = store.GetView(ctx, id)
	if view.UsedAmount != 0 || view.AvailableAmount != 100 || len(view.Reservations) != 4 {
		t.Fatalf("quota after both reversals: %+v", view)
	}
}

func TestReverseRejectsInvalidTargets(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	mustCreate(t, store, id, 100, from, to)
	ctx := context.Background()

	// Pending reservation cannot be reversed.
	if _, _, err := store.CreateReservation(ctx, id, "res-pending", 10); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	_, _, err := store.ReverseReservation(ctx, id, "res-pending", "c1")
	wantCode(t, err, CodeReservationSettled)

	// Released reservation cannot be reversed.
	if _, _, err := store.CreateReservation(ctx, id, "res-released", 10); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := store.SettleReservation(ctx, id, "res-released", ActionRelease); err != nil {
		t.Fatalf("release: %v", err)
	}
	_, _, err = store.ReverseReservation(ctx, id, "res-released", "c2")
	wantCode(t, err, CodeReservationSettled)

	// Unknown reservation and unknown entitlement.
	_, _, err = store.ReverseReservation(ctx, id, "res-404", "c3")
	wantCode(t, err, CodeReservationNotFound)
	_, _, err = store.ReverseReservation(ctx, "ent-404", "res-1", "c4")
	wantCode(t, err, CodeEntitlementNotFound)

	// Bad correction identifier.
	_, _, err = store.ReverseReservation(ctx, id, "res-pending", "bad id!")
	wantCode(t, err, CodeInvalidRequest)

	view, _ := store.GetView(ctx, id)
	if view.UsedAmount != 0 || view.AvailableAmount != 90 || view.ReservedAmount != 10 {
		t.Fatalf("rejected reversals changed state: %+v", view)
	}
}

func TestReverseAfterExpiry(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Now()
	id := uniqueID(t)
	mustCreate(t, store, id, 100, now.Add(-time.Hour), now.Add(time.Hour))
	confirmRes(t, store, id, "res-1", 40)
	if _, err := store.pool.Exec(ctx, `UPDATE entitlements SET valid_to = now() - interval '1 minute' WHERE entitlement_id = $1`, id); err != nil {
		t.Fatalf("expire: %v", err)
	}
	// New reservations stay blocked after expiry...
	_, _, err := store.CreateReservation(ctx, id, "res-new", 1)
	wantCode(t, err, CodeEntitlementNotActive)
	// ...but correcting a confirmed reservation is still allowed.
	if _, _, err := store.ReverseReservation(ctx, id, "res-1", "corr-1"); err != nil {
		t.Fatalf("reverse after expiry: %v", err)
	}
	view, _ := store.GetView(ctx, id)
	if view.Status != "expired" || view.UsedAmount != 0 || view.AvailableAmount != 100 {
		t.Fatalf("view after expired reversal: %+v", view)
	}
}

func TestConcurrentReversalsApplyOnce(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	const quota = 100
	mustCreate(t, store, id, quota, from, to)
	ctx := context.Background()
	const n = 10
	for i := 0; i < n; i++ {
		confirmRes(t, store, id, fmt.Sprintf("res-%d", i), 10)
	}

	// Two different correction ids racing on each reservation; exactly one
	// must win, and duplicate-id replays must not refund twice.
	var wg sync.WaitGroup
	errs := make(chan error, n*4)
	for i := 0; i < n; i++ {
		rid := fmt.Sprintf("res-%d", i)
		for _, cid := range []string{rid + "-a", rid + "-b"} {
			wg.Add(1)
			go func(rid, cid string) {
				defer wg.Done()
				if _, _, err := store.ReverseReservation(ctx, id, rid, cid); err != nil {
					errs <- err
				}
			}(rid, cid)
			wg.Add(1)
			go func(rid, cid string) { // duplicate replay of -a
				defer wg.Done()
				if _, _, err := store.ReverseReservation(ctx, id, rid, rid+"-a"); err != nil {
					errs <- err
				}
			}(rid, cid)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		var domainErr *Error
		if !errors.As(err, &domainErr) || domainErr.Code != CodeReservationSettled {
			t.Fatalf("unexpected concurrent failure: %v", err)
		}
	}
	view, err := store.GetView(ctx, id)
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	if view.UsedAmount != 0 || view.AvailableAmount != quota {
		t.Fatalf("each reservation reversed once: used=%d available=%d", view.UsedAmount, view.AvailableAmount)
	}
	if view.UsedAmount+view.ReservedAmount > quota {
		t.Fatalf("invariant violated: %+v", view)
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
	if view.UsedAmount != 30 || view.AvailableAmount != 70 || len(view.Reservations) != 1 ||
		view.Reservations[0].Status != StatusConfirmed {
		t.Fatalf("state lost across restart: %+v", view)
	}
}

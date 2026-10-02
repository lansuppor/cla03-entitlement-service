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

// snapshotCount returns how many closure snapshots exist for an entitlement.
// At most one may ever be written.
func snapshotCount(t *testing.T, store *Store, id string) int {
	t.Helper()
	var n int
	if err := store.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM closure_snapshots WHERE entitlement_id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	return n
}

func expireEntitlement(t *testing.T, store *Store, id string) {
	t.Helper()
	if _, err := store.pool.Exec(context.Background(),
		`UPDATE entitlements SET valid_to = now() - interval '1 minute' WHERE entitlement_id = $1`, id); err != nil {
		t.Fatalf("expire entitlement: %v", err)
	}
}

func TestAutoCloseOnViewTakesSnapshotOnce(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	mustCreate(t, store, id, 100, from, to)
	ctx := context.Background()

	// Occupy quota: 40 confirmed, 20 still held.
	if _, _, err := store.CreateReservation(ctx, id, "res-used", "dept-1", 40); err != nil {
		t.Fatalf("reserve used: %v", err)
	}
	if _, err := store.SettleReservation(ctx, id, "res-used", ActionConfirm); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if _, _, err := store.CreateReservation(ctx, id, "res-held", "dept-1", 20); err != nil {
		t.Fatalf("reserve held: %v", err)
	}
	expireEntitlement(t, store, id)

	// The first view after expiry closes the entitlement and snapshots the
	// figures at that moment.
	view, err := store.GetView(ctx, id)
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	if view.Status != StatusClosed || view.ClosedAt == nil {
		t.Fatalf("entitlement not closed by view: %+v", view)
	}
	snap := view.Snapshot
	if snap == nil {
		t.Fatalf("closure snapshot missing: %+v", view)
	}
	if snap.SeatsTotal != 100 || snap.QuotaTotal != 100 || snap.UsedAmount != 40 ||
		snap.ReservedAmount != 20 || snap.AvailableAmount != 40 {
		t.Fatalf("snapshot figures wrong: %+v", snap)
	}
	if !snap.ClosedAt.Equal(*view.ClosedAt) || snap.ClosedAt.IsZero() {
		t.Fatalf("snapshot closure time %v should equal view closed_at %v", snap.ClosedAt, view.ClosedAt)
	}
	if !snap.ValidFrom.Equal(view.ValidFrom) || !snap.ValidTo.Equal(view.ValidTo) {
		t.Fatalf("snapshot window inconsistent: %+v", snap)
	}
	if snapshotCount(t, store, id) != 1 {
		t.Fatalf("expected exactly one closure snapshot")
	}

	// Settle the hold and correct the confirmation: the live figures move,
	// but the snapshot stays as it was at closure.
	if _, err := store.SettleReservation(ctx, id, "res-held", ActionRelease); err != nil {
		t.Fatalf("release after close: %v", err)
	}
	if _, _, err := store.ReverseReservation(ctx, id, "res-used", "corr-1"); err != nil {
		t.Fatalf("reverse after close: %v", err)
	}
	view, _ = store.GetView(ctx, id)
	if view.UsedAmount != 0 || view.ReservedAmount != 0 || view.AvailableAmount != 100 {
		t.Fatalf("live figures after post-close settlement: %+v", view)
	}
	if view.Snapshot.UsedAmount != 40 || view.Snapshot.ReservedAmount != 20 ||
		view.Snapshot.AvailableAmount != 40 {
		t.Fatalf("snapshot moved with live figures: %+v", view.Snapshot)
	}
	// Repeated views and writes never create a second close record.
	_, _ = store.GetView(ctx, id)
	_, _ = store.SettleReservation(ctx, id, "res-used", ActionRelease)
	if snapshotCount(t, store, id) != 1 || view.Status != StatusClosed {
		t.Fatalf("second closure record appeared")
	}
}

func TestClosureRejectsNewIssuanceAllowsInFlight(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	mustCreate(t, store, id, 100, from, to)
	ctx := context.Background()
	if _, _, err := store.CreateReservation(ctx, id, "res-1", "dept-1", 30); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := store.SettleReservation(ctx, id, "res-1", ActionConfirm); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if _, _, err := store.CreateReservation(ctx, id, "res-2", "dept-1", 20); err != nil {
		t.Fatalf("reserve in-flight: %v", err)
	}
	past := time.Now().Add(-time.Minute)
	expireEntitlement(t, store, id)

	// New issuance and every figure/window change fail as closed and write
	// nothing; identical replays of records already registered still return
	// them.
	if _, _, err := store.CreateReservation(ctx, id, "res-3", "dept-1", 1); !isCode(err, CodeEntitlementClosed) {
		t.Fatalf("new reservation after close: %v", err)
	}
	if _, _, err := store.CreateAdjustment(ctx, id, "adj-1", 10, past); !isCode(err, CodeEntitlementClosed) {
		t.Fatalf("adjustment after close: %v", err)
	}
	if _, _, err := store.CreateSeatAdjustment(ctx, id, "seat-1", 10); !isCode(err, CodeEntitlementClosed) {
		t.Fatalf("seat adjustment after close: %v", err)
	}
	if _, _, err := store.RescheduleEntitlement(ctx, id, "move-1", from, time.Now().Add(time.Hour)); !isCode(err, CodeEntitlementClosed) {
		t.Fatalf("reschedule after close: %v", err)
	}
	if _, _, err := store.PauseEntitlement(ctx, id, "pause-1", time.Now()); !isCode(err, CodeEntitlementClosed) {
		t.Fatalf("pause after close: %v", err)
	}

	// In-flight holds still settle and confirmed ones still reverse.
	if _, err := store.SettleReservation(ctx, id, "res-2", ActionRelease); err != nil {
		t.Fatalf("release in-flight after close: %v", err)
	}
	if _, _, err := store.ReverseReservation(ctx, id, "res-1", "corr-1"); err != nil {
		t.Fatalf("reverse confirmed after close: %v", err)
	}

	// Failed requests left no ledger rows of their own.
	view, _ := store.GetView(ctx, id)
	if len(view.Adjustments) != 0 || len(view.SeatAdjustments) != 0 ||
		len(view.Reschedules) != 0 || len(view.PauseResume) != 0 {
		t.Fatalf("failed post-close requests wrote records: %+v", view)
	}
	if view.QuotaTotal != 100 || view.SeatsTotal != 100 || view.UsedAmount != 0 {
		t.Fatalf("figures changed after close: %+v", view)
	}
}

func TestClosureIsOneWayAcrossReschedule(t *testing.T) {
	store := testStore(t)
	now := time.Now()
	id := uniqueID(t)
	mustCreate(t, store, id, 100, now.Add(-time.Hour), now.Add(time.Hour))
	ctx := context.Background()

	// Expire and close via a view, then rewrite the window back over now via
	// a *different* reschedule identifier: closure rejects it.
	expireEntitlement(t, store, id)
	if _, err := store.GetView(ctx, id); err != nil {
		t.Fatalf("closing view: %v", err)
	}
	reopenFrom := now.Add(-time.Hour).UTC().Truncate(time.Microsecond)
	reopenTo := now.Add(time.Hour).UTC().Truncate(time.Microsecond)
	_, _, err := store.RescheduleEntitlement(ctx, id, "move-reopen", reopenFrom, reopenTo)
	if !isCode(err, CodeEntitlementClosed) {
		t.Fatalf("reopen closed entitlement: %v", err)
	}
	view, _ := store.GetView(ctx, id)
	if view.Status != StatusClosed || view.ValidTo.After(now) || snapshotCount(t, store, id) != 1 {
		t.Fatalf("closed entitlement reopened: %+v", view)
	}
}

func TestPauseRejectsNewReservationsOnly(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	mustCreate(t, store, id, 100, from, to)
	ctx := context.Background()
	if _, _, err := store.CreateReservation(ctx, id, "res-1", "dept-1", 30); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := store.SettleReservation(ctx, id, "res-1", ActionConfirm); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if _, _, err := store.CreateReservation(ctx, id, "res-2", "dept-1", 20); err != nil {
		t.Fatalf("reserve in-flight: %v", err)
	}

	// Pause within the window.
	pauseAt := time.Now().Add(-time.Minute)
	paused, created, err := store.PauseEntitlement(ctx, id, "pause-1", pauseAt)
	if err != nil || !created || paused.Type != ActionPause {
		t.Fatalf("pause: %+v created=%v err=%v", paused, created, err)
	}

	// New issuance is refused while paused; in-flight settlement and
	// correction still proceed.
	if _, _, err := store.CreateReservation(ctx, id, "res-3", "dept-1", 1); !isCode(err, CodeEntitlementPaused) {
		t.Fatalf("reserve while paused: %v", err)
	}
	if _, err := store.SettleReservation(ctx, id, "res-2", ActionRelease); err != nil {
		t.Fatalf("release while paused: %v", err)
	}
	if _, _, err := store.ReverseReservation(ctx, id, "res-1", "corr-1"); err != nil {
		t.Fatalf("reverse while paused: %v", err)
	}

	// Resume; issuance works again and no figure moved because of pause/resume.
	resumeAt := time.Now()
	resumed, created, err := store.ResumeEntitlement(ctx, id, "resume-1", resumeAt)
	if err != nil || !created || resumed.Type != ActionResume {
		t.Fatalf("resume: %+v created=%v err=%v", resumed, created, err)
	}
	if _, _, err := store.CreateReservation(ctx, id, "res-4", "dept-1", 10); err != nil {
		t.Fatalf("reserve after resume: %v", err)
	}
	view, _ := store.GetView(ctx, id)
	if view.QuotaTotal != 100 || view.SeatsTotal != 100 || view.UsedAmount != 0 ||
		view.ReservedAmount != 10 || view.AvailableAmount != 90 {
		t.Fatalf("pause/resume moved figures: %+v", view)
	}
	if len(view.PauseResume) != 2 {
		t.Fatalf("pause/resume ledger: %+v", view.PauseResume)
	}
}

func TestPauseResumeIdempotencyAndSharedIDSpace(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	mustCreate(t, store, id, 100, from, to)
	ctx := context.Background()
	pauseAt := time.Now().Add(-2 * time.Minute)
	resumeAt := time.Now().Add(-time.Minute)

	first, created, err := store.PauseEntitlement(ctx, id, "evt-1", pauseAt)
	if err != nil || !created {
		t.Fatalf("first pause: %+v created=%v err=%v", first, created, err)
	}
	// Identical replay returns the original and does not take effect again.
	replay, created, err := store.PauseEntitlement(ctx, id, "evt-1", pauseAt)
	if err != nil || created || replay.CreatedAt != first.CreatedAt || replay.At != first.At {
		t.Fatalf("pause replay: %+v created=%v err=%v", replay, created, err)
	}
	// Same identifier with a different time fails atomically.
	if _, _, err := store.PauseEntitlement(ctx, id, "evt-1", pauseAt.Add(time.Second)); !isCode(err, CodePauseResumeParamChanged) {
		t.Fatalf("pause replay with different time: %v", err)
	}
	// A resume request reusing a pause identifier (even with the same time)
	// is a parameter mismatch; identifiers are independent across kinds.
	if _, _, err := store.ResumeEntitlement(ctx, id, "evt-1", pauseAt); !isCode(err, CodePauseResumeParamChanged) {
		t.Fatalf("resume reusing pause identifier: %v", err)
	}
	// Still paused: the mismatches changed nothing.
	if _, _, err := store.PauseEntitlement(ctx, id, "pause-2", time.Now()); !isCode(err, CodePauseResumeConflict) {
		t.Fatalf("second pause while paused: %v", err)
	}

	// Proper resume, then its replays and a resume-of-resumed conflict.
	resumed, created, err := store.ResumeEntitlement(ctx, id, "resume-1", resumeAt)
	if err != nil || !created {
		t.Fatalf("resume: %+v created=%v err=%v", resumed, created, err)
	}
	if _, created, err := store.ResumeEntitlement(ctx, id, "resume-1", resumeAt); err != nil || created {
		t.Fatalf("resume replay: created=%v err=%v", created, err)
	}
	if _, _, err := store.ResumeEntitlement(ctx, id, "resume-1", resumeAt.Add(time.Second)); !isCode(err, CodePauseResumeParamChanged) {
		t.Fatalf("resume replay different time: %v", err)
	}
	if _, _, err := store.ResumeEntitlement(ctx, id, "resume-2", time.Now()); !isCode(err, CodePauseResumeConflict) {
		t.Fatalf("resume while resumed: %v", err)
	}
	// A pause identifier may be reused for a later, independent pause cycle
	// only with a *new* identifier; the old one stays a pause.
	if _, _, err := store.PauseEntitlement(ctx, id, "pause-2", time.Now()); err != nil {
		t.Fatalf("second pause cycle: %v", err)
	}

	view, _ := store.GetView(ctx, id)
	if len(view.PauseResume) != 3 {
		t.Fatalf("ledger should hold evt-1, resume-1, pause-2: %+v", view.PauseResume)
	}
}

func TestPauseResumeValidation(t *testing.T) {
	store := testStore(t)
	now := time.Now()
	from, to := now.Add(-time.Hour), now.Add(time.Hour)
	id := uniqueID(t)
	mustCreate(t, store, id, 100, from, to)
	ctx := context.Background()

	// Unknown entitlement is a distinct failure.
	if _, _, err := store.PauseEntitlement(ctx, "ent-404", "pause-1", now); !isCode(err, CodeEntitlementNotFound) {
		t.Fatalf("pause unknown: %v", err)
	}
	if _, _, err := store.ResumeEntitlement(ctx, "ent-404", "resume-1", now); !isCode(err, CodeEntitlementNotFound) {
		t.Fatalf("resume unknown: %v", err)
	}
	// Illegal identifier and missing time are invalid requests.
	if _, _, err := store.PauseEntitlement(ctx, id, "bad id!", now); !isCode(err, CodeInvalidRequest) {
		t.Fatalf("bad pause id: %v", err)
	}
	if _, _, err := store.PauseEntitlement(ctx, id, "pause-bad", time.Time{}); !isCode(err, CodeInvalidRequest) {
		t.Fatalf("missing pause time: %v", err)
	}
	// A pause outside the window (before start or after end) is invalid.
	if _, _, err := store.PauseEntitlement(ctx, id, "pause-early", from.Add(-time.Minute)); !isCode(err, CodeInvalidRequest) {
		t.Fatalf("pause before start: %v", err)
	}
	// Resuming an entitlement that was never paused is a state conflict.
	if _, _, err := store.ResumeEntitlement(ctx, id, "resume-1", now); !isCode(err, CodePauseResumeConflict) {
		t.Fatalf("resume unpaused: %v", err)
	}

	// Boundary-valid pause: exactly at valid_to is allowed.
	pauseAt := to
	if _, _, err := store.PauseEntitlement(ctx, id, "pause-edge", pauseAt); err != nil {
		t.Fatalf("pause at valid_to: %v", err)
	}
	// Resume must be later than the pause and no later than valid_to.
	if _, _, err := store.ResumeEntitlement(ctx, id, "resume-late", to.Add(time.Second)); !isCode(err, CodeInvalidRequest) {
		t.Fatalf("resume after valid_to: %v", err)
	}
	if _, _, err := store.ResumeEntitlement(ctx, id, "resume-equal", pauseAt); !isCode(err, CodeInvalidRequest) {
		t.Fatalf("resume not later than pause: %v", err)
	}

	// None of the failed requests wrote a ledger row or changed pause state.
	view, _ := store.GetView(ctx, id)
	if len(view.PauseResume) != 1 || view.PauseResume[0].BusinessID != "pause-edge" {
		t.Fatalf("failed pause/resume left partial state: %+v", view.PauseResume)
	}
}

func TestPauseDoesNotSurviveExpiry(t *testing.T) {
	store := testStore(t)
	now := time.Now()
	id := uniqueID(t)
	mustCreate(t, store, id, 100, now.Add(-time.Hour), now.Add(time.Hour))
	ctx := context.Background()
	if _, _, err := store.PauseEntitlement(ctx, id, "pause-1", now.Add(-time.Minute)); err != nil {
		t.Fatalf("pause: %v", err)
	}
	// Once the window elapses, closure is terminal: a resume is rejected as
	// closed (and does not commit the closure), but a view closes normally.
	expireEntitlement(t, store, id)
	if _, _, err := store.ResumeEntitlement(ctx, id, "resume-1", now.Add(time.Minute)); !isCode(err, CodeEntitlementClosed) {
		t.Fatalf("resume after expiry: %v", err)
	}
	view, _ := store.GetView(ctx, id)
	if view.Status != StatusClosed || view.Snapshot == nil || len(view.PauseResume) != 1 {
		t.Fatalf("paused entitlement did not close correctly: %+v", view)
	}
}

func TestPauseResumeLedgerOrderedAndPersisted(t *testing.T) {
	store := testStore(t)
	now := time.Now()
	id := uniqueID(t)
	mustCreate(t, store, id, 100, now.Add(-time.Hour), now.Add(time.Hour))
	ctx := context.Background()
	events := []struct {
		biz, action string
		at          time.Time
	}{
		{"pause-1", ActionPause, now.Add(-30 * time.Minute)},
		{"resume-1", ActionResume, now.Add(-25 * time.Minute)},
		{"pause-2", ActionPause, now.Add(-20 * time.Minute)},
		{"resume-2", ActionResume, now.Add(-15 * time.Minute)},
	}
	for _, e := range events {
		var err error
		if e.action == ActionPause {
			_, _, err = store.PauseEntitlement(ctx, id, e.biz, e.at)
		} else {
			_, _, err = store.ResumeEntitlement(ctx, id, e.biz, e.at)
		}
		if err != nil {
			t.Fatalf("%s: %v", e.biz, err)
		}
	}
	view, _ := store.GetView(ctx, id)
	if len(view.PauseResume) != len(events) {
		t.Fatalf("ledger: %+v", view.PauseResume)
	}
	for i, e := range events {
		got := view.PauseResume[i]
		if got.BusinessID != e.biz || got.Type != e.action || !got.At.Equal(e.at) {
			t.Fatalf("ledger entry %d: want %+v got %+v", i, e, got)
		}
		if i > 0 && got.CreatedAt.Before(view.PauseResume[i-1].CreatedAt) {
			t.Fatalf("ledger not in registration order: %+v", view.PauseResume)
		}
	}

	// A fresh pool simulates a restart: closed state is open here, but the
	// full pause/resume ledger survives and the entitlement ends resumed.
	pool, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	defer pool.Close()
	restarted := NewStore(pool)
	rview, err := restarted.GetView(ctx, id)
	if err != nil || len(rview.PauseResume) != len(events) || rview.Status != "active" {
		t.Fatalf("ledger lost across restart: %+v err=%v", rview, err)
	}
	// Replays still return the originals after a restart.
	if _, created, err := restarted.PauseEntitlement(ctx, id, "pause-1", events[0].at); err != nil || created {
		t.Fatalf("pause replay after restart: created=%v err=%v", created, err)
	}
}

func TestClosureStateSurvivesReconnection(t *testing.T) {
	store := testStore(t)
	from, to := activeWindow()
	id := uniqueID(t)
	mustCreate(t, store, id, 100, from, to)
	ctx := context.Background()
	if _, _, err := store.CreateReservation(ctx, id, "res-1", "dept-1", 30); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	expireEntitlement(t, store, id)
	before, err := store.GetView(ctx, id)
	if err != nil || before.Status != StatusClosed || before.Snapshot == nil {
		t.Fatalf("close before restart: %+v err=%v", before, err)
	}

	pool, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	defer pool.Close()
	restarted := NewStore(pool)
	after, err := restarted.GetView(ctx, id)
	if err != nil {
		t.Fatalf("view after restart: %v", err)
	}
	if after.Status != StatusClosed || after.ClosedAt == nil || after.Snapshot == nil ||
		!after.ClosedAt.Equal(*before.ClosedAt) || snapshotCount(t, store, id) != 1 {
		t.Fatalf("closure state lost across restart: %+v", after)
	}
	if after.Snapshot.ReservedAmount != 30 || after.Snapshot.QuotaTotal != 100 {
		t.Fatalf("snapshot lost across restart: %+v", after.Snapshot)
	}
	// Still one-way and still rejecting issuance after the restart.
	if _, _, err := restarted.RescheduleEntitlement(ctx, id, "move-1", from, time.Now().Add(time.Hour)); !isCode(err, CodeEntitlementClosed) {
		t.Fatalf("reopen after restart: %v", err)
	}
	if _, _, err := restarted.CreateReservation(ctx, id, "res-2", "dept-1", 1); !isCode(err, CodeEntitlementClosed) {
		t.Fatalf("reserve after restart: %v", err)
	}
}

func TestConcurrentPauseResumeAndIssuance(t *testing.T) {
	store := testStore(t)
	now := time.Now()
	id := uniqueID(t)
	// A long window so the entitlement never auto-closes during the race.
	mustCreate(t, store, id, 10000, now.Add(-time.Hour), now.Add(24*time.Hour))
	ctx := context.Background()
	pauseAt := now.Add(-time.Minute)
	resumeAt := now

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers*4)

	// Phase 1: every worker submits and replays the SAME pause identifier;
	// exactly one pause takes effect, the rest are idempotent replays.
	pauseBarrier := make(chan struct{})
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-pauseBarrier
			for i := 0; i < 2; i++ {
				if _, _, err := store.PauseEntitlement(ctx, id, "pause-shared", pauseAt); err != nil {
					errs <- fmt.Errorf("pause: %w", err)
				}
			}
		}()
	}
	close(pauseBarrier)
	wg.Wait()

	// While paused, racing new reservations are all refused with the paused
	// code, never with an internal error.
	pausedRes := make(chan struct{})
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-pausedRes
			if _, _, err := store.CreateReservation(ctx, id, fmt.Sprintf("res-paused-%d", w), "dept-1", 1); err != nil {
				if !isCode(err, CodeEntitlementPaused) {
					errs <- fmt.Errorf("reserve while paused: %w", err)
				}
			} else {
				errs <- fmt.Errorf("reservation accepted while paused")
			}
		}(w)
	}
	close(pausedRes)
	wg.Wait()

	// Phase 2: every worker submits and replays the SAME resume identifier.
	resumeBarrier := make(chan struct{})
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-resumeBarrier
			for i := 0; i < 2; i++ {
				if _, _, err := store.ResumeEntitlement(ctx, id, "resume-shared", resumeAt); err != nil {
					errs <- fmt.Errorf("resume: %w", err)
				}
			}
		}()
	}
	close(resumeBarrier)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("unexpected concurrent failure: %v", err)
	}

	view, err := store.GetView(ctx, id)
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	// Exactly one pause and one resume record survived the concurrent storm.
	if len(view.PauseResume) != 2 ||
		view.PauseResume[0].BusinessID != "pause-shared" || view.PauseResume[0].Type != ActionPause ||
		view.PauseResume[1].BusinessID != "resume-shared" || view.PauseResume[1].Type != ActionResume {
		t.Fatalf("pause/resume ledger after concurrent storm: %+v", view.PauseResume)
	}
	if view.Status != "active" {
		t.Fatalf("entitlement should end active/resumed: %+v", view)
	}
}

func isCode(err error, code string) bool {
	var domainErr *Error
	return errors.As(err, &domainErr) && domainErr.Code == code
}

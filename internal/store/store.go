// Package store implements entitlements.Service on top of PostgreSQL.
//
// Serialization: every reserve/confirm/release transaction takes a row lock
// (SELECT ... FOR UPDATE) on the parent entitlement, so operations on the same
// entitlement apply strictly one after another. A trigger additionally guards
// the invariant used + reserved <= total as defence in depth. Any error rolls
// the transaction back, so a failed request leaves no partial state.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lansolocoder/cla03-entitlement-service/internal/entitlements"
)

// Store is a PostgreSQL backed entitlements.Service.
type Store struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// New creates a Store.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, now: time.Now}
}

// migrate holds the schema statements, kept here for documentation.
var migrateStatements = []string{
	`CREATE TABLE IF NOT EXISTS entitlements (
		id TEXT PRIMARY KEY,
		seat_total BIGINT NOT NULL CHECK (seat_total >= 0),
		credit_total BIGINT NOT NULL CHECK (credit_total >= 0),
		credit_used BIGINT NOT NULL DEFAULT 0 CHECK (credit_used >= 0),
		effective_from TIMESTAMPTZ NOT NULL,
		effective_to TIMESTAMPTZ NOT NULL,
		CHECK (effective_from < effective_to)
	)`,
	`CREATE TABLE IF NOT EXISTS reservations (
		entitlement_id TEXT NOT NULL
			REFERENCES entitlements(id) ON DELETE CASCADE,
		reservation_id TEXT NOT NULL,
		credit BIGINT NOT NULL CHECK (credit > 0),
		status TEXT NOT NULL CHECK (status IN ('reserved','confirmed','released')),
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		settled_at TIMESTAMPTZ,
		PRIMARY KEY (entitlement_id, reservation_id)
	)`,
	// Defence in depth: with all writers serialized by the entitlement row
	// lock this trigger must never fire; it makes the quota invariant
	// impossible to violate even for a buggy or out-of-band writer.
	`CREATE OR REPLACE FUNCTION entitlements_check_quota() RETURNS trigger AS $$
	BEGIN
		IF EXISTS (
			SELECT 1
			FROM entitlements e
			WHERE e.credit_used + COALESCE((
				SELECT sum(r.credit)
				FROM reservations r
				WHERE r.entitlement_id = e.id AND r.status = 'reserved'
			), 0) > e.credit_total
		) THEN
			RAISE EXCEPTION 'entitlement quota invariant violated: used + reserved exceeds total';
		END IF;
		RETURN NULL;
	END;
	$$ LANGUAGE plpgsql`,
	`DROP TRIGGER IF EXISTS entitlements_quota_guard ON entitlements`,
	`CREATE CONSTRAINT TRIGGER entitlements_quota_guard
		AFTER UPDATE OF credit_used ON entitlements
		DEFERRABLE INITIALLY DEFERRED
		FOR EACH ROW EXECUTE FUNCTION entitlements_check_quota()`,
	`DROP TRIGGER IF EXISTS reservations_quota_guard ON reservations`,
	`CREATE CONSTRAINT TRIGGER reservations_quota_guard
		AFTER INSERT OR UPDATE OF credit, status ON reservations
		DEFERRABLE INITIALLY DEFERRED
		FOR EACH ROW EXECUTE FUNCTION entitlements_check_quota()`,
}

// Migrate creates the schema if it does not exist. It is safe to call on every
// startup.
func (s *Store) Migrate(ctx context.Context) error {
	for _, stmt := range migrateStatements {
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

type entitlementRow struct {
	seatTotal     int64
	creditTotal   int64
	creditUsed    int64
	effectiveFrom time.Time
	effectiveTo   time.Time
}

const entitlementColumns = `SELECT seat_total, credit_total, credit_used, effective_from, effective_to
	FROM entitlements WHERE id = $1`

// CreateEntitlement validates input and inserts a new entitlement atomically.
func (s *Store) CreateEntitlement(ctx context.Context, in entitlements.EntitlementInput) (*entitlements.View, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx,
		`INSERT INTO entitlements (id, seat_total, credit_total, effective_from, effective_to)
		 VALUES ($1, $2, $3, $4, $5)`,
		in.ID, in.SeatTotal, in.CreditTotal, in.EffectiveFrom, in.EffectiveTo)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, entitlements.NewError(entitlements.CodeEntitlementExists,
				"entitlement "+in.ID+" already exists")
		}
		return nil, err
	}
	view, err := s.buildView(ctx, tx, in.ID, s.now())
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return view, nil
}

// EntitlementView returns a read-consistent snapshot projection.
func (s *Store) EntitlementView(ctx context.Context, entitlementID string) (*entitlements.View, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	view, err := s.buildView(ctx, tx, entitlementID, s.now())
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return view, nil
}

// Reserve creates a reservation under the entitlement's row lock. A repeated
// submission with the same business identifier and identical parameters
// replays the stored result; a parameter mismatch fails without changes.
func (s *Store) Reserve(ctx context.Context, entitlementID string, in entitlements.ReservationInput) (*entitlements.ReservationView, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// Lock the parent row first: reserves, confirms and releases on one
	// entitlement therefore apply strictly one after another.
	row, err := lockEntitlement(ctx, tx, entitlementID)
	if err != nil {
		return nil, err
	}

	// Idempotency check runs before the validity-window check: a resend of an
	// already recorded reservation replays even after expiry, while a new
	// reservation after expiry is rejected.
	var existingCredit int64
	var existingStatus string
	err = tx.QueryRow(ctx,
		`SELECT credit, status FROM reservations
		 WHERE entitlement_id = $1 AND reservation_id = $2`,
		entitlementID, in.ID).Scan(&existingCredit, &existingStatus)
	switch {
	case err == nil:
		if existingCredit != in.Credit {
			return nil, entitlements.NewError(entitlements.CodeIdempotencyMismatch,
				"reservation "+in.ID+" already exists with a different credit amount")
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return reservationFromStore(in.ID, existingCredit, existingStatus), nil
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, err
	}

	now := s.now()
	switch entitlements.StatusAt(row.effectiveFrom, row.effectiveTo, now) {
	case entitlements.StatusPending:
		return nil, entitlements.NewError(entitlements.CodeNotEffective,
			"entitlement "+entitlementID+" is not effective yet")
	case entitlements.StatusExpired:
		return nil, entitlements.NewError(entitlements.CodeExpired,
			"entitlement "+entitlementID+" has expired")
	}

	var reservedSum int64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(sum(credit), 0) FROM reservations
		 WHERE entitlement_id = $1 AND status = 'reserved'`,
		entitlementID).Scan(&reservedSum); err != nil {
		return nil, err
	}
	if row.creditTotal-row.creditUsed-reservedSum < in.Credit {
		return nil, entitlements.NewError(entitlements.CodeInsufficientCredit,
			"insufficient available credit for reservation "+in.ID)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO reservations (entitlement_id, reservation_id, credit, status)
		 VALUES ($1, $2, $3, 'reserved')`,
		entitlementID, in.ID, in.Credit); err != nil {
		if isUniqueViolation(err) {
			// Unreachable while the entitlement row lock is held, kept for
			// completeness: another writer that bypassed the lock raced us.
			return nil, entitlements.NewError(entitlements.CodeIdempotencyMismatch,
				"reservation "+in.ID+" already exists")
		}
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return reservationFromStore(in.ID, in.Credit, string(entitlements.ReservationReserved)), nil
}

// settle implements the shared confirm/release path.
func (s *Store) settle(ctx context.Context, entitlementID, reservationID string, outcome entitlements.ReservationStatus) (*entitlements.ReservationView, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// Locking the parent serializes settlement against concurrent reserves and
	// settlements; the reservation lock then pins the single reservation row.
	if _, err := lockEntitlement(ctx, tx, entitlementID); err != nil {
		return nil, err
	}
	var credit int64
	var status string
	err = tx.QueryRow(ctx,
		`SELECT credit, status FROM reservations
		 WHERE entitlement_id = $1 AND reservation_id = $2 FOR UPDATE`,
		entitlementID, reservationID).Scan(&credit, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, entitlements.NewError(entitlements.CodeReservationNotFound,
			"reservation "+reservationID+" not found")
	}
	if err != nil {
		return nil, err
	}
	if status != string(entitlements.ReservationReserved) {
		return nil, entitlements.NewError(entitlements.CodeAlreadySettled,
			"reservation "+reservationID+" is already "+status)
	}

	// Flip the reservation first, then adjust used credit. The quota guard
	// triggers are deferred to commit, so the check runs only against the
	// transaction's final, consistent state.
	if _, err := tx.Exec(ctx,
		`UPDATE reservations SET status = $3, settled_at = now()
		 WHERE entitlement_id = $1 AND reservation_id = $2`,
		entitlementID, reservationID, string(outcome)); err != nil {
		return nil, err
	}
	if outcome == entitlements.ReservationConfirmed {
		if _, err := tx.Exec(ctx,
			`UPDATE entitlements SET credit_used = credit_used + $2 WHERE id = $1`,
			entitlementID, credit); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return reservationFromStore(reservationID, credit, string(outcome)), nil
}

// Confirm converts reserved credit into used credit. Expiry does not block it.
func (s *Store) Confirm(ctx context.Context, entitlementID, reservationID string) (*entitlements.ReservationView, error) {
	return s.settle(ctx, entitlementID, reservationID, entitlements.ReservationConfirmed)
}

// Release returns reserved credit to the available pool. Expiry does not block it.
func (s *Store) Release(ctx context.Context, entitlementID, reservationID string) (*entitlements.ReservationView, error) {
	return s.settle(ctx, entitlementID, reservationID, entitlements.ReservationReleased)
}

func lockEntitlement(ctx context.Context, tx pgx.Tx, entitlementID string) (entitlementRow, error) {
	var row entitlementRow
	err := tx.QueryRow(ctx,
		`SELECT seat_total, credit_total, credit_used, effective_from, effective_to
		 FROM entitlements WHERE id = $1 FOR UPDATE`,
		entitlementID).Scan(&row.seatTotal, &row.creditTotal, &row.creditUsed,
		&row.effectiveFrom, &row.effectiveTo)
	if errors.Is(err, pgx.ErrNoRows) {
		return row, entitlements.NewError(entitlements.CodeEntitlementNotFound,
			"entitlement "+entitlementID+" not found")
	}
	return row, err
}

func (s *Store) buildView(ctx context.Context, q querier, entitlementID string, now time.Time) (*entitlements.View, error) {
	var row entitlementRow
	err := q.QueryRow(ctx,
		entitlementColumns, entitlementID).Scan(&row.seatTotal, &row.creditTotal,
		&row.creditUsed, &row.effectiveFrom, &row.effectiveTo)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, entitlements.NewError(entitlements.CodeEntitlementNotFound,
			"entitlement "+entitlementID+" not found")
	}
	if err != nil {
		return nil, err
	}

	view := &entitlements.View{
		EntitlementID: entitlementID,
		SeatTotal:     row.seatTotal,
		CreditTotal:   row.creditTotal,
		CreditUsed:    row.creditUsed,
		Status:        entitlements.StatusAt(row.effectiveFrom, row.effectiveTo, now),
		EffectiveFrom: row.effectiveFrom,
		EffectiveTo:   row.effectiveTo,
		Reservations:  []entitlements.ReservationView{},
	}

	rows, err := q.Query(ctx,
		`SELECT reservation_id, credit, status
		 FROM reservations WHERE entitlement_id = $1
		 ORDER BY created_at, reservation_id`, entitlementID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, status string
		var credit int64
		if err := rows.Scan(&id, &credit, &status); err != nil {
			return nil, err
		}
		view.Reservations = append(view.Reservations, *reservationFromStore(id, credit, status))
		if status == string(entitlements.ReservationReserved) {
			view.CreditReserved += credit
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	view.CreditAvailable = row.creditTotal - row.creditUsed - view.CreditReserved
	return view, nil
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func reservationFromStore(id string, credit int64, status string) *entitlements.ReservationView {
	current := entitlements.ReservationStatus(status)
	r := &entitlements.ReservationView{
		ReservationID: id,
		Credit:        credit,
		Status:        current,
	}
	if current != entitlements.ReservationReserved {
		result := current
		r.Result = &result
	}
	return r
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

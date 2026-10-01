// Package entitlements implements the seat-quota entitlement domain:
// entitlement creation, idempotent reservations, confirm/release
// settlement, and corrective reversals of confirmed reservations,
// persisted in PostgreSQL.
package entitlements

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Error codes returned to API callers. They are stable identifiers; clients
// should branch on Code, not on Message.
const (
	CodeInvalidRequest          = "invalid_request"
	CodeEntitlementExists       = "entitlement_exists"
	CodeEntitlementNotFound     = "entitlement_not_found"
	CodeEntitlementNotActive    = "entitlement_not_active"
	CodeInsufficientQuota       = "insufficient_quota"
	CodeReservationNotFound     = "reservation_not_found"
	CodeReservationParamChanged = "reservation_param_mismatch"
	CodeReservationSettled      = "reservation_already_settled"
	CodeReservationNotConfirmed = "reservation_not_confirmed"
	CodeReservationReversed     = "reservation_already_reversed"
)

// Error is a domain failure with a stable machine-readable code.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func fail(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Reservation lifecycle states.
const (
	StatusPending   = "pending"
	StatusConfirmed = "confirmed"
	StatusReleased  = "released"
	StatusReversed  = "reversed"
)

// Settlement actions.
const (
	ActionConfirm = "confirm"
	ActionRelease = "release"
)

// Entitlement is a team's seat-quota grant over a validity window.
type Entitlement struct {
	EntitlementID string    `json:"entitlement_id"`
	SeatsTotal    int64     `json:"seats_total"`
	QuotaTotal    int64     `json:"quota_total"`
	ValidFrom     time.Time `json:"valid_from"`
	ValidTo       time.Time `json:"valid_to"`
}

// Reservation is a quota hold placed by an internal sub-team.
type Reservation struct {
	ReservationID string     `json:"reservation_id"`
	Amount        int64      `json:"amount"`
	Status        string     `json:"status"`
	SettledAt     *time.Time `json:"settled_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

// Reversal is a corrective record that refunds a confirmed reservation's
// amount from used quota back to available quota. It is identified by the
// caller-supplied correction identifier, unique within the entitlement, and
// never alters the original reservation it refunds.
type Reversal struct {
	ReversalID    string     `json:"reversal_id"`
	ReservationID string     `json:"reservation_id"`
	Amount        int64      `json:"amount"`
	Status        string     `json:"status"`
	SettledAt     *time.Time `json:"settled_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

// View is the per-entitlement quota and reservation report. Reservations
// lists both reservations and corrective reversals; a reversal entry carries
// its own correction identifier in ReservationID and status "reversed".
// UsedAmount is confirmed quota minus effective reversals.
type View struct {
	EntitlementID   string        `json:"entitlement_id"`
	SeatsTotal      int64         `json:"seats_total"`
	QuotaTotal      int64         `json:"quota_total"`
	UsedAmount      int64         `json:"used_amount"`
	ReservedAmount  int64         `json:"reserved_amount"`
	AvailableAmount int64         `json:"available_amount"`
	Status          string        `json:"status"` // pending, active or expired
	ValidFrom       time.Time     `json:"valid_from"`
	ValidTo         time.Time     `json:"valid_to"`
	Reservations    []Reservation `json:"reservations"`
}

var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func validIdentifier(kind, value string) *Error {
	if !identifierPattern.MatchString(value) {
		return fail(CodeInvalidRequest, "%s must be 1-128 characters of letters, digits, dot, underscore or dash, and start with a letter or digit", kind)
	}
	return nil
}

// Store persists entitlements and reservations in PostgreSQL. All mutating
// operations take a row lock on the entitlement so concurrent reservations
// and settlements on the same entitlement are serialized.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Migrate creates the schema if it does not exist yet.
func (s *Store) Migrate(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS entitlements (
    entitlement_id TEXT PRIMARY KEY,
    seats_total    BIGINT NOT NULL CHECK (seats_total > 0),
    quota_total    BIGINT NOT NULL CHECK (quota_total > 0),
    used_amount    BIGINT NOT NULL DEFAULT 0 CHECK (used_amount >= 0),
    valid_from     TIMESTAMPTZ NOT NULL,
    valid_to       TIMESTAMPTZ NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS reservations (
    entitlement_id TEXT NOT NULL REFERENCES entitlements (entitlement_id),
    reservation_id TEXT NOT NULL,
    amount         BIGINT NOT NULL CHECK (amount > 0),
    status         TEXT NOT NULL DEFAULT 'pending'
                   CHECK (status IN ('pending', 'confirmed', 'released')),
    settled_at     TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (entitlement_id, reservation_id)
);
CREATE TABLE IF NOT EXISTS reversals (
    entitlement_id TEXT NOT NULL REFERENCES entitlements (entitlement_id),
    reversal_id    TEXT NOT NULL,
    reservation_id TEXT NOT NULL,
    amount         BIGINT NOT NULL CHECK (amount > 0),
    settled_at     TIMESTAMPTZ NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (entitlement_id, reversal_id),
    UNIQUE (entitlement_id, reservation_id)
);
-- Databases initialized by an older schema may already have a reversals
-- table without the settlement timestamp.
ALTER TABLE reversals ADD COLUMN IF NOT EXISTS settled_at TIMESTAMPTZ;`)
	if err != nil {
		return fmt.Errorf("migrate schema: %w", err)
	}
	return nil
}

// CreateEntitlementInput carries the raw creation parameters.
type CreateEntitlementInput struct {
	EntitlementID string
	SeatsTotal    *int64
	QuotaTotal    *int64
	ValidFrom     time.Time
	ValidTo       time.Time
}

// CreateEntitlement validates and stores a new entitlement. Invalid input
// fails before anything is written.
func (s *Store) CreateEntitlement(ctx context.Context, in CreateEntitlementInput) (Entitlement, error) {
	if err := validIdentifier("entitlement_id", in.EntitlementID); err != nil {
		return Entitlement{}, err
	}
	if in.QuotaTotal == nil || *in.QuotaTotal <= 0 {
		return Entitlement{}, fail(CodeInvalidRequest, "quota_total is required and must be a positive integer")
	}
	seats := *in.QuotaTotal
	if in.SeatsTotal != nil {
		if *in.SeatsTotal <= 0 {
			return Entitlement{}, fail(CodeInvalidRequest, "seats_total must be a positive integer")
		}
		seats = *in.SeatsTotal
	}
	if in.ValidFrom.IsZero() || in.ValidTo.IsZero() {
		return Entitlement{}, fail(CodeInvalidRequest, "valid_from and valid_to are required (RFC 3339 timestamps)")
	}
	if !in.ValidFrom.Before(in.ValidTo) {
		return Entitlement{}, fail(CodeInvalidRequest, "valid_from must be before valid_to")
	}
	ent := Entitlement{
		EntitlementID: in.EntitlementID,
		SeatsTotal:    seats,
		QuotaTotal:    *in.QuotaTotal,
		ValidFrom:     in.ValidFrom.UTC(),
		ValidTo:       in.ValidTo.UTC(),
	}
	_, err := s.pool.Exec(ctx, `
INSERT INTO entitlements (entitlement_id, seats_total, quota_total, valid_from, valid_to)
VALUES ($1, $2, $3, $4, $5)`,
		ent.EntitlementID, ent.SeatsTotal, ent.QuotaTotal, ent.ValidFrom, ent.ValidTo)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Entitlement{}, fail(CodeEntitlementExists, "entitlement %q already exists", ent.EntitlementID)
		}
		return Entitlement{}, fmt.Errorf("insert entitlement: %w", err)
	}
	return ent, nil
}

// entitlementRow is the locked state shared by the mutating flows.
type entitlementRow struct {
	quotaTotal int64
	usedAmount int64
	validFrom  time.Time
	validTo    time.Time
	now        time.Time
}

// lockEntitlement takes the per-entitlement serialization lock inside tx.
func lockEntitlement(ctx context.Context, tx pgx.Tx, entitlementID string) (entitlementRow, error) {
	var row entitlementRow
	err := tx.QueryRow(ctx, `
SELECT quota_total, used_amount, valid_from, valid_to, now()
FROM entitlements WHERE entitlement_id = $1 FOR UPDATE`, entitlementID).
		Scan(&row.quotaTotal, &row.usedAmount, &row.validFrom, &row.validTo, &row.now)
	if errors.Is(err, pgx.ErrNoRows) {
		return entitlementRow{}, fail(CodeEntitlementNotFound, "entitlement %q does not exist", entitlementID)
	}
	if err != nil {
		return entitlementRow{}, fmt.Errorf("lock entitlement: %w", err)
	}
	return row, nil
}

func pendingAmount(ctx context.Context, tx pgx.Tx, entitlementID string) (int64, error) {
	var pending int64
	err := tx.QueryRow(ctx, `
SELECT COALESCE(SUM(amount), 0) FROM reservations
WHERE entitlement_id = $1 AND status = 'pending'`, entitlementID).Scan(&pending)
	if err != nil {
		return 0, fmt.Errorf("sum pending reservations: %w", err)
	}
	return pending, nil
}

// CreateReservation places a quota hold. The caller-supplied reservationID is
// unique per entitlement: resubmitting it with the same amount returns the
// original reservation without consuming quota again (created=false);
// resubmitting it with a different amount fails without changing state.
func (s *Store) CreateReservation(ctx context.Context, entitlementID, reservationID string, amount int64) (Reservation, bool, error) {
	if err := validIdentifier("reservation_id", reservationID); err != nil {
		return Reservation{}, false, err
	}
	if amount <= 0 {
		return Reservation{}, false, fail(CodeInvalidRequest, "amount is required and must be a positive integer")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Reservation{}, false, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ent, err := lockEntitlement(ctx, tx, entitlementID)
	if err != nil {
		return Reservation{}, false, err
	}

	var existing Reservation
	err = tx.QueryRow(ctx, `
SELECT reservation_id, amount, status, settled_at, created_at
FROM reservations WHERE entitlement_id = $1 AND reservation_id = $2`,
		entitlementID, reservationID).
		Scan(&existing.ReservationID, &existing.Amount, &existing.Status, &existing.SettledAt, &existing.CreatedAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Reservation{}, false, fmt.Errorf("load reservation: %w", err)
	}
	if err == nil {
		if existing.Amount != amount {
			return Reservation{}, false, fail(CodeReservationParamChanged,
				"reservation %q already exists with amount %d, not %d", reservationID, existing.Amount, amount)
		}
		return existing, false, tx.Commit(ctx)
	}

	if ent.now.Before(ent.validFrom) || !ent.now.Before(ent.validTo) {
		return Reservation{}, false, fail(CodeEntitlementNotActive,
			"entitlement %q is not in its validity window", entitlementID)
	}
	pending, err := pendingAmount(ctx, tx, entitlementID)
	if err != nil {
		return Reservation{}, false, err
	}
	if ent.usedAmount+pending+amount > ent.quotaTotal {
		return Reservation{}, false, fail(CodeInsufficientQuota,
			"requested %d but only %d of %d quota available", amount, ent.quotaTotal-ent.usedAmount-pending, ent.quotaTotal)
	}
	var created Reservation
	err = tx.QueryRow(ctx, `
INSERT INTO reservations (entitlement_id, reservation_id, amount)
VALUES ($1, $2, $3)
RETURNING reservation_id, amount, status, settled_at, created_at`,
		entitlementID, reservationID, amount).
		Scan(&created.ReservationID, &created.Amount, &created.Status, &created.SettledAt, &created.CreatedAt)
	if err != nil {
		return Reservation{}, false, fmt.Errorf("insert reservation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Reservation{}, false, fmt.Errorf("commit reservation: %w", err)
	}
	return created, true, nil
}

// SettleReservation settles a pending reservation exactly once: confirm moves
// its amount to used quota, release returns it to available quota. Any
// further settlement attempt fails without effect.
func (s *Store) SettleReservation(ctx context.Context, entitlementID, reservationID, action string) (Reservation, error) {
	if action != ActionConfirm && action != ActionRelease {
		return Reservation{}, fail(CodeInvalidRequest, "action must be %q or %q", ActionConfirm, ActionRelease)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Reservation{}, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := lockEntitlement(ctx, tx, entitlementID); err != nil {
		return Reservation{}, err
	}
	var reservation Reservation
	err = tx.QueryRow(ctx, `
SELECT reservation_id, amount, status, settled_at, created_at
FROM reservations WHERE entitlement_id = $1 AND reservation_id = $2`,
		entitlementID, reservationID).
		Scan(&reservation.ReservationID, &reservation.Amount, &reservation.Status, &reservation.SettledAt, &reservation.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Reservation{}, fail(CodeReservationNotFound,
			"reservation %q does not exist on entitlement %q", reservationID, entitlementID)
	}
	if err != nil {
		return Reservation{}, fmt.Errorf("load reservation: %w", err)
	}
	if reservation.Status != StatusPending {
		return Reservation{}, fail(CodeReservationSettled,
			"reservation %q is already %s", reservationID, reservation.Status)
	}

	newStatus := StatusReleased
	if action == ActionConfirm {
		newStatus = StatusConfirmed
		if _, err := tx.Exec(ctx, `
UPDATE entitlements SET used_amount = used_amount + $1 WHERE entitlement_id = $2`,
			reservation.Amount, entitlementID); err != nil {
			return Reservation{}, fmt.Errorf("consume quota: %w", err)
		}
	}
	err = tx.QueryRow(ctx, `
UPDATE reservations SET status = $1, settled_at = now()
WHERE entitlement_id = $2 AND reservation_id = $3
RETURNING reservation_id, amount, status, settled_at, created_at`,
		newStatus, entitlementID, reservationID).
		Scan(&reservation.ReservationID, &reservation.Amount, &reservation.Status, &reservation.SettledAt, &reservation.CreatedAt)
	if err != nil {
		return Reservation{}, fmt.Errorf("settle reservation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Reservation{}, fmt.Errorf("commit settlement: %w", err)
	}
	return reservation, nil
}

// ReverseReservation refunds a confirmed reservation: it records a reversal
// with the reservation's original amount and moves that amount from used
// quota back to available quota. The caller-supplied reversalID identifies
// the correction and is unique per entitlement: resubmitting it for the same
// reservation returns the original reversal without refunding again
// (created=false); resubmitting it for a different reservation fails without
// changing state. The amount always comes from the confirmed reservation and
// cannot be specified by the caller. A reservation can be reversed at most
// once, and only while confirmed; the original reservation row is left
// untouched. Reversal stays allowed after the entitlement expires.
func (s *Store) ReverseReservation(ctx context.Context, entitlementID, reservationID, reversalID string) (Reversal, bool, error) {
	if err := validIdentifier("reversal_id", reversalID); err != nil {
		return Reversal{}, false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Reversal{}, false, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := lockEntitlement(ctx, tx, entitlementID); err != nil {
		return Reversal{}, false, err
	}

	var existing Reversal
	err = tx.QueryRow(ctx, `
SELECT reversal_id, reservation_id, amount, settled_at, created_at
FROM reversals WHERE entitlement_id = $1 AND reversal_id = $2`,
		entitlementID, reversalID).
		Scan(&existing.ReversalID, &existing.ReservationID, &existing.Amount, &existing.SettledAt, &existing.CreatedAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Reversal{}, false, fmt.Errorf("load reversal: %w", err)
	}
	if err == nil {
		if existing.ReservationID != reservationID {
			return Reversal{}, false, fail(CodeReservationParamChanged,
				"reversal %q already targets reservation %q, not %q", reversalID, existing.ReservationID, reservationID)
		}
		existing.Status = StatusReversed
		return existing, false, tx.Commit(ctx)
	}

	var reservation Reservation
	err = tx.QueryRow(ctx, `
SELECT reservation_id, amount, status, settled_at, created_at
FROM reservations WHERE entitlement_id = $1 AND reservation_id = $2`,
		entitlementID, reservationID).
		Scan(&reservation.ReservationID, &reservation.Amount, &reservation.Status, &reservation.SettledAt, &reservation.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Reversal{}, false, fail(CodeReservationNotFound,
			"reservation %q does not exist on entitlement %q", reservationID, entitlementID)
	}
	if err != nil {
		return Reversal{}, false, fmt.Errorf("load reservation: %w", err)
	}
	if reservation.Status != StatusConfirmed {
		return Reversal{}, false, fail(CodeReservationNotConfirmed,
			"reservation %q is %s, only a confirmed reservation can be reversed", reservationID, reservation.Status)
	}
	var alreadyReversed bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS(SELECT 1 FROM reversals WHERE entitlement_id = $1 AND reservation_id = $2)`,
		entitlementID, reservationID).Scan(&alreadyReversed); err != nil {
		return Reversal{}, false, fmt.Errorf("check existing reversal: %w", err)
	}
	if alreadyReversed {
		return Reversal{}, false, fail(CodeReservationReversed,
			"reservation %q has already been reversed", reservationID)
	}

	var created Reversal
	err = tx.QueryRow(ctx, `
INSERT INTO reversals (entitlement_id, reversal_id, reservation_id, amount, settled_at)
VALUES ($1, $2, $3, $4, now())
RETURNING reversal_id, reservation_id, amount, settled_at, created_at`,
		entitlementID, reversalID, reservationID, reservation.Amount).
		Scan(&created.ReversalID, &created.ReservationID, &created.Amount, &created.SettledAt, &created.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Reversal{}, false, fail(CodeReservationReversed,
				"reservation %q has already been reversed", reservationID)
		}
		return Reversal{}, false, fmt.Errorf("insert reversal: %w", err)
	}
	created.Status = StatusReversed
	if _, err := tx.Exec(ctx, `
UPDATE entitlements SET used_amount = used_amount - $1 WHERE entitlement_id = $2`,
		created.Amount, entitlementID); err != nil {
		return Reversal{}, false, fmt.Errorf("refund quota: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Reversal{}, false, fmt.Errorf("commit reversal: %w", err)
	}
	return created, true, nil
}

// GetView returns the quota breakdown and reservation list for an
// entitlement, consistent with the committed state.
func (s *Store) GetView(ctx context.Context, entitlementID string) (View, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return View{}, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var view View
	var now time.Time
	err = tx.QueryRow(ctx, `
SELECT seats_total, quota_total, used_amount, valid_from, valid_to, now()
FROM entitlements WHERE entitlement_id = $1`, entitlementID).
		Scan(&view.SeatsTotal, &view.QuotaTotal, &view.UsedAmount, &view.ValidFrom, &view.ValidTo, &now)
	if errors.Is(err, pgx.ErrNoRows) {
		return View{}, fail(CodeEntitlementNotFound, "entitlement %q does not exist", entitlementID)
	}
	if err != nil {
		return View{}, fmt.Errorf("load entitlement: %w", err)
	}
	view.EntitlementID = entitlementID
	switch {
	case now.Before(view.ValidFrom):
		view.Status = "pending"
	case now.Before(view.ValidTo):
		view.Status = "active"
	default:
		view.Status = "expired"
	}
	view.ReservedAmount, err = pendingAmount(ctx, tx, entitlementID)
	if err != nil {
		return View{}, err
	}
	view.AvailableAmount = view.QuotaTotal - view.UsedAmount - view.ReservedAmount

	rows, err := tx.Query(ctx, `
SELECT reservation_id, amount, status, settled_at, created_at
FROM (
    SELECT reservation_id, amount, status, settled_at, created_at
    FROM reservations WHERE entitlement_id = $1
    UNION ALL
    SELECT reversal_id, amount, 'reversed', settled_at, created_at
    FROM reversals WHERE entitlement_id = $1
) AS entries
ORDER BY created_at, reservation_id`, entitlementID)
	if err != nil {
		return View{}, fmt.Errorf("list reservations: %w", err)
	}
	defer rows.Close()
	view.Reservations = []Reservation{}
	for rows.Next() {
		var r Reservation
		if err := rows.Scan(&r.ReservationID, &r.Amount, &r.Status, &r.SettledAt, &r.CreatedAt); err != nil {
			return View{}, fmt.Errorf("scan reservation: %w", err)
		}
		view.Reservations = append(view.Reservations, r)
	}
	if err := rows.Err(); err != nil {
		return View{}, fmt.Errorf("list reservations: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return View{}, fmt.Errorf("commit view: %w", err)
	}
	return view, nil
}

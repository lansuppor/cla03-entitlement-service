// Package entitlements implements the seat-quota entitlement domain:
// entitlement creation, idempotent reservations, confirm/release
// settlement, corrective reversals of confirmed reservations, scheduled
// quota-total adjustments, and rescheduling of the validity window,
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
	CodeAdjustmentParamChanged  = "adjustment_param_mismatch"
	CodeRescheduleParamChanged  = "reschedule_param_mismatch"
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

// Reservation lifecycle states. StatusPending is also the state of an
// adjustment that has not been folded into the quota total yet.
const (
	StatusPending   = "pending"
	StatusConfirmed = "confirmed"
	StatusReleased  = "released"
	StatusReversed  = "reversed"
	StatusApplied   = "applied"
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

// Adjustment is a caller-initiated change of the quota total, identified by
// the caller-supplied adjustment identifier, unique within the entitlement.
// Delta is a non-zero integer: positive grows the quota total, negative
// shrinks it. The adjustment is folded into the quota total once its
// effective time has arrived — an increase always applies, a decrease only
// when the adjusted total still covers used plus unsettled quota. Status is
// "pending" until applied, then "applied".
type Adjustment struct {
	AdjustmentID string     `json:"adjustment_id"`
	Delta        int64      `json:"delta"`
	EffectiveAt  time.Time  `json:"effective_at"`
	Status       string     `json:"status"`
	AppliedAt    *time.Time `json:"applied_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

// Reschedule is a caller-initiated rewrite of an entitlement's validity
// window, identified by the caller-supplied reschedule identifier, unique
// within the entitlement. The first submission records the reschedule and
// immediately replaces the entitlement's valid_from and valid_to with the
// new values; it never changes any quota figure. Resubmitting the same
// identifier with the same window returns the original record without
// touching the times again.
type Reschedule struct {
	RescheduleID string    `json:"reschedule_id"`
	NewValidFrom time.Time `json:"new_valid_from"`
	NewValidTo   time.Time `json:"new_valid_to"`
	CreatedAt    time.Time `json:"created_at"`
}

// adjustmentStatus derives the lifecycle state from the application
// timestamp.
func adjustmentStatus(appliedAt *time.Time) string {
	if appliedAt != nil {
		return StatusApplied
	}
	return StatusPending
}

// View is the per-entitlement quota and reservation report. Reservations
// lists both reservations and corrective reversals; a reversal entry carries
// its own correction identifier in ReservationID and status "reversed".
// UsedAmount is confirmed quota minus effective reversals. Adjustments lists
// every quota adjustment with its effective time and whether it has been
// folded into QuotaTotal; the quota figures only reflect applied adjustments.
// Reschedules lists every validity-window rewrite in creation order; the
// reported ValidFrom/ValidTo and Status always reflect the latest one.
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
	Adjustments     []Adjustment  `json:"adjustments"`
	Reschedules     []Reschedule  `json:"reschedules"`
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
ALTER TABLE reversals ADD COLUMN IF NOT EXISTS settled_at TIMESTAMPTZ;
CREATE TABLE IF NOT EXISTS adjustments (
    entitlement_id TEXT NOT NULL REFERENCES entitlements (entitlement_id),
    adjustment_id  TEXT NOT NULL,
    delta          BIGINT NOT NULL CHECK (delta <> 0),
    effective_at   TIMESTAMPTZ NOT NULL,
    applied_at     TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (entitlement_id, adjustment_id)
);
CREATE TABLE IF NOT EXISTS reschedules (
    entitlement_id TEXT NOT NULL REFERENCES entitlements (entitlement_id),
    reschedule_id  TEXT NOT NULL,
    new_valid_from TIMESTAMPTZ NOT NULL,
    new_valid_to   TIMESTAMPTZ NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (entitlement_id, reschedule_id),
    CHECK (new_valid_from < new_valid_to)
);`)
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
	seatsTotal int64
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
SELECT seats_total, quota_total, used_amount, valid_from, valid_to, now()
FROM entitlements WHERE entitlement_id = $1 FOR UPDATE`, entitlementID).
		Scan(&row.seatsTotal, &row.quotaTotal, &row.usedAmount, &row.validFrom, &row.validTo, &row.now)
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

// applyDueAdjustments folds every adjustment whose effective time has arrived
// into the entitlement's quota total, each exactly once. Increases always
// apply. A decrease applies only when the adjusted total stays positive and
// still covers used plus unsettled quota; otherwise it stays pending and is
// retried by the next operation on the entitlement. It runs under the
// per-entitlement row lock, so concurrent operations, repeated requests and
// restarts can never apply an adjustment twice, and ent.quotaTotal is left
// holding the current total.
func applyDueAdjustments(ctx context.Context, tx pgx.Tx, ent *entitlementRow, entitlementID string) error {
	rows, err := tx.Query(ctx, `
SELECT adjustment_id, delta FROM adjustments
WHERE entitlement_id = $1 AND applied_at IS NULL AND effective_at <= $2
ORDER BY effective_at, created_at, adjustment_id`, entitlementID, ent.now)
	if err != nil {
		return fmt.Errorf("list due adjustments: %w", err)
	}
	type dueAdjustment struct {
		id    string
		delta int64
	}
	var due []dueAdjustment
	for rows.Next() {
		var d dueAdjustment
		if err := rows.Scan(&d.id, &d.delta); err != nil {
			rows.Close()
			return fmt.Errorf("scan due adjustment: %w", err)
		}
		due = append(due, d)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("list due adjustments: %w", err)
	}
	rows.Close()
	if len(due) == 0 {
		return nil
	}
	// Occupancy does not change while adjustments are folded in, so one
	// pending sum serves every feasibility check below.
	pending, err := pendingAmount(ctx, tx, entitlementID)
	if err != nil {
		return err
	}
	for _, d := range due {
		newTotal := ent.quotaTotal + d.delta
		if d.delta < 0 && (newTotal <= 0 || ent.usedAmount+pending > newTotal) {
			// The decrease would crowd out occupied quota (or drop the
			// total to zero or below): leave it pending.
			continue
		}
		if _, err := tx.Exec(ctx, `
UPDATE adjustments SET applied_at = now()
WHERE entitlement_id = $1 AND adjustment_id = $2 AND applied_at IS NULL`,
			entitlementID, d.id); err != nil {
			return fmt.Errorf("mark adjustment applied: %w", err)
		}
		if _, err := tx.Exec(ctx, `
UPDATE entitlements SET quota_total = quota_total + $1 WHERE entitlement_id = $2`,
			d.delta, entitlementID); err != nil {
			return fmt.Errorf("adjust quota total: %w", err)
		}
		ent.quotaTotal = newTotal
	}
	return nil
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
	if err := applyDueAdjustments(ctx, tx, &ent, entitlementID); err != nil {
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

	ent, err := lockEntitlement(ctx, tx, entitlementID)
	if err != nil {
		return Reservation{}, err
	}
	if err := applyDueAdjustments(ctx, tx, &ent, entitlementID); err != nil {
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
		ent.usedAmount += reservation.Amount
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
	// A release frees occupied quota, which may let a blocked decrease take
	// effect right away.
	if err := applyDueAdjustments(ctx, tx, &ent, entitlementID); err != nil {
		return Reservation{}, err
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

	ent, err := lockEntitlement(ctx, tx, entitlementID)
	if err != nil {
		return Reversal{}, false, err
	}
	if err := applyDueAdjustments(ctx, tx, &ent, entitlementID); err != nil {
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
	// The refund lowers used quota, which may let a blocked decrease take
	// effect right away.
	ent.usedAmount -= created.Amount
	if err := applyDueAdjustments(ctx, tx, &ent, entitlementID); err != nil {
		return Reversal{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Reversal{}, false, fmt.Errorf("commit reversal: %w", err)
	}
	return created, true, nil
}

// CreateAdjustment registers a quota adjustment against an entitlement. The
// caller-supplied adjustmentID is unique per entitlement: resubmitting it
// with the same delta and effective time returns the original adjustment
// without touching quota again (created=false); resubmitting it with a
// different delta or effective time fails without changing state. The
// adjustment is recorded as pending and folds into the quota total once its
// effective time has arrived — immediately when the effective time is not in
// the future. An increase always applies; a decrease applies only while the
// adjusted total still covers used plus unsettled quota, otherwise it stays
// pending and takes effect automatically once occupancy allows. A decrease
// that would drop the current quota total to zero or below fails and writes
// nothing.
func (s *Store) CreateAdjustment(ctx context.Context, entitlementID, adjustmentID string, delta int64, effectiveAt time.Time) (Adjustment, bool, error) {
	if err := validIdentifier("adjustment_id", adjustmentID); err != nil {
		return Adjustment{}, false, err
	}
	if delta == 0 {
		return Adjustment{}, false, fail(CodeInvalidRequest, "delta is required and must be a non-zero integer")
	}
	if effectiveAt.IsZero() {
		return Adjustment{}, false, fail(CodeInvalidRequest, "effective_at is required (an RFC 3339 timestamp)")
	}
	effectiveAt = effectiveAt.UTC()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Adjustment{}, false, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ent, err := lockEntitlement(ctx, tx, entitlementID)
	if err != nil {
		return Adjustment{}, false, err
	}
	// Fold in adjustments that came due earlier so a new decrease is checked
	// against the current quota total.
	if err := applyDueAdjustments(ctx, tx, &ent, entitlementID); err != nil {
		return Adjustment{}, false, err
	}

	var existing Adjustment
	err = tx.QueryRow(ctx, `
SELECT adjustment_id, delta, effective_at, applied_at, created_at
FROM adjustments WHERE entitlement_id = $1 AND adjustment_id = $2`,
		entitlementID, adjustmentID).
		Scan(&existing.AdjustmentID, &existing.Delta, &existing.EffectiveAt, &existing.AppliedAt, &existing.CreatedAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Adjustment{}, false, fmt.Errorf("load adjustment: %w", err)
	}
	if err == nil {
		if existing.Delta != delta || !existing.EffectiveAt.Equal(effectiveAt) {
			return Adjustment{}, false, fail(CodeAdjustmentParamChanged,
				"adjustment %q already exists with delta %d and effective_at %s, not delta %d and effective_at %s",
				adjustmentID, existing.Delta, existing.EffectiveAt.Format(time.RFC3339),
				delta, effectiveAt.Format(time.RFC3339))
		}
		existing.Status = adjustmentStatus(existing.AppliedAt)
		return existing, false, tx.Commit(ctx)
	}

	if delta < 0 && ent.quotaTotal+delta <= 0 {
		return Adjustment{}, false, fail(CodeInsufficientQuota,
			"decrease of %d would drop the quota total of %d to zero or below", -delta, ent.quotaTotal)
	}
	var created Adjustment
	err = tx.QueryRow(ctx, `
INSERT INTO adjustments (entitlement_id, adjustment_id, delta, effective_at)
VALUES ($1, $2, $3, $4)
RETURNING adjustment_id, delta, effective_at, applied_at, created_at`,
		entitlementID, adjustmentID, delta, effectiveAt).
		Scan(&created.AdjustmentID, &created.Delta, &created.EffectiveAt, &created.AppliedAt, &created.CreatedAt)
	if err != nil {
		return Adjustment{}, false, fmt.Errorf("insert adjustment: %w", err)
	}
	// An effective time that has already arrived applies within the same
	// transaction (a decrease only when occupancy allows it).
	if err := applyDueAdjustments(ctx, tx, &ent, entitlementID); err != nil {
		return Adjustment{}, false, err
	}
	if created.AppliedAt == nil && !effectiveAt.After(ent.now) {
		if err := tx.QueryRow(ctx, `
SELECT applied_at FROM adjustments WHERE entitlement_id = $1 AND adjustment_id = $2`,
			entitlementID, adjustmentID).Scan(&created.AppliedAt); err != nil {
			return Adjustment{}, false, fmt.Errorf("reload adjustment: %w", err)
		}
	}
	created.Status = adjustmentStatus(created.AppliedAt)
	if err := tx.Commit(ctx); err != nil {
		return Adjustment{}, false, fmt.Errorf("commit adjustment: %w", err)
	}
	return created, true, nil
}

// RescheduleEntitlement rewrites an existing entitlement's validity window.
// The caller-supplied rescheduleID is unique per entitlement: the first
// submission records the reschedule and immediately sets valid_from and
// valid_to to the new values (created=true); resubmitting the same
// identifier with the same new window returns the original record and does
// not touch the times again (created=false); resubmitting it with a
// different new start or end fails without changing state. No quota figure
// is ever modified: quota total, used, reserved and available amounts plus
// every reservation, reversal and adjustment keep their existing behavior
// and idempotency rules. The whole flow runs under the per-entitlement row
// lock, so reschedules serialize with reservations, settlements, reversals
// and adjustments, replays never rewrite the window twice, and failures
// leave no partial state.
func (s *Store) RescheduleEntitlement(ctx context.Context, entitlementID, rescheduleID string, newValidFrom, newValidTo time.Time) (Reschedule, bool, error) {
	if err := validIdentifier("reschedule_id", rescheduleID); err != nil {
		return Reschedule{}, false, err
	}
	if newValidFrom.IsZero() || newValidTo.IsZero() {
		return Reschedule{}, false, fail(CodeInvalidRequest, "new_valid_from and new_valid_to are required (RFC 3339 timestamps)")
	}
	if !newValidFrom.Before(newValidTo) {
		return Reschedule{}, false, fail(CodeInvalidRequest, "new_valid_from must be before new_valid_to")
	}
	newValidFrom = newValidFrom.UTC()
	newValidTo = newValidTo.UTC()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Reschedule{}, false, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := lockEntitlement(ctx, tx, entitlementID); err != nil {
		return Reschedule{}, false, err
	}

	var existing Reschedule
	err = tx.QueryRow(ctx, `
SELECT reschedule_id, new_valid_from, new_valid_to, created_at
FROM reschedules WHERE entitlement_id = $1 AND reschedule_id = $2`,
		entitlementID, rescheduleID).
		Scan(&existing.RescheduleID, &existing.NewValidFrom, &existing.NewValidTo, &existing.CreatedAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Reschedule{}, false, fmt.Errorf("load reschedule: %w", err)
	}
	if err == nil {
		if !existing.NewValidFrom.Equal(newValidFrom) || !existing.NewValidTo.Equal(newValidTo) {
			return Reschedule{}, false, fail(CodeRescheduleParamChanged,
				"reschedule %q already exists with new_valid_from %s and new_valid_to %s, not %s and %s",
				rescheduleID, existing.NewValidFrom.Format(time.RFC3339), existing.NewValidTo.Format(time.RFC3339),
				newValidFrom.Format(time.RFC3339), newValidTo.Format(time.RFC3339))
		}
		return existing, false, tx.Commit(ctx)
	}

	var created Reschedule
	err = tx.QueryRow(ctx, `
INSERT INTO reschedules (entitlement_id, reschedule_id, new_valid_from, new_valid_to)
VALUES ($1, $2, $3, $4)
RETURNING reschedule_id, new_valid_from, new_valid_to, created_at`,
		entitlementID, rescheduleID, newValidFrom, newValidTo).
		Scan(&created.RescheduleID, &created.NewValidFrom, &created.NewValidTo, &created.CreatedAt)
	if err != nil {
		return Reschedule{}, false, fmt.Errorf("insert reschedule: %w", err)
	}
	if _, err := tx.Exec(ctx, `
UPDATE entitlements SET valid_from = $1, valid_to = $2 WHERE entitlement_id = $3`,
		newValidFrom, newValidTo, entitlementID); err != nil {
		return Reschedule{}, false, fmt.Errorf("rewrite validity window: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Reschedule{}, false, fmt.Errorf("commit reschedule: %w", err)
	}
	return created, true, nil
}

// GetView returns the quota breakdown, reservation list, adjustment list
// and reschedule list for an entitlement, consistent with the committed
// state. Due adjustments are folded into the quota total as part of the
// read, under the same row lock as the mutating flows, so the reported
// figures always reflect every adjustment whose effective time has
// arrived — applied exactly once, even across restarts.
func (s *Store) GetView(ctx context.Context, entitlementID string) (View, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return View{}, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ent, err := lockEntitlement(ctx, tx, entitlementID)
	if err != nil {
		return View{}, err
	}
	if err := applyDueAdjustments(ctx, tx, &ent, entitlementID); err != nil {
		return View{}, err
	}

	var view View
	view.EntitlementID = entitlementID
	view.SeatsTotal = ent.seatsTotal
	view.QuotaTotal = ent.quotaTotal
	view.UsedAmount = ent.usedAmount
	view.ValidFrom = ent.validFrom
	view.ValidTo = ent.validTo
	switch {
	case ent.now.Before(view.ValidFrom):
		view.Status = "pending"
	case ent.now.Before(view.ValidTo):
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
	rows.Close()

	adjustmentRows, err := tx.Query(ctx, `
SELECT adjustment_id, delta, effective_at, applied_at, created_at
FROM adjustments WHERE entitlement_id = $1
ORDER BY created_at, adjustment_id`, entitlementID)
	if err != nil {
		return View{}, fmt.Errorf("list adjustments: %w", err)
	}
	defer adjustmentRows.Close()
	view.Adjustments = []Adjustment{}
	for adjustmentRows.Next() {
		var a Adjustment
		if err := adjustmentRows.Scan(&a.AdjustmentID, &a.Delta, &a.EffectiveAt, &a.AppliedAt, &a.CreatedAt); err != nil {
			return View{}, fmt.Errorf("scan adjustment: %w", err)
		}
		a.Status = adjustmentStatus(a.AppliedAt)
		view.Adjustments = append(view.Adjustments, a)
	}
	if err := adjustmentRows.Err(); err != nil {
		return View{}, fmt.Errorf("list adjustments: %w", err)
	}
	adjustmentRows.Close()

	rescheduleRows, err := tx.Query(ctx, `
SELECT reschedule_id, new_valid_from, new_valid_to, created_at
FROM reschedules WHERE entitlement_id = $1
ORDER BY created_at, reschedule_id`, entitlementID)
	if err != nil {
		return View{}, fmt.Errorf("list reschedules: %w", err)
	}
	defer rescheduleRows.Close()
	view.Reschedules = []Reschedule{}
	for rescheduleRows.Next() {
		var rc Reschedule
		if err := rescheduleRows.Scan(&rc.RescheduleID, &rc.NewValidFrom, &rc.NewValidTo, &rc.CreatedAt); err != nil {
			return View{}, fmt.Errorf("scan reschedule: %w", err)
		}
		view.Reschedules = append(view.Reschedules, rc)
	}
	if err := rescheduleRows.Err(); err != nil {
		return View{}, fmt.Errorf("list reschedules: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return View{}, fmt.Errorf("commit view: %w", err)
	}
	return view, nil
}

// Package entitlements implements the seat-quota entitlement domain:
// entitlement creation, idempotent reservations (each tagged with the
// owning internal department), confirm/release settlement, corrective
// reversals of confirmed reservations, scheduled quota-total adjustments,
// idempotent validity-window reschedules, immediate idempotent
// adjustments of the seat total (seat allocation and recovery),
// one-way expiry closure with a close-time accounting snapshot, and
// caller-timed pause/resume of grant issuance, persisted in PostgreSQL.
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
	CodeInvalidRequest           = "invalid_request"
	CodeEntitlementExists        = "entitlement_exists"
	CodeEntitlementNotFound      = "entitlement_not_found"
	CodeEntitlementNotActive     = "entitlement_not_active"
	CodeInsufficientQuota        = "insufficient_quota"
	CodeReservationNotFound      = "reservation_not_found"
	CodeReservationParamChanged  = "reservation_param_mismatch"
	CodeReservationSettled       = "reservation_already_settled"
	CodeReservationNotConfirmed  = "reservation_not_confirmed"
	CodeReservationReversed      = "reservation_already_reversed"
	CodeAdjustmentParamChanged   = "adjustment_param_mismatch"
	CodeRescheduleParamChanged   = "reschedule_param_mismatch"
	CodeSeatAdjustmentChanged    = "seat_adjustment_param_mismatch"
	CodeEntitlementClosed        = "entitlement_closed"
	CodePauseResumeParamChanged  = "pause_resume_param_mismatch"
	CodeEntitlementPaused        = "entitlement_paused"
	CodeEntitlementAlreadyPaused = "entitlement_already_paused"
	CodeEntitlementNotPaused     = "entitlement_not_paused"
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

// StatusClosed is the one-way lifecycle state of an entitlement whose
// validity time has elapsed: it stops accepting new grants and changes but
// keeps settling in-flight reservations. Unlike "expired" — a window-relative
// view label that can flip back to "active" after a reschedule — closure is
// permanent.
const StatusClosed = "closed"

// Pause and resume event kinds.
const (
	KindPause  = "pause"
	KindResume = "resume"
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

// Reservation is a quota hold placed by an internal sub-team. DepartmentID
// is the identifier of the internal department the hold is allocated to;
// it is supplied by the caller at creation, never changes afterwards, and
// is included on every replay with the same reservation identifier.
type Reservation struct {
	ReservationID string     `json:"reservation_id"`
	DepartmentID  string     `json:"department_id,omitempty"`
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

// SeatAdjustment is a caller-initiated allocation to, or recovery from, an
// entitlement's seat pool — identified by the caller-supplied business
// identifier, unique within the entitlement. Delta is a non-zero integer:
// positive grows the seat total immediately, negative shrinks it. A
// decrease is accepted only while the resulting total stays positive and
// still covers the seats already allocated to internal departments, so a
// seat adjustment never crowds out an existing allocation. SeatTotal is
// the cumulative seat total after this adjustment, computed by summing
// every adjustment in creation order starting from the entitlement's
// initial seats total. Seat adjustments never change any quota figure.
type SeatAdjustment struct {
	SeatAdjustmentID string    `json:"seat_adjustment_id"`
	Delta            int64     `json:"delta"`
	SeatTotal        int64     `json:"seat_total"`
	CreatedAt        time.Time `json:"created_at"`
}

// ClosureSnapshot is the accounting record written exactly once when an
// entitlement closes after its validity time has elapsed. It is keyed by the
// entitlement identifier and freezes the figures at the close instant: the
// seat total, quota total, used quota, unsettled reservations, available
// quota, the validity window in force and the close time. Every figure is
// the actual state at that moment; closure itself changes none of them.
type ClosureSnapshot struct {
	EntitlementID   string    `json:"entitlement_id"`
	SeatsTotal      int64     `json:"seats_total"`
	QuotaTotal      int64     `json:"quota_total"`
	UsedAmount      int64     `json:"used_amount"`
	ReservedAmount  int64     `json:"reserved_amount"`
	AvailableAmount int64     `json:"available_amount"`
	ValidFrom       time.Time `json:"valid_from"`
	ValidTo         time.Time `json:"valid_to"`
	ClosedAt        time.Time `json:"closed_at"`
}

// PauseResumeEvent is a caller-registered pause or resume of grant issuance,
// identified by the caller-supplied business identifier, unique within the
// entitlement and independent across the two kinds. At is the caller-supplied
// pause or resume time: a pause lies within the validity window, and a resume
// lies later than its matching pause and no later than the validity end.
// Neither event changes any quota figure, seat figure or validity window.
type PauseResumeEvent struct {
	ID        string    `json:"id"`
	Kind      string    `json:"type"` // "pause" or "resume"
	At        time.Time `json:"at"`
	CreatedAt time.Time `json:"created_at"`
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
// SeatAdjustments lists every seat-total adjustment in creation order, each
// with the cumulative seat total produced by summing deltas in that order;
// none of the quota figures above is affected by it. ClosedAt is set once the
// entitlement has closed after its validity end, and ClosureSnapshot then
// carries the close-time accounting record (nil/empty before closure).
// PauseResumeEvents lists every pause and resume in registration order; the
// last entry determines whether issuance is currently paused.
type View struct {
	EntitlementID   string             `json:"entitlement_id"`
	SeatsTotal      int64              `json:"seats_total"`
	QuotaTotal      int64              `json:"quota_total"`
	UsedAmount      int64              `json:"used_amount"`
	ReservedAmount  int64              `json:"reserved_amount"`
	AvailableAmount int64              `json:"available_amount"`
	Status          string             `json:"status"` // pending, active, expired or closed
	ValidFrom       time.Time          `json:"valid_from"`
	ValidTo         time.Time          `json:"valid_to"`
	ClosedAt        *time.Time         `json:"closed_at,omitempty"`
	ClosureSnapshot *ClosureSnapshot   `json:"closure_snapshot,omitempty"`
	PauseResume     []PauseResumeEvent `json:"pause_resume_events"`
	Reservations    []Reservation      `json:"reservations"`
	Adjustments     []Adjustment       `json:"adjustments"`
	Reschedules     []Reschedule       `json:"reschedules"`
	SeatAdjustments []SeatAdjustment   `json:"seat_adjustments"`
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
-- The internal department a reservation is allocated to, supplied at
-- creation and immutable afterwards. Nullable only for rows written by an
-- older schema; new reservations always carry one.
ALTER TABLE reservations ADD COLUMN IF NOT EXISTS department_id TEXT;
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
);
CREATE TABLE IF NOT EXISTS seat_adjustments (
    entitlement_id     TEXT NOT NULL REFERENCES entitlements (entitlement_id),
    seat_adjustment_id TEXT NOT NULL,
    delta              BIGINT NOT NULL CHECK (delta <> 0),
    -- Cumulative seat total after this adjustment, summed in creation order.
    seat_total         BIGINT NOT NULL CHECK (seat_total > 0),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (entitlement_id, seat_adjustment_id)
);
-- Closure is one-way: closed_at is set exactly once, by the first write or
-- view that touches the entitlement after its validity end, inside the same
-- transaction and row lock as that touch. paused_at marks an entitlement
-- whose grant issuance is currently suspended; null means issuing normally.
ALTER TABLE entitlements ADD COLUMN IF NOT EXISTS closed_at TIMESTAMPTZ;
ALTER TABLE entitlements ADD COLUMN IF NOT EXISTS paused_at TIMESTAMPTZ;
-- The accounting record frozen at the close instant. Exactly one row per
-- entitlement, written together with closed_at, so repeated touches after
-- expiry can never produce a second close record.
CREATE TABLE IF NOT EXISTS closure_snapshots (
    entitlement_id   TEXT PRIMARY KEY REFERENCES entitlements (entitlement_id),
    seats_total      BIGINT NOT NULL CHECK (seats_total > 0),
    quota_total      BIGINT NOT NULL CHECK (quota_total > 0),
    used_amount      BIGINT NOT NULL CHECK (used_amount >= 0),
    reserved_amount  BIGINT NOT NULL CHECK (reserved_amount >= 0),
    available_amount BIGINT NOT NULL CHECK (available_amount >= 0),
    valid_from       TIMESTAMPTZ NOT NULL,
    valid_to         TIMESTAMPTZ NOT NULL,
    closed_at        TIMESTAMPTZ NOT NULL
);
-- Caller-registered pause and resume events. The business identifier is
-- unique within an entitlement and shared across both kinds (an identifier
-- used for a pause cannot be reused for a resume with different data); the
-- kind is recorded on each row.
CREATE TABLE IF NOT EXISTS pause_resume_events (
    entitlement_id TEXT NOT NULL REFERENCES entitlements (entitlement_id),
    event_id       TEXT NOT NULL,
    kind           TEXT NOT NULL CHECK (kind IN ('pause', 'resume')),
    at             TIMESTAMPTZ NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (entitlement_id, event_id)
);
CREATE INDEX IF NOT EXISTS idx_pause_resume_events_order
    ON pause_resume_events (entitlement_id, created_at, event_id);`)
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
	closedAt   *time.Time
	pausedAt   *time.Time
}

// lockEntitlement takes the per-entitlement serialization lock inside tx.
func lockEntitlement(ctx context.Context, tx pgx.Tx, entitlementID string) (entitlementRow, error) {
	var row entitlementRow
	err := tx.QueryRow(ctx, `
SELECT seats_total, quota_total, used_amount, valid_from, valid_to, now(), closed_at, paused_at
FROM entitlements WHERE entitlement_id = $1 FOR UPDATE`, entitlementID).
		Scan(&row.seatsTotal, &row.quotaTotal, &row.usedAmount, &row.validFrom, &row.validTo,
			&row.now, &row.closedAt, &row.pausedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return entitlementRow{}, fail(CodeEntitlementNotFound, "entitlement %q does not exist", entitlementID)
	}
	if err != nil {
		return entitlementRow{}, fmt.Errorf("lock entitlement: %w", err)
	}
	return row, nil
}

// closeIfDue performs closure exactly once: if the validity end has arrived
// and the entitlement is not closed yet, it writes the closure snapshot with
// the figures at the close instant and stamps closed_at, all inside the
// caller's transaction under the row lock. A later reschedule can move the
// window back over the current time; closed_at stays set, so closure is
// one-way. Repeated touches find closed_at already set and neither insert a
// second snapshot nor move the close time. The snapshot is taken after due
// adjustments have been folded in, so it reflects the actual state at the
// close instant; closure itself changes no quota or seat figure.
func closeIfDue(ctx context.Context, tx pgx.Tx, ent *entitlementRow, entitlementID string) error {
	if ent.closedAt != nil || ent.now.Before(ent.validTo) {
		return nil
	}
	reserved, err := pendingAmount(ctx, tx, entitlementID)
	if err != nil {
		return err
	}
	available := ent.quotaTotal - ent.usedAmount - reserved
	closedAt := ent.now
	if _, err := tx.Exec(ctx, `
INSERT INTO closure_snapshots
    (entitlement_id, seats_total, quota_total, used_amount, reserved_amount,
     available_amount, valid_from, valid_to, closed_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		entitlementID, ent.seatsTotal, ent.quotaTotal, ent.usedAmount, reserved,
		available, ent.validFrom, ent.validTo, closedAt); err != nil {
		return fmt.Errorf("insert closure snapshot: %w", err)
	}
	if _, err := tx.Exec(ctx, `
UPDATE entitlements SET closed_at = $1
WHERE entitlement_id = $2 AND closed_at IS NULL`,
		closedAt, entitlementID); err != nil {
		return fmt.Errorf("stamp closure: %w", err)
	}
	ent.closedAt = &closedAt
	return nil
}

// ensureOpen rejects the write types that closure stops: new reservations,
// quota adjustments, seat adjustments and reschedules. Settlement and
// corrective reversal of in-flight reservations stay allowed, so they must
// not call this guard.
func ensureOpen(ent *entitlementRow, entitlementID string) *Error {
	if ent.closedAt != nil {
		return fail(CodeEntitlementClosed,
			"entitlement %q closed at %s and no longer accepts this operation",
			entitlementID, ent.closedAt.Format(time.RFC3339))
	}
	return nil
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

// allocatedSeats sums the seats currently allocated to internal
// departments — every reservation that has not been released. Reversals do
// not release a reservation (the original confirmed row stays confirmed),
// so they keep counting, matching the seats the customer still has
// distributed to departments. A seat decrease must never drop the seat
// total below this number.
func allocatedSeats(ctx context.Context, tx pgx.Tx, entitlementID string) (int64, error) {
	var allocated int64
	err := tx.QueryRow(ctx, `
SELECT COALESCE(SUM(amount), 0) FROM reservations
WHERE entitlement_id = $1 AND status <> 'released'`, entitlementID).Scan(&allocated)
	if err != nil {
		return 0, fmt.Errorf("sum allocated seats: %w", err)
	}
	return allocated, nil
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

// CreateReservation places a quota hold for an internal department. The
// caller-supplied reservationID is unique per entitlement, and
// departmentID identifies the department the seats are allocated to: it is
// fixed at creation, never changes and must be identical on every replay.
// Resubmitting the same identifier with the same amount and department
// returns the original reservation without consuming quota again
// (created=false); resubmitting it with a different amount or department
// fails without changing state.
func (s *Store) CreateReservation(ctx context.Context, entitlementID, reservationID, departmentID string, amount int64) (Reservation, bool, error) {
	if err := validIdentifier("reservation_id", reservationID); err != nil {
		return Reservation{}, false, err
	}
	if err := validIdentifier("department_id", departmentID); err != nil {
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
	// Touching an entitlement whose validity end has passed closes it once,
	// inside this same transaction.
	if err := closeIfDue(ctx, tx, &ent, entitlementID); err != nil {
		return Reservation{}, false, err
	}

	var existing Reservation
	err = tx.QueryRow(ctx, `
SELECT reservation_id, COALESCE(department_id, '') AS department_id, amount, status, settled_at, created_at
FROM reservations WHERE entitlement_id = $1 AND reservation_id = $2`,
		entitlementID, reservationID).
		Scan(&existing.ReservationID, &existing.DepartmentID, &existing.Amount,
			&existing.Status, &existing.SettledAt, &existing.CreatedAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Reservation{}, false, fmt.Errorf("load reservation: %w", err)
	}
	if err == nil {
		switch {
		case existing.Amount != amount:
			return Reservation{}, false, fail(CodeReservationParamChanged,
				"reservation %q already exists with amount %d, not %d", reservationID, existing.Amount, amount)
		case existing.DepartmentID != departmentID:
			return Reservation{}, false, fail(CodeReservationParamChanged,
				"reservation %q already exists with department_id %q, not %q", reservationID, existing.DepartmentID, departmentID)
		}
		return existing, false, tx.Commit(ctx)
	}

	// A closed entitlement never grants again. An identical replay above
	// still returns its original reservation, but a new identifier is refused.
	if err := ensureOpen(&ent, entitlementID); err != nil {
		return Reservation{}, false, err
	}
	if ent.pausedAt != nil {
		return Reservation{}, false, fail(CodeEntitlementPaused,
			"entitlement %q is paused (since %s) and does not accept new reservations",
			entitlementID, ent.pausedAt.Format(time.RFC3339))
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
INSERT INTO reservations (entitlement_id, reservation_id, department_id, amount)
VALUES ($1, $2, $3, $4)
RETURNING reservation_id, department_id, amount, status, settled_at, created_at`,
		entitlementID, reservationID, departmentID, amount).
		Scan(&created.ReservationID, &created.DepartmentID, &created.Amount,
			&created.Status, &created.SettledAt, &created.CreatedAt)
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
	// An expired entitlement closes on this touch, but an in-flight
	// reservation still settles exactly once afterwards; pausing does not
	// block settlement either.
	if err := closeIfDue(ctx, tx, &ent, entitlementID); err != nil {
		return Reservation{}, err
	}
	var reservation Reservation
	err = tx.QueryRow(ctx, `
SELECT reservation_id, COALESCE(department_id, '') AS department_id, amount, status, settled_at, created_at
FROM reservations WHERE entitlement_id = $1 AND reservation_id = $2`,
		entitlementID, reservationID).
		Scan(&reservation.ReservationID, &reservation.DepartmentID, &reservation.Amount,
			&reservation.Status, &reservation.SettledAt, &reservation.CreatedAt)
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
	// Closure happens on this touch but never blocks the correction of an
	// already confirmed reservation.
	if err := closeIfDue(ctx, tx, &ent, entitlementID); err != nil {
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
SELECT reservation_id, COALESCE(department_id, '') AS department_id, amount, status, settled_at, created_at
FROM reservations WHERE entitlement_id = $1 AND reservation_id = $2`,
		entitlementID, reservationID).
		Scan(&reservation.ReservationID, &reservation.DepartmentID, &reservation.Amount,
			&reservation.Status, &reservation.SettledAt, &reservation.CreatedAt)
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
	// An expired entitlement closes on this touch before the replay or the
	// feasibility checks below.
	if err := closeIfDue(ctx, tx, &ent, entitlementID); err != nil {
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

	// A closed entitlement accepts no new quota adjustment; an identical
	// replay above still returns the original record.
	if err := ensureOpen(&ent, entitlementID); err != nil {
		return Adjustment{}, false, err
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

// CreateSeatAdjustment changes an existing entitlement's seat total
// immediately — an allocation when delta is positive, a recovery when it
// is negative. The caller-supplied seatAdjustmentID is unique per
// entitlement: the first submission records the adjustment and sets the
// seat total to its new value (created=true); resubmitting the same
// identifier with the same delta returns the original adjustment,
// including its cumulative seat total, without changing seats again
// (created=false); resubmitting it with a different delta fails without
// changing state. A decrease is rejected, before anything is written, when
// it would drop the seat total below the seats already allocated to
// internal departments or to zero or below. Seat adjustments never touch
// quota_total, used, reserved or available quota, and they serialize with
// every other mutating flow on the per-entitlement row lock, so repeated
// requests never move the seat total twice and failures leave no partial
// state.
func (s *Store) CreateSeatAdjustment(ctx context.Context, entitlementID, seatAdjustmentID string, delta int64) (SeatAdjustment, bool, error) {
	if err := validIdentifier("seat_adjustment_id", seatAdjustmentID); err != nil {
		return SeatAdjustment{}, false, err
	}
	if delta == 0 {
		return SeatAdjustment{}, false, fail(CodeInvalidRequest, "delta is required and must be a non-zero integer")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SeatAdjustment{}, false, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ent, err := lockEntitlement(ctx, tx, entitlementID)
	if err != nil {
		return SeatAdjustment{}, false, err
	}
	// A touch folds due quota adjustments (the seat change itself still moves
	// no quota figure) and closes an expired entitlement before the replay or
	// the seat-total change below.
	if err := applyDueAdjustments(ctx, tx, &ent, entitlementID); err != nil {
		return SeatAdjustment{}, false, err
	}
	if err := closeIfDue(ctx, tx, &ent, entitlementID); err != nil {
		return SeatAdjustment{}, false, err
	}

	var existing SeatAdjustment
	err = tx.QueryRow(ctx, `
SELECT seat_adjustment_id, delta, seat_total, created_at
FROM seat_adjustments WHERE entitlement_id = $1 AND seat_adjustment_id = $2`,
		entitlementID, seatAdjustmentID).
		Scan(&existing.SeatAdjustmentID, &existing.Delta, &existing.SeatTotal, &existing.CreatedAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return SeatAdjustment{}, false, fmt.Errorf("load seat adjustment: %w", err)
	}
	if err == nil {
		if existing.Delta != delta {
			return SeatAdjustment{}, false, fail(CodeSeatAdjustmentChanged,
				"seat adjustment %q already exists with delta %d, not %d", seatAdjustmentID, existing.Delta, delta)
		}
		return existing, false, tx.Commit(ctx)
	}

	// A closed entitlement accepts no new seat adjustment; an identical
	// replay above still returns the original record.
	if err := ensureOpen(&ent, entitlementID); err != nil {
		return SeatAdjustment{}, false, err
	}

	newTotal := ent.seatsTotal + delta
	if delta > 0 {
		// Guard against a BIGINT overflow instead of failing deep in
		// PostgreSQL; realistic seat counts never approach this.
		if newTotal < ent.seatsTotal {
			return SeatAdjustment{}, false, fail(CodeInvalidRequest,
				"increase of %d would overflow the seat total of %d", delta, ent.seatsTotal)
		}
	} else {
		if newTotal <= 0 {
			return SeatAdjustment{}, false, fail(CodeInsufficientQuota,
				"decrease of %d would drop the seat total of %d to zero or below", -delta, ent.seatsTotal)
		}
		allocated, err := allocatedSeats(ctx, tx, entitlementID)
		if err != nil {
			return SeatAdjustment{}, false, err
		}
		if newTotal < allocated {
			return SeatAdjustment{}, false, fail(CodeInsufficientQuota,
				"decrease of %d would drop the seat total to %d, below the %d seats already allocated to departments",
				-delta, newTotal, allocated)
		}
	}

	var created SeatAdjustment
	err = tx.QueryRow(ctx, `
INSERT INTO seat_adjustments (entitlement_id, seat_adjustment_id, delta, seat_total)
VALUES ($1, $2, $3, $4)
RETURNING seat_adjustment_id, delta, seat_total, created_at`,
		entitlementID, seatAdjustmentID, delta, newTotal).
		Scan(&created.SeatAdjustmentID, &created.Delta, &created.SeatTotal, &created.CreatedAt)
	if err != nil {
		return SeatAdjustment{}, false, fmt.Errorf("insert seat adjustment: %w", err)
	}
	if _, err := tx.Exec(ctx, `
UPDATE entitlements SET seats_total = $1 WHERE entitlement_id = $2`,
		newTotal, entitlementID); err != nil {
		return SeatAdjustment{}, false, fmt.Errorf("adjust seat total: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return SeatAdjustment{}, false, fmt.Errorf("commit seat adjustment: %w", err)
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

	ent, err := lockEntitlement(ctx, tx, entitlementID)
	if err != nil {
		return Reschedule{}, false, err
	}
	// Like every other touch, fold due adjustments and close an entitlement
	// whose validity end has passed.
	if err := applyDueAdjustments(ctx, tx, &ent, entitlementID); err != nil {
		return Reschedule{}, false, err
	}
	if err := closeIfDue(ctx, tx, &ent, entitlementID); err != nil {
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

	// Closure is one-way: a closed entitlement cannot be reopened, so a new
	// reschedule is refused even if its window covers the current time. An
	// identical replay above still returns the original record.
	if err := ensureOpen(&ent, entitlementID); err != nil {
		return Reschedule{}, false, err
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

// registerPauseResume is the shared body of PauseEntitlement and
// ResumeEntitlement. The caller-supplied eventID is unique within the
// entitlement and shared across both kinds: resubmitting the same
// identifier with the same kind and time returns the original record
// without taking effect again (created=false); resubmitting it with a
// different kind or time fails without changing state. A pause stamps
// paused_at and is refused while already paused; a resume clears it and is
// refused while not paused. Times are caller-supplied: a pause lies within
// the validity window (bounds inclusive), and a resume lies strictly later
// than the matching pause and no later than the validity end. Neither event
// changes any quota figure, seat figure or the validity window, and both
// run under the per-entitlement row lock, serializing with every other
// flow. Like every other touch, the call first folds due adjustments and
// performs the one-time closure if the validity end has passed.
func (s *Store) registerPauseResume(ctx context.Context, entitlementID, eventID, kind string, at time.Time) (PauseResumeEvent, bool, error) {
	if err := validIdentifier("event_id", eventID); err != nil {
		return PauseResumeEvent{}, false, err
	}
	if at.IsZero() {
		return PauseResumeEvent{}, false, fail(CodeInvalidRequest, "at is required (an RFC 3339 timestamp)")
	}
	at = at.UTC()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PauseResumeEvent{}, false, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ent, err := lockEntitlement(ctx, tx, entitlementID)
	if err != nil {
		return PauseResumeEvent{}, false, err
	}
	if err := applyDueAdjustments(ctx, tx, &ent, entitlementID); err != nil {
		return PauseResumeEvent{}, false, err
	}
	if err := closeIfDue(ctx, tx, &ent, entitlementID); err != nil {
		return PauseResumeEvent{}, false, err
	}

	var existing PauseResumeEvent
	err = tx.QueryRow(ctx, `
SELECT event_id, kind, at, created_at FROM pause_resume_events
WHERE entitlement_id = $1 AND event_id = $2`, entitlementID, eventID).
		Scan(&existing.ID, &existing.Kind, &existing.At, &existing.CreatedAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return PauseResumeEvent{}, false, fmt.Errorf("load pause/resume event: %w", err)
	}
	if err == nil {
		if existing.Kind != kind || !existing.At.Equal(at) {
			return PauseResumeEvent{}, false, fail(CodePauseResumeParamChanged,
				"event %q already exists with type %q and at %s, not type %q and at %s",
				eventID, existing.Kind, existing.At.Format(time.RFC3339), kind, at.Format(time.RFC3339))
		}
		// An identical replay returns the original record and never changes
		// the paused flag, even after closure.
		return existing, false, tx.Commit(ctx)
	}

	// Pause and resume control issuance within the validity period; a closed
	// entitlement has stopped issuing for good, so no new event is accepted
	// (an identical replay above still returns its original record).
	if err := ensureOpen(&ent, entitlementID); err != nil {
		return PauseResumeEvent{}, false, err
	}

	// The time must lie inside the validity window; the resume must also
	// come strictly after the pause it undoes.
	if at.Before(ent.validFrom) || at.After(ent.validTo) {
		return PauseResumeEvent{}, false, fail(CodeInvalidRequest,
			"%s time %s must be within the validity window [%s, %s]",
			kind, at.Format(time.RFC3339), ent.validFrom.Format(time.RFC3339), ent.validTo.Format(time.RFC3339))
	}
	if kind == KindPause {
		if ent.pausedAt != nil {
			return PauseResumeEvent{}, false, fail(CodeEntitlementAlreadyPaused,
				"entitlement %q is already paused (since %s)", entitlementID, ent.pausedAt.Format(time.RFC3339))
		}
	} else {
		if ent.pausedAt == nil {
			return PauseResumeEvent{}, false, fail(CodeEntitlementNotPaused,
				"entitlement %q is not paused, so it cannot be resumed", entitlementID)
		}
		if !at.After(*ent.pausedAt) {
			return PauseResumeEvent{}, false, fail(CodeInvalidRequest,
				"resume time %s must be later than the pause time %s",
				at.Format(time.RFC3339), ent.pausedAt.Format(time.RFC3339))
		}
	}

	var created PauseResumeEvent
	err = tx.QueryRow(ctx, `
INSERT INTO pause_resume_events (entitlement_id, event_id, kind, at)
VALUES ($1, $2, $3, $4)
RETURNING event_id, kind, at, created_at`,
		entitlementID, eventID, kind, at).
		Scan(&created.ID, &created.Kind, &created.At, &created.CreatedAt)
	if err != nil {
		return PauseResumeEvent{}, false, fmt.Errorf("insert pause/resume event: %w", err)
	}
	if kind == KindPause {
		if _, err := tx.Exec(ctx, `UPDATE entitlements SET paused_at = $1 WHERE entitlement_id = $2`,
			at, entitlementID); err != nil {
			return PauseResumeEvent{}, false, fmt.Errorf("stamp pause: %w", err)
		}
	} else {
		if _, err := tx.Exec(ctx, `UPDATE entitlements SET paused_at = NULL WHERE entitlement_id = $1`,
			entitlementID); err != nil {
			return PauseResumeEvent{}, false, fmt.Errorf("clear pause: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return PauseResumeEvent{}, false, fmt.Errorf("commit pause/resume event: %w", err)
	}
	return created, true, nil
}

// PauseEntitlement suspends grant issuance on an entitlement at the
// caller-supplied time. While paused, new reservations are refused;
// settlement and corrective reversal of in-flight reservations are
// unaffected. No quota figure, seat figure or validity time changes.
func (s *Store) PauseEntitlement(ctx context.Context, entitlementID, eventID string, at time.Time) (PauseResumeEvent, bool, error) {
	return s.registerPauseResume(ctx, entitlementID, eventID, KindPause, at)
}

// ResumeEntitlement ends a pause previously registered with PauseEntitlement.
// The resume time must be later than the matching pause time.
func (s *Store) ResumeEntitlement(ctx context.Context, entitlementID, eventID string, at time.Time) (PauseResumeEvent, bool, error) {
	return s.registerPauseResume(ctx, entitlementID, eventID, KindResume, at)
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
	// A view is itself a touch: the first read after the validity end closes
	// the entitlement once, inside this read transaction.
	if err := closeIfDue(ctx, tx, &ent, entitlementID); err != nil {
		return View{}, err
	}

	var view View
	view.EntitlementID = entitlementID
	view.SeatsTotal = ent.seatsTotal
	view.QuotaTotal = ent.quotaTotal
	view.UsedAmount = ent.usedAmount
	view.ValidFrom = ent.validFrom
	view.ValidTo = ent.validTo
	view.ClosedAt = ent.closedAt
	switch {
	case ent.closedAt != nil:
		view.Status = StatusClosed
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
SELECT reservation_id, COALESCE(department_id, '') AS department_id, amount, status, settled_at, created_at
FROM (
    SELECT reservation_id, department_id, amount, status, settled_at, created_at
    FROM reservations WHERE entitlement_id = $1
    UNION ALL
    SELECT reversal_id, NULL::text, amount, 'reversed', settled_at, created_at
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
		if err := rows.Scan(&r.ReservationID, &r.DepartmentID, &r.Amount, &r.Status, &r.SettledAt, &r.CreatedAt); err != nil {
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
	rescheduleRows.Close()

	seatAdjustmentRows, err := tx.Query(ctx, `
SELECT seat_adjustment_id, delta, seat_total, created_at
FROM seat_adjustments WHERE entitlement_id = $1
ORDER BY created_at, seat_adjustment_id`, entitlementID)
	if err != nil {
		return View{}, fmt.Errorf("list seat adjustments: %w", err)
	}
	defer seatAdjustmentRows.Close()
	view.SeatAdjustments = []SeatAdjustment{}
	for seatAdjustmentRows.Next() {
		var sa SeatAdjustment
		if err := seatAdjustmentRows.Scan(&sa.SeatAdjustmentID, &sa.Delta, &sa.SeatTotal, &sa.CreatedAt); err != nil {
			return View{}, fmt.Errorf("scan seat adjustment: %w", err)
		}
		view.SeatAdjustments = append(view.SeatAdjustments, sa)
	}
	if err := seatAdjustmentRows.Err(); err != nil {
		return View{}, fmt.Errorf("list seat adjustments: %w", err)
	}

	// The close-time accounting record exists exactly when closed.
	if ent.closedAt != nil {
		var snap ClosureSnapshot
		if err := tx.QueryRow(ctx, `
SELECT entitlement_id, seats_total, quota_total, used_amount, reserved_amount,
       available_amount, valid_from, valid_to, closed_at
FROM closure_snapshots WHERE entitlement_id = $1`, entitlementID).
			Scan(&snap.EntitlementID, &snap.SeatsTotal, &snap.QuotaTotal, &snap.UsedAmount,
				&snap.ReservedAmount, &snap.AvailableAmount, &snap.ValidFrom, &snap.ValidTo,
				&snap.ClosedAt); err != nil {
			return View{}, fmt.Errorf("load closure snapshot: %w", err)
		}
		view.ClosureSnapshot = &snap
	}

	eventRows, err := tx.Query(ctx, `
SELECT event_id, kind, at, created_at
FROM pause_resume_events WHERE entitlement_id = $1
ORDER BY created_at, event_id`, entitlementID)
	if err != nil {
		return View{}, fmt.Errorf("list pause/resume events: %w", err)
	}
	view.PauseResume = []PauseResumeEvent{}
	for eventRows.Next() {
		var e PauseResumeEvent
		if err := eventRows.Scan(&e.ID, &e.Kind, &e.At, &e.CreatedAt); err != nil {
			eventRows.Close()
			return View{}, fmt.Errorf("scan pause/resume event: %w", err)
		}
		view.PauseResume = append(view.PauseResume, e)
	}
	if err := eventRows.Err(); err != nil {
		eventRows.Close()
		return View{}, fmt.Errorf("list pause/resume events: %w", err)
	}
	eventRows.Close()

	if err := tx.Commit(ctx); err != nil {
		return View{}, fmt.Errorf("commit view: %w", err)
	}
	return view, nil
}

package entitlements

import (
	"errors"
	"fmt"
	"net/http"
)

// Code is a stable, machine-readable error code. The same code always maps to
// the same HTTP status (see HTTPStatus), so clients can distinguish failure
// kinds reliably.
type Code string

const (
	// CodeInvalidRequest: malformed JSON or missing/illegal parameters.
	CodeInvalidRequest Code = "invalid_request"
	// CodeEntitlementNotFound: no entitlement for the given identifier.
	CodeEntitlementNotFound Code = "entitlement_not_found"
	// CodeReservationNotFound: no reservation for the given business
	// identifier within the entitlement.
	CodeReservationNotFound Code = "reservation_not_found"
	// CodeEntitlementExists: creating an entitlement whose identifier is
	// already taken.
	CodeEntitlementExists Code = "entitlement_exists"
	// CodeNotEffective: reservations are rejected before the effective start.
	CodeNotEffective Code = "entitlement_not_effective"
	// CodeExpired: reservations are rejected at or after the effective end.
	CodeExpired Code = "entitlement_expired"
	// CodeInsufficientCredit: available credit cannot cover the reservation.
	CodeInsufficientCredit Code = "insufficient_credit"
	// CodeIdempotencyMismatch: the same business reservation identifier was
	// resent with different parameters. No state changes.
	CodeIdempotencyMismatch Code = "idempotency_mismatch"
	// CodeAlreadySettled: a reservation was settled more than once, or
	// confirmed after being released. No state changes.
	CodeAlreadySettled Code = "reservation_already_settled"
)

// Error is a domain error carrying a stable code alongside its message.
type Error struct {
	Code    Code
	Message string
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// NewError builds a coded error.
func NewError(code Code, message string) error {
	return &Error{Code: code, Message: message}
}

// InvalidRequest builds a validation error.
func InvalidRequest(message string) error {
	return NewError(CodeInvalidRequest, message)
}

// HTTPStatus maps an error code to its stable HTTP status.
func HTTPStatus(code Code) int {
	switch code {
	case CodeInvalidRequest:
		return http.StatusBadRequest
	case CodeEntitlementNotFound, CodeReservationNotFound:
		return http.StatusNotFound
	default:
		// All remaining domain conflicts are state conflicts.
		return http.StatusConflict
	}
}

// CodeOf extracts the domain code from an error, reporting whether one was
// present.
func CodeOf(err error) (Code, bool) {
	var coded *Error
	if errors.As(err, &coded) {
		return coded.Code, true
	}
	return "", false
}

// Wrap annotates a coded error while preserving its code.
func Wrap(code Code, format string, args ...any) error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

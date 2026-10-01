# Entitlement service

Go HTTP service for team service entitlements: teams declare an entitlement
with a seat total, a credit total and an effective window; internal sub-teams
reserve credit against it, then settle each reservation by confirming
(consume) or releasing (refund). All state is persisted in PostgreSQL.
Requires Go 1.27.1 and PostgreSQL 18.6.

## Running locally

```sh
go mod download
go test ./...
mkdir -p .local
initdb -D .local/pgdata --auth=trust --encoding=UTF8 --locale=C
pg_ctl -D .local/pgdata -l .local/postgres.log -o '-h 127.0.0.1 -p 15432' -w start
createdb -h 127.0.0.1 -p 15432 entitlements
DATABASE_URL='postgres://127.0.0.1:15432/entitlements?sslmode=disable' LISTEN_ADDR=127.0.0.1:8080 go run ./cmd/server
```

On startup the service creates its schema
(`entitlements` and `reservations` tables plus a quota-guard trigger) if it is
missing; no separate migration step is needed. Stop the server with Ctrl+C and
the local database with `pg_ctl -D .local/pgdata -m fast -w stop`.

The local setup uses a trusted loopback connection and C collation without ICU.
Use separate database directories, database ports and HTTP ports when running
multiple copies. `DATABASE_URL` is required; `LISTEN_ADDR` defaults to
`127.0.0.1:8080`.

## Health checks

`GET /healthz` returns HTTP 200 with `{"status":"ok"}`. `GET /readyz` checks
PostgreSQL and returns HTTP 200 with `{"status":"ready"}`, or HTTP 503 with
`{"status":"unavailable"}`. Unknown paths return 404; wrong methods return 405.

## Entitlement API

All requests and responses are JSON (`Content-Type: application/json`,
RFC3339 timestamps). Error responses look like
`{"error":{"code":"...","message":"..."}}`.

### Create an entitlement — `POST /v1/entitlements`

```sh
curl -sS -X POST http://127.0.0.1:8080/v1/entitlements \
  -H 'Content-Type: application/json' \
  -d '{"entitlement_id":"team-a","seat_total":50,"credit_total":100,
       "effective_from":"2026-01-01T00:00:00Z","effective_to":"2026-12-31T23:59:59Z"}'
```

Returns `201` with the entitlement view. Every field is required; `seat_total`
and `credit_total` must be `>= 0`, and `effective_from` must precede
`effective_to`. Invalid input fails with `400 invalid_request` and writes
nothing. A repeated identifier returns `409 entitlement_exists` without
changing the original.

### Get the entitlement view — `GET /v1/entitlements/{entitlement_id}`

```sh
curl -sS http://127.0.0.1:8080/v1/entitlements/team-a
```

Returns the total/used/reserved/available credit, the seat total, the current
lifecycle status (`pending` | `active` | `expired`), the effective window and
the full reservation list (each with its business id, credit, status
`reserved`|`confirmed`|`released` and settlement `result` once settled). The
view is read as one database snapshot, so it always matches committed state.
Unknown entitlements return `404 entitlement_not_found`.

### Create (or replay) a reservation — `PUT /v1/entitlements/{id}/reservations/{business_reservation_id}`

```sh
curl -sS -X PUT http://127.0.0.1:8080/v1/entitlements/team-a/reservations/order-123 \
  -H 'Content-Type: application/json' -d '{"credit":30}'
```

`{business_reservation_id}` is caller-provided and unique within one
entitlement; it is the idempotency key. The first call reserves `credit`
(must be `> 0`); resending the same identifier with the same body returns the
original result without deducting twice. The same identifier with a different
credit returns `409 idempotency_mismatch` and changes nothing.

Reservations are rejected before the window starts (`409
entitlement_not_effective`), after it ends (`409 entitlement_expired`) and
when `used + outstanding reserved + credit > total` (`409
insufficient_credit`). Expiry never revokes existing reservations; after
expiry new reservations fail but outstanding ones can still be confirmed or
released.

### Settle a reservation

```sh
curl -sS -X POST http://127.0.0.1:8080/v1/entitlements/team-a/reservations/order-123/confirm
curl -sS -X POST http://127.0.0.1:8080/v1/entitlements/team-a/reservations/order-123/release
```

`confirm` moves reserved credit into `credit_used`; `release` returns it to
`credit_available`. A reservation settles exactly once: a second settlement,
or confirming a released reservation, returns
`409 reservation_already_settled` without affecting existing data. Unknown
reservations return `404 reservation_not_found`.

### Error codes and status codes

| HTTP | `error.code`                 | Meaning                                                    |
|------|------------------------------|------------------------------------------------------------|
| 400  | `invalid_request`            | Malformed JSON or missing/illegal parameter                |
| 404  | `entitlement_not_found`      | Unknown entitlement id                                     |
| 404  | `reservation_not_found`      | Unknown reservation id within the entitlement              |
| 409  | `entitlement_exists`         | Creating an entitlement whose id already exists            |
| 409  | `entitlement_not_effective`  | Reservation before `effective_from`                        |
| 409  | `entitlement_expired`        | Reservation at/after `effective_to`                        |
| 409  | `insufficient_credit`        | Available credit cannot cover the reservation              |
| 409  | `idempotency_mismatch`       | Same business reservation id resent with different credit  |
| 409  | `reservation_already_settled` | Double settlement or confirm after release                |
| 500  | `internal_error`             | Unexpected server error (internal details are not exposed) |

## Concurrency and durability

Reserves, confirms and releases on one entitlement take a row lock on that
entitlement inside one transaction, so they apply strictly serially; a
deferred database trigger additionally enforces
`credit_used + outstanding reserved <= credit_total`. Failed requests roll
back completely and leave no partial state. All amounts and statuses live in
PostgreSQL, so a service restart preserves every balance and settlement.

## Tests

```sh
go test ./...                 # unit tests (skips PostgreSQL tests without a database)
DATABASE_URL='postgres://127.0.0.1:15432/entitlements?sslmode=disable' \
  go test -race ./...         # includes the PostgreSQL integration suite
```

The integration tests cover validation, idempotent replay and parameter
mismatch, validity windows, quota exhaustion, confirm/release settlement,
expired-window settlement, concurrent reserve/confirm/reload workers, and a
simulated restart that re-checks balances from a fresh connection pool.

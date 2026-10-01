# Entitlement service

Go HTTP service for team service entitlements: seat-quota grants with idempotent reservations, confirm/release settlement, and idempotent correction reversals of confirmed reservations, persisted in PostgreSQL. Requires Go 1.27.1 and PostgreSQL 18.6.

```sh
go mod download
go test ./...
mkdir -p .local
initdb -D .local/pgdata --auth=trust --encoding=UTF8 --locale=C
pg_ctl -D .local/pgdata -l .local/postgres.log -o '-h 127.0.0.1 -p 15432' -w start
createdb -h 127.0.0.1 -p 15432 entitlements
DATABASE_URL='postgres://127.0.0.1:15432/entitlements?sslmode=disable' LISTEN_ADDR=127.0.0.1:8080 go run ./cmd/server
```

The schema is created automatically on startup. `DATABASE_URL` is required; `LISTEN_ADDR` defaults to `127.0.0.1:8080`. Stop the server with Ctrl+C and the local database with `pg_ctl -D .local/pgdata -m fast -w stop`.

The integration tests in `internal/entitlements` run when a database is reachable:

```sh
TEST_DATABASE_URL='postgres://127.0.0.1:15432/entitlements?sslmode=disable' go test ./...
```

## Health endpoints

`GET /healthz` returns HTTP 200 with `{"status":"ok"}`. `GET /readyz` checks PostgreSQL and returns HTTP 200 with `{"status":"ready"}`, or HTTP 503 with `{"status":"unavailable"}`. Other paths return 404.

## Entitlement API

All requests and responses are JSON. Failures return `{"error":{"code":"...","message":"..."}}` with a stable code and HTTP status.

### Create an entitlement

`POST /entitlements`

```json
{"entitlement_id":"team-alpha","seats_total":100,"quota_total":100,"valid_from":"2026-01-01T00:00:00Z","valid_to":"2027-01-01T00:00:00Z"}
```

`seats_total` is optional and defaults to `quota_total`. Returns 201 with the entitlement. Missing or illegal parameters fail with 400 `invalid_request` and write nothing; a duplicate identifier fails with 409 `entitlement_exists`.

### Create a reservation

`POST /entitlements/{entitlement_id}/reservations`

```json
{"reservation_id":"sub-a-001","amount":30}
```

`reservation_id` is the caller-supplied business identifier, unique within the entitlement. The first submission returns 201 and holds the quota. Resubmitting the same identifier with the same amount returns 200 with the original reservation and does not consume quota again; resubmitting it with a different amount fails with 409 `reservation_param_mismatch` and changes nothing. Reservations outside the validity window fail with 422 `entitlement_not_active`; insufficient quota fails with 422 `insufficient_quota` and leaves no partial state.

### Settle a reservation

`POST /entitlements/{entitlement_id}/reservations/{reservation_id}/confirm` moves the held amount to used quota. `POST .../release` returns it to available quota. Both return 200 with the settled reservation. A reservation settles exactly once: any further confirm or release fails with 409 `reservation_already_settled` without affecting committed data. Settlement stays allowed after the entitlement expires.

### Reverse a confirmed reservation (correction)

When operations or implementation staff find that a confirmed reservation was recorded in error, they reverse it:

`POST /entitlements/{entitlement_id}/reservations/{reservation_id}/reverse`

```json
{"reversal_id":"fix-2026-03-001"}
```

`reversal_id` is the caller-supplied correction business identifier, unique within the entitlement. The first submission returns 201 with an independent negative used record and returns the reservation's original amount from used to available quota — the caller never specifies an amount, the confirmed reservation's amount is reused. Reversing stays allowed after the entitlement expires, matching settlement; new reservations are still blocked then.

Reversals are idempotent and constrained:

- Resubmitting the same `reversal_id` for the same target reservation returns 200 with the original reversal and does **not** refund quota again.
- Resubmitting the same `reversal_id` for a different target reservation fails with 409 `reservation_param_mismatch` and changes nothing.
- A reservation reverses at most once: a second correction (with any other `reversal_id`) fails with 409 `reservation_already_settled` without affecting committed data.
- Pending or released reservations fail with 409 `reservation_already_settled`; an unknown reservation fails with 404 `reservation_not_found`; an unknown entitlement fails with 404 `entitlement_not_found`.
- A missing or malformed `reversal_id` fails with 400 `invalid_request` and writes nothing.

A reversal does not overwrite the confirmation. The original reservation and confirmation stay queryable, and the reversal is a separate record; both appear in the entitlement's reservation list.

### Query an entitlement

`GET /entitlements/{entitlement_id}` returns 200 with the quota breakdown and reservation list:

```json
{
  "entitlement_id": "team-alpha",
  "seats_total": 100,
  "quota_total": 100,
  "used_amount": 0,
  "reserved_amount": 0,
  "available_amount": 100,
  "status": "active",
  "valid_from": "2026-01-01T00:00:00Z",
  "valid_to": "2027-01-01T00:00:00Z",
  "reservations": [
    {"reservation_id": "sub-a-001", "amount": 30, "status": "confirmed", "settled_at": "...", "created_at": "..."},
    {"kind": "reversed", "reversal_id": "fix-2026-03-001", "reversal_of": "sub-a-001", "amount": -30, "status": "reversed", "settled_at": "...", "created_at": "..."}
  ]
}
```

`status` is `pending`, `active` or `expired` relative to the validity window. Reservation entries report their business identifier, amount, lifecycle status (`pending`, `confirmed`, `released`) and settlement timestamp. Reversal entries are distinguished by `"kind":"reversed"` and report their own correction identifier (`reversal_id`), the target reservation (`reversal_of`), a negative `amount`, status `reversed`, and creation/settlement timestamps. `used_amount` is the sum of confirmed amounts minus applied reversals; `reserved_amount` covers only pending reservations, and `available_amount = quota_total - used_amount - reserved_amount`.

### Error codes

| Code | HTTP | Meaning |
| --- | --- | --- |
| `invalid_request` | 400 | Missing or illegal parameters, malformed JSON |
| `entitlement_exists` | 409 | Entitlement identifier already in use |
| `entitlement_not_found` | 404 | Unknown entitlement |
| `entitlement_not_active` | 422 | Reservation outside the validity window |
| `insufficient_quota` | 422 | Not enough available quota |
| `reservation_not_found` | 404 | Unknown reservation |
| `reservation_param_mismatch` | 409 | Business identifier reused with different parameters |
| `reservation_already_settled` | 409 | Reservation was already confirmed or released |

## Consistency

Concurrent reservations, confirmations, releases and reversals on the same entitlement are serialized with a row lock in PostgreSQL, so used quota plus unsettled reservations never exceeds the total quota, and failed requests leave no partial results. Repeated requests neither consume nor refund quota twice: reservations and reversals are keyed by their caller-supplied business identifiers, and a reservation is reversed at most once (enforced by a unique constraint as well as the row lock). All state lives in PostgreSQL, so a restart preserves every quota figure, reservation status and correction record.

## Verifying a correction end to end

With the server running on `127.0.0.1:8080`, the operations/implementation team can walk the full flow with curl:

```sh
B=http://127.0.0.1:8080; E=team-alpha
curl -s -X POST $B/entitlements -H 'Content-Type: application/json' \
  -d '{"entitlement_id":"'$E'","quota_total":100,"valid_from":"2026-01-01T00:00:00Z","valid_to":"2027-01-01T00:00:00Z"}'
curl -s -X POST $B/entitlements/$E/reservations -H 'Content-Type: application/json' \
  -d '{"reservation_id":"sub-a-001","amount":30}'
curl -s -X POST $B/entitlements/$E/reservations/sub-a-001/confirm
# Correction: reverse the confirmed reservation (no amount supplied).
curl -s -i -X POST $B/entitlements/$E/reservations/sub-a-001/reverse \
  -H 'Content-Type: application/json' -d '{"reversal_id":"fix-2026-03-001"}'
# Replaying the same correction returns 200 and the same body, refunding once.
curl -s -i -X POST $B/entitlements/$E/reservations/sub-a-001/reverse \
  -H 'Content-Type: application/json' -d '{"reversal_id":"fix-2026-03-001"}'
# A second correction of the same reservation is rejected.
curl -s -i -X POST $B/entitlements/$E/reservations/sub-a-001/reverse \
  -H 'Content-Type: application/json' -d '{"reversal_id":"fix-other"}'
# The ledger shows both the original confirmation and the negative reversal.
curl -s $B/entitlements/$E
```

The final query reports `used_amount: 0`, `reserved_amount: 0`, `available_amount: 100` and two `reservations` entries: the original `confirmed` reservation and the independent `reversed` record with `amount: -30`.

The local setup uses a trusted loopback connection and C collation without ICU. Use separate database directories, database ports and HTTP ports when running multiple copies.

# Entitlement service

Go HTTP service for team service entitlements: seat-quota grants with idempotent reservations, confirm/release settlement, corrective reversals of confirmed reservations, and idempotent quota-total adjustments with a scheduled effective time, persisted in PostgreSQL. Requires Go 1.27.1 and PostgreSQL 18.6.

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

### Reverse a confirmed reservation

`POST /entitlements/{entitlement_id}/reservations/{reservation_id}/reverse`

```json
{"reversal_id":"corr-2026-0001"}
```

When operations discovers a confirmed reservation was recorded by mistake, a reversal corrects it: the service records a reversal entry with the reservation's original amount and moves that amount from used quota back to available quota. The amount always comes from the confirmed reservation; the caller must not send one (an `amount` field is rejected with 400 `invalid_request`).

`reversal_id` is the caller-supplied correction identifier, unique within the entitlement. The first submission returns 201 with the reversal. Resubmitting the same identifier for the same reservation returns 200 with the original reversal and does not refund again; resubmitting it for a different reservation fails with 409 `reservation_param_mismatch` and changes nothing. A reservation can be reversed at most once: a further reversal under a new identifier fails with 409 `reservation_already_reversed`. Only confirmed reservations can be reversed — pending or released ones fail with 422 `reservation_not_confirmed`; an unknown reservation fails with 404 `reservation_not_found`. Reversal stays allowed after the entitlement expires, matching settlement.

The reversal response and the entitlement's reservation list both describe the reversal with its own identifier, amount, status `reversed`, and creation and settlement timestamps; the original reservation and its confirmation record stay untouched and remain visible alongside it.

### Adjust the quota total

`POST /entitlements/{entitlement_id}/adjustments`

```json
{"adjustment_id":"adj-2026-0001","delta":50,"effective_at":"2026-02-01T00:00:00Z"}
```

When a customer buys more quota, is granted bonus quota, or has quota clawed back, operations records an adjustment against the existing entitlement. `delta` is a non-zero integer: positive raises the quota total, negative lowers it. `effective_at` is the moment the adjustment starts counting into the quota total.

`adjustment_id` is the caller-supplied business identifier, unique within the entitlement. The first submission returns 201 and records the adjustment. Resubmitting the same identifier with the same delta and effective time returns 200 with the original adjustment and does not count it again; resubmitting it with a different delta or effective time fails with 409 `adjustment_param_mismatch` and changes nothing. A missing or zero `delta` or a malformed `effective_at` fails with 400 `invalid_request`; an adjustment that would reduce the current quota total to zero or less fails with 422 `adjustment_quota_nonpositive` and writes nothing.

Before `effective_at` the adjustment is listed in the entitlement view with status `pending` and the quota total is unchanged. Once the effective time arrives the adjustment is counted into the quota total exactly once — even across service restarts or late duplicate requests — and its status becomes `applied`. A positive adjustment always applies. A decrease applies only while the resulting total still covers the used plus unsettled reserved amounts; otherwise it stays `pending` (distinguishable in the view) and applies automatically, in full and exactly once, as soon as the occupied amount fits the decreased total. A decrease never applies partially, and used plus reserved quota never exceeds the current quota total. Adjustments are accepted independently of the validity window, matching settlement and reversal.

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
    {"reservation_id": "corr-2026-0001", "amount": 30, "status": "reversed", "settled_at": "...", "created_at": "..."}
  ],
  "adjustments": [
    {"adjustment_id": "adj-2026-0001", "delta": 50, "effective_at": "2026-02-01T00:00:00Z", "status": "applied", "applied_at": "...", "created_at": "..."},
    {"adjustment_id": "adj-2026-0002", "delta": -20, "effective_at": "2026-03-01T00:00:00Z", "status": "pending", "created_at": "..."}
  ]
}
```

`status` is `pending`, `active` or `expired` relative to the validity window. Each reservation entry reports its business identifier, amount, lifecycle status (`pending`, `confirmed`, `released` for reservations; `reversed` for corrective reversals) and settlement timestamp. `used_amount` is confirmed quota minus effective reversals, so a reversed reservation leaves the original confirmation and the reversal both visible while the quota figures reflect only the net effect. Each adjustment entry reports its business identifier, delta, effective time and whether it has been counted into `quota_total` yet (`applied`) or is still waiting (`pending`); `quota_total`, `available_amount` and the other figures always reflect exactly the applied adjustments.

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
| `reservation_not_confirmed` | 422 | Reservation is not confirmed, so it cannot be reversed |
| `reservation_already_reversed` | 409 | Reservation was already reversed |
| `adjustment_param_mismatch` | 409 | Adjustment identifier reused with a different delta or effective time |
| `adjustment_quota_nonpositive` | 422 | Adjustment would reduce the quota total to zero or less |

## Consistency

Concurrent reservations, confirmations, releases, reversals and adjustments on the same entitlement are serialized with a row lock in PostgreSQL, so used quota plus unsettled reservations never exceeds the total quota, repeated requests never grant, refund or adjust quota twice, and failed requests leave no partial results. All state lives in PostgreSQL, so a restart preserves every quota figure, reservation status, reversal record and adjustment record with its applied or pending state.

The local setup uses a trusted loopback connection and C collation without ICU. Use separate database directories, database ports and HTTP ports when running multiple copies.

## Verifying the reversal flow

With the server running as above, the full correction cycle can be exercised with curl:

```sh
# Create an entitlement and confirm a reservation of 30 seats.
curl -s -X POST 127.0.0.1:8080/entitlements -d '{"entitlement_id":"team-alpha","quota_total":100,"valid_from":"2026-01-01T00:00:00Z","valid_to":"2027-01-01T00:00:00Z"}'
curl -s -X POST 127.0.0.1:8080/entitlements/team-alpha/reservations -d '{"reservation_id":"sub-a-001","amount":30}'
curl -s -X POST 127.0.0.1:8080/entitlements/team-alpha/reservations/sub-a-001/confirm

# Reverse it: used_amount drops back to 0 and both records stay listed.
curl -s -X POST 127.0.0.1:8080/entitlements/team-alpha/reservations/sub-a-001/reverse -d '{"reversal_id":"corr-2026-0001"}'
curl -s 127.0.0.1:8080/entitlements/team-alpha

# Replays and illegal corrections are safe no-ops with stable error codes.
curl -s -X POST 127.0.0.1:8080/entitlements/team-alpha/reservations/sub-a-001/reverse -d '{"reversal_id":"corr-2026-0001"}'   # 200, original reversal
curl -s -X POST 127.0.0.1:8080/entitlements/team-alpha/reservations/sub-a-001/reverse -d '{"reversal_id":"corr-2026-0002"}'   # 409 reservation_already_reversed
curl -s -X POST 127.0.0.1:8080/entitlements/team-alpha/reservations/sub-a-404/reverse -d '{"reversal_id":"corr-2026-0003"}'   # 404 reservation_not_found
```

## Verifying the adjustment flow

With the server running as above, the full adjustment cycle can be exercised with curl:

```sh
# Create an entitlement with quota 100 and occupy 90 of it (60 confirmed + 30 held).
curl -s -X POST 127.0.0.1:8080/entitlements -d '{"entitlement_id":"team-beta","quota_total":100,"valid_from":"2026-01-01T00:00:00Z","valid_to":"2027-01-01T00:00:00Z"}'
curl -s -X POST 127.0.0.1:8080/entitlements/team-beta/reservations -d '{"reservation_id":"sub-b-001","amount":60}'
curl -s -X POST 127.0.0.1:8080/entitlements/team-beta/reservations/sub-b-001/confirm
curl -s -X POST 127.0.0.1:8080/entitlements/team-beta/reservations -d '{"reservation_id":"sub-b-002","amount":30}'

# A scheduled increase is listed as pending and does not change quota_total yet.
curl -s -X POST 127.0.0.1:8080/entitlements/team-beta/adjustments -d '{"adjustment_id":"adj-2027-0001","delta":50,"effective_at":"2027-06-01T00:00:00Z"}'
curl -s 127.0.0.1:8080/entitlements/team-beta

# A decrease to 50 cannot cover the 90 occupied: it stays pending, quota_total unchanged.
curl -s -X POST 127.0.0.1:8080/entitlements/team-beta/adjustments -d '{"adjustment_id":"adj-2026-0002","delta":-50,"effective_at":"2026-10-01T00:00:00Z"}'
curl -s 127.0.0.1:8080/entitlements/team-beta

# Replays and mismatches are safe no-ops with stable error codes.
curl -s -X POST 127.0.0.1:8080/entitlements/team-beta/adjustments -d '{"adjustment_id":"adj-2026-0002","delta":-50,"effective_at":"2026-10-01T00:00:00Z"}'  # 200, original adjustment
curl -s -X POST 127.0.0.1:8080/entitlements/team-beta/adjustments -d '{"adjustment_id":"adj-2026-0002","delta":-40,"effective_at":"2026-10-01T00:00:00Z"}'  # 409 adjustment_param_mismatch
curl -s -X POST 127.0.0.1:8080/entitlements/team-beta/adjustments -d '{"adjustment_id":"adj-2026-0003","delta":-100,"effective_at":"2026-10-01T00:00:00Z"}' # 422 adjustment_quota_nonpositive

# Free the occupied quota: the pending decrease applies automatically, exactly once.
curl -s -X POST 127.0.0.1:8080/entitlements/team-beta/reservations/sub-b-002/release
curl -s -X POST 127.0.0.1:8080/entitlements/team-beta/reservations/sub-b-001/reverse -d '{"reversal_id":"corr-2026-0009"}'
curl -s 127.0.0.1:8080/entitlements/team-beta   # quota_total 50, adjustment applied
```

Restarting the service (Ctrl+C and `go run ./cmd/server` again) preserves every adjustment record, its applied or pending state, and the quota total; replaying an applied adjustment after the restart returns the original record without counting it again.

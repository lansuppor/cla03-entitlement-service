# Entitlement service

Go HTTP service for team service entitlements: seat-quota grants with idempotent reservations, confirm/release settlement, corrective reversals of confirmed reservations, scheduled quota-total adjustments, and idempotent validity-window reschedules, persisted in PostgreSQL. Requires Go 1.27.1 and PostgreSQL 18.6.

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
{"adjustment_id":"upsell-2026-0042","delta":50,"effective_at":"2026-06-01T00:00:00Z"}
```

When a customer buys more seats, is granted bonus quota, or has quota clawed back, operations registers an adjustment against the existing entitlement. `delta` is a non-zero integer: positive grows the quota total, negative shrinks it. `effective_at` is the moment the adjustment starts counting toward the quota total. `adjustment_id` is the caller-supplied business identifier, unique within the entitlement. The first submission returns 201 with the adjustment. Resubmitting the same identifier with the same delta and effective time returns 200 with the original adjustment and does not change any quota; resubmitting it with a different delta or effective time fails with 409 `adjustment_param_mismatch` and changes nothing. A zero or missing `delta` or a malformed `effective_at` fails with 400 `invalid_request`.

Before `effective_at` the adjustment is recorded with status `pending`, is listed in the entitlement view, and does not change the quota total. Once the effective time has arrived the adjustment is folded into `quota_total` exactly once — by the next operation or query on the entitlement — and reported with status `applied` and its `applied_at` timestamp; service restarts and late duplicate requests can neither apply it twice nor lose it. An increase always applies. A decrease applies only while the adjusted total still covers used plus unsettled quota: if occupied quota exceeds the adjusted total, the decrease stays `pending` (visible as such in the view) and takes effect automatically, in full and exactly once, once occupancy allows — it is never partially applied, and used plus unsettled quota never exceeds the current total. A decrease that would drop the current quota total to zero or below fails with 422 `insufficient_quota` and writes nothing.

### Reschedule the validity window

`POST /entitlements/{entitlement_id}/reschedules`

```json
{"reschedule_id":"postpone-launch-2026-007","new_valid_from":"2026-02-01T00:00:00Z","new_valid_to":"2027-02-01T00:00:00Z"}
```

When a customer goes live later than planned, is taken offline early, or signs a commercial date change, operations rewrites the validity window of an existing entitlement. `reschedule_id` is the caller-supplied business identifier, unique within the entitlement. `new_valid_from` and `new_valid_to` are RFC 3339 timestamps and the new start must be strictly before the new end. Both are required; a reversed, missing or malformed window fails with 400 `invalid_request`, and an unknown entitlement fails with 404 `entitlement_not_found`.

The first submission returns 201, records the reschedule, and immediately sets the entitlement's `valid_from`/`valid_to` to the new window. Resubmitting the same identifier with the same window returns 200 with the original reschedule and does not touch the times again; resubmitting it with a different new start or new end fails with 409 `reschedule_param_mismatch` and changes nothing.

A reschedule never changes any quota figure — quota total, seats total, used, unsettled reservations and available quota, plus the existing reservation, settlement, release, reversal and adjustment flows and their idempotency rules, all keep their behavior. The effective state is judged against the new window immediately: an entitlement that had not started yet accepts new reservations once the new window has begun; one moved to an end in the past stops accepting new reservations, while unsettled reservations can still be confirmed or released and confirmed ones reversed, exactly as after ordinary expiry. Adjustments are unaffected by the reschedule — their registration, `pending`/`applied` status and counting are driven solely by their own `effective_at`, and a past-due adjustment still counts toward the quota total even while the entitlement sits outside its validity window. Reschedules, reservations, settlements, reversals and adjustments on the same entitlement are all serialized by the same row lock.

### Query an entitlement

`GET /entitlements/{entitlement_id}` returns 200 with the quota breakdown, reservation list, adjustment list and reschedule list:

```json
{
  "entitlement_id": "team-alpha",
  "seats_total": 100,
  "quota_total": 150,
  "used_amount": 0,
  "reserved_amount": 0,
  "available_amount": 150,
  "status": "active",
  "valid_from": "2026-01-01T00:00:00Z",
  "valid_to": "2027-01-01T00:00:00Z",
  "reservations": [
    {"reservation_id": "sub-a-001", "amount": 30, "status": "confirmed", "settled_at": "...", "created_at": "..."},
    {"reservation_id": "corr-2026-0001", "amount": 30, "status": "reversed", "settled_at": "...", "created_at": "..."}
  ],
  "adjustments": [
    {"adjustment_id": "upsell-2026-0042", "delta": 50, "effective_at": "2026-06-01T00:00:00Z", "status": "applied", "applied_at": "...", "created_at": "..."}
  ],
  "reschedules": [
    {"reschedule_id": "postpone-launch-2026-007", "new_valid_from": "2026-02-01T00:00:00Z", "new_valid_to": "2027-02-01T00:00:00Z", "created_at": "..."}
  ]
}
```

`status` is `pending`, `active` or `expired` relative to the validity window. Each reservation entry reports its business identifier, amount, lifecycle status (`pending`, `confirmed`, `released` for reservations; `reversed` for corrective reversals) and settlement timestamp. `used_amount` is confirmed quota minus effective reversals, so a reversed reservation leaves the original confirmation and the reversal both visible while the quota figures reflect only the net effect. Each adjustment entry reports its business identifier, delta, effective time and whether it is `pending` or `applied`; `quota_total`, `available_amount` and the other figures count only applied adjustments, so they always match the quota actually in force. Each reschedule entry reports its business identifier, the new start and end of the validity window, and its creation time, listed in creation order; the view's top-level `valid_from`, `valid_to` and `status` always reflect the latest reschedule that has taken effect.

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
| `reschedule_param_mismatch` | 409 | Reschedule identifier reused with a different new start or new end |

## Consistency

Concurrent reservations, confirmations, releases, reversals, adjustments and reschedules on the same entitlement are serialized with a row lock in PostgreSQL, so used quota plus unsettled reservations never exceeds the total quota, repeated requests never grant, refund, adjust quota or rewrite the validity window twice, and failed requests leave no partial results. A reschedule only rewrites `valid_from`/`valid_to` — it never touches a quota figure, a reservation, a reversal or an adjustment record, and adjustments still apply solely by their own effective time. Due adjustments are folded into the quota total inside the same lock — by whichever operation or query touches the entitlement first after the effective time — so every adjustment applies exactly once and a decrease never crowds out occupied quota. All state lives in PostgreSQL, so a restart preserves every quota figure, reservation status, reversal record, adjustment record including its applied or pending state, reschedule record and the rescheduled validity window.

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
# Create an entitlement and occupy 80 of 100 seats (60 confirmed, 20 reserved).
curl -s -X POST 127.0.0.1:8080/entitlements -d '{"entitlement_id":"team-beta","quota_total":100,"valid_from":"2026-01-01T00:00:00Z","valid_to":"2027-01-01T00:00:00Z"}'
curl -s -X POST 127.0.0.1:8080/entitlements/team-beta/reservations -d '{"reservation_id":"sub-b-001","amount":60}'
curl -s -X POST 127.0.0.1:8080/entitlements/team-beta/reservations/sub-b-001/confirm
curl -s -X POST 127.0.0.1:8080/entitlements/team-beta/reservations -d '{"reservation_id":"sub-b-002","amount":20}'

# An increase due in the past applies immediately: quota_total becomes 150.
curl -s -X POST 127.0.0.1:8080/entitlements/team-beta/adjustments -d '{"adjustment_id":"upsell-001","delta":50,"effective_at":"2026-01-01T00:00:00Z"}'
curl -s 127.0.0.1:8080/entitlements/team-beta

# A decrease to 50 is due but 80 seats are occupied: it stays pending.
curl -s -X POST 127.0.0.1:8080/entitlements/team-beta/adjustments -d '{"adjustment_id":"clawback-001","delta":-100,"effective_at":"2026-01-01T00:00:00Z"}'
curl -s 127.0.0.1:8080/entitlements/team-beta   # quota_total still 150, adjustment listed as pending

# Free the occupancy; the pending decrease applies automatically, exactly once.
curl -s -X POST 127.0.0.1:8080/entitlements/team-beta/reservations/sub-b-002/release
curl -s -X POST 127.0.0.1:8080/entitlements/team-beta/reservations/sub-b-001/reverse -d '{"reversal_id":"corr-b-001"}'
curl -s 127.0.0.1:8080/entitlements/team-beta   # quota_total now 50, adjustment applied

# Replays and illegal adjustments are safe no-ops with stable error codes.
curl -s -X POST 127.0.0.1:8080/entitlements/team-beta/adjustments -d '{"adjustment_id":"upsell-001","delta":50,"effective_at":"2026-01-01T00:00:00Z"}'   # 200, original adjustment
curl -s -X POST 127.0.0.1:8080/entitlements/team-beta/adjustments -d '{"adjustment_id":"upsell-001","delta":60,"effective_at":"2026-01-01T00:00:00Z"}'   # 409 adjustment_param_mismatch
curl -s -X POST 127.0.0.1:8080/entitlements/team-beta/adjustments -d '{"adjustment_id":"clawback-002","delta":-50,"effective_at":"2026-01-01T00:00:00Z"}' # 422 insufficient_quota (total would reach 0)
curl -s -X POST 127.0.0.1:8080/entitlements/team-beta/adjustments -d '{"adjustment_id":"clawback-003","delta":0,"effective_at":"2026-01-01T00:00:00Z"}'   # 400 invalid_request
```

## Verifying the reschedule flow

With the server running as above, the full date-change cycle can be exercised with curl. The timestamp helpers below use the macOS `date` syntax; the fixed RFC 3339 strings can be substituted directly on other platforms.

```sh
PAST=$(date -u -v-1H +%Y-%m-%dT%H:%M:%SZ)     # one hour ago
FUTURE=$(date -u -v+2H +%Y-%m-%dT%H:%M:%SZ)   # two hours from now
EXPIRED=$(date -u -v-5M +%Y-%m-%dT%H:%M:%SZ)  # five minutes ago

# An entitlement that has not started yet rejects reservations.
curl -s -X POST 127.0.0.1:8080/entitlements -d '{"entitlement_id":"team-gamma","quota_total":100,"valid_from":"2026-11-01T00:00:00Z","valid_to":"2026-12-01T00:00:00Z"}'
curl -s -X POST 127.0.0.1:8080/entitlements/team-gamma/reservations -d '{"reservation_id":"sub-g-001","amount":30}'   # 422 entitlement_not_active

# First reschedule: 201, the window is rewritten immediately and reservations now work.
curl -s -X POST 127.0.0.1:8080/entitlements/team-gamma/reschedules -d "{\"reschedule_id\":\"move-enter\",\"new_valid_from\":\"$PAST\",\"new_valid_to\":\"$FUTURE\"}"
curl -s -X POST 127.0.0.1:8080/entitlements/team-gamma/reservations -d '{"reservation_id":"sub-g-001","amount":30}'    # 201

# An identical replay returns the original record (200) and does not touch the window;
# the same identifier with a different window fails atomically (409).
curl -s -X POST 127.0.0.1:8080/entitlements/team-gamma/reschedules -d "{\"reschedule_id\":\"move-enter\",\"new_valid_from\":\"$PAST\",\"new_valid_to\":\"$FUTURE\"}"
curl -s -X POST 127.0.0.1:8080/entitlements/team-gamma/reschedules -d '{"reschedule_id":"move-enter","new_valid_from":"2026-02-01T00:00:00Z","new_valid_to":"2027-02-01T00:00:00Z"}' # 409 reschedule_param_mismatch

# Move the end into the past: new reservations are rejected, settlement and
# reversal stay allowed, and no quota figure moves because of the reschedule.
curl -s -X POST 127.0.0.1:8080/entitlements/team-gamma/reschedules -d "{\"reschedule_id\":\"move-expire\",\"new_valid_from\":\"$PAST\",\"new_valid_to\":\"$EXPIRED\"}"
curl -s -X POST 127.0.0.1:8080/entitlements/team-gamma/reservations -d '{"reservation_id":"sub-g-002","amount":1}'      # 422 entitlement_not_active
curl -s -X POST 127.0.0.1:8080/entitlements/team-gamma/reservations/sub-g-001/release                                  # 200, still settles

# The view reports the latest window, status "expired" and both reschedule
# records in creation order; quota figures are unchanged by the reschedules.
curl -s 127.0.0.1:8080/entitlements/team-gamma

# Bad input and unknown targets use the same stable error envelope.
curl -s -X POST 127.0.0.1:8080/entitlements/team-gamma/reschedules -d "{\"reschedule_id\":\"move-bad\",\"new_valid_from\":\"$FUTURE\",\"new_valid_to\":\"$PAST\"}" # 400 invalid_request
curl -s -X POST 127.0.0.1:8080/entitlements/team-gamma/reschedules -d "{\"reschedule_id\":\"move-bad\",\"new_valid_to\":\"$FUTURE\"}"                              # 400 invalid_request
curl -s -X POST 127.0.0.1:8080/entitlements/team-missing/reschedules -d "{\"reschedule_id\":\"move-1\",\"new_valid_from\":\"$PAST\",\"new_valid_to\":\"$FUTURE\"}" # 404 entitlement_not_found
```

After restarting the server (Ctrl+C, then the same `go run` command), `GET /entitlements/team-gamma` still shows the rescheduled window, status `expired` and the reschedule history; replaying `move-expire` with its exact original timestamps returns 200 with the original record and changes nothing.

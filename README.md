# Entitlement service

Go HTTP service for team service entitlements: seat-quota grants with idempotent reservations (each tagged with its owning internal department), confirm/release settlement, corrective reversals of confirmed reservations, scheduled quota-total adjustments, idempotent validity-window reschedules, immediate idempotent allocation and recovery of the entitlement's seats, one-way expiry closure with a close-time accounting snapshot, caller-timed pause/resume of grant issuance, and idempotent disbursement and return of quota between the free pool and internal departments, persisted in PostgreSQL. Requires Go 1.27.1 and PostgreSQL 18.6.

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
{"reservation_id":"sub-a-001","department_id":"dept-platform","amount":30}
```

`reservation_id` is the caller-supplied business identifier, unique within the entitlement, and `department_id` identifies the internal department the seats are allocated to. The department identifier is required, uses the same character rules as the other business identifiers, and is an immutable reservation attribute set at creation. The first submission returns 201 and holds the quota. Resubmitting the same identifier with the same amount and department returns 200 with the original reservation and does not consume quota again; resubmitting it with a different amount or department fails with 409 `reservation_param_mismatch` and changes nothing. Reservations outside the validity window fail with 422 `entitlement_not_active`; insufficient quota fails with 422 `insufficient_quota` and leaves no partial state.

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

### Allocate or recover seats

`POST /entitlements/{entitlement_id}/seat-adjustments`

```json
{"seat_adjustment_id":"alloc-2026-0017","delta":20}
```

When the customer hands purchased seats down to internal departments, or takes seats back from departments, operations adjusts the seat total of an existing entitlement. `delta` is a non-zero integer: positive allocates seats and grows the seat total immediately, negative recovers seats and shrinks it. The change takes effect at once — there is no effective time. `seat_adjustment_id` is the caller-supplied business identifier, unique within the entitlement. The first submission returns 201, records the seat adjustment and sets `seats_total` to the new value. Resubmitting the same identifier with the same delta returns 200 with the original seat adjustment and does not move the seat total again; resubmitting it with a different delta fails with 409 `seat_adjustment_param_mismatch` and changes nothing. A zero or missing `delta` or a missing/illegal identifier fails with 400 `invalid_request`, and an unknown entitlement fails with 404 `entitlement_not_found`.

A recovery never takes seats that are already distributed to departments: if the adjusted seat total would be lower than the seats currently allocated to internal departments (the amount of every reservation that is pending or confirmed — released seats have been handed back; a corrective reversal leaves the original confirmation in place, so it still counts), the whole request fails with 422 `insufficient_quota` and writes nothing. A recovery that would drop the seat total to zero or below fails the same way. Seat adjustments never change `quota_total`, `used_amount`, `reserved_amount` or `available_amount`, and they do not affect reservations, settlements, reversals, quota adjustments or reschedules and their idempotency rules; all of them, including seat adjustments, are serialized by the same row lock.

### Expiry closure and the close-time snapshot

An entitlement is created in normal state. Once its `valid_to` has passed it is **closed exactly once, lazily and in the touching transaction**: the next write or view query on that entitlement stamps `closed_at` with the real time of that touch and registers one accounting record. A repeated touch never produces a second record or moves the close time. Closure is one-way — even if a reschedule later moves the entitlement back into a window covering the current time, it stays closed.

At closure the service writes a **closure snapshot**, keyed by the entitlement identifier (unique within the entitlement), capturing the actual figures at that instant: `seats_total`, `quota_total`, `used_amount`, the unsettled `reserved_amount`, `available_amount`, the `valid_from`/`valid_to` in force and `closed_at`. Due quota adjustments are folded in first, so the snapshot reflects the state really in force. The snapshot is a frozen record: later settlements change the live figures but never rewrite the snapshot.

Once closed, an entitlement rejects new reservations, quota adjustments, seat adjustments, reschedules and pause/resume registrations — each such request fails as a whole with 409 `entitlement_closed` and writes nothing. In-flight reservations can still be confirmed or released, confirmed reservations can still be corrected with a reversal, and an idempotent replay of a reservation, adjustment, reschedule or seat adjustment created before closure still returns its original record (200) without applying anything again.

`status` in the view is `closed` after closure; before closure it stays the window-relative `pending`/`active`/`expired`. Note the distinction: `expired` is a window label (a reschedule can move an entitlement from expired back to active), while `closed` is the permanent lifecycle state.

### Pause and resume issuance

Within the validity period, operations can suspend and resume grant issuance with caller-supplied business identifiers and times. Pause and resume never change any quota figure, seat figure or the validity window.

`POST /entitlements/{entitlement_id}/pauses`

```json
{"pause_id":"freeze-2026-0031","paused_at":"2026-06-01T00:00:00Z"}
```

`POST /entitlements/{entitlement_id}/resumes`

```json
{"resume_id":"unfreeze-2026-0031","resumed_at":"2026-06-03T00:00:00Z"}
```

`pause_id`/`resume_id` use the same character rules as the other business identifiers. They are unique within the entitlement and share one namespace across the two kinds (a single identifier denotes one event, either a pause or a resume). The first submission returns 201; resubmitting the same identifier with the same kind and time returns 200 with the original record and does not take effect again; resubmitting it with a different kind or time fails with 409 `pause_resume_param_mismatch` and changes nothing.

Time rules (validated strictly, so a bad time fails with 400 `invalid_request` and writes nothing): the pause time must be no earlier than `valid_from` and no later than `valid_to` (both bounds inclusive); the resume time must be strictly later than the matching pause time and no later than `valid_to`. Pausing an already-paused entitlement fails with 409 `entitlement_already_paused`; resuming one that is not paused fails with 409 `entitlement_not_paused`; registering either event on a closed entitlement fails with 409 `entitlement_closed` (apart from an identical replay). Pausing or resuming an unknown entitlement fails with 404 `entitlement_not_found`.

While paused, new reservations are refused with 422 `entitlement_paused`. Confirmation and release of in-flight reservations and corrective reversals are unaffected by a pause; quota and seat adjustments are not issuance and remain allowed. After a resume, new reservations are accepted again, and a fresh pause is then permitted.

### Disburse quota to a department

`POST /entitlements/{entitlement_id}/disbursements`

```json
{"disbursement_id":"alloc-dept-2026-0007","department_id":"dept-platform","amount":40}
```

When operations or implementation hands part of the purchased quota down to an internal department, they disburse from the entitlement's free pool. `disbursement_id` is the caller-supplied business identifier, unique within the entitlement; `department_id` identifies the internal department and uses the same character rules as the other business identifiers; `amount` must be a positive integer. The first submission returns 201 with the record and immediately occupies free quota — the available quota minus what departments already hold. Resubmitting the same identifier with the same department and amount returns 200 with the original record and does not occupy quota again; resubmitting it with a different department or amount fails with 409 `disbursement_param_mismatch` and changes nothing. A missing or illegal identifier, department or amount fails with 400 `invalid_request`, an unknown entitlement with 404 `entitlement_not_found`, and a disbursement larger than the free pool fails as a whole with 422 `insufficient_quota` — every failure writes nothing.

A disbursement creates no reservation and changes no quota or seat figure: `quota_total`, `used_amount`, `reserved_amount`, `available_amount` and `seats_total` all stay exactly as they were, and the reservation, settlement, release, reversal, quota adjustment, seat adjustment, reschedule and pause/resume flows and their idempotency rules are unaffected. Quota a department has returned is free again and can be disbursed once more.

### Return quota from a department

`POST /entitlements/{entitlement_id}/returns`

```json
{"return_id":"recall-dept-2026-0003","department_id":"dept-platform","amount":15}
```

When operations takes quota back from a department, the return hands department-held quota back to the free pool. `return_id` is the caller-supplied business identifier, unique within the entitlement (and sharing one namespace with the disbursement identifiers: a single identifier denotes one record, either a disbursement or a return). The first submission returns 201 with the record and immediately frees the quota. Resubmitting the same identifier with the same department and amount returns 200 with the original record and does not free quota again; resubmitting it with a different department or amount fails with 409 `return_param_mismatch` and changes nothing. A return that would drop the department's current holding below zero fails as a whole with 422 `insufficient_quota` and writes nothing; missing or illegal parameters fail with 400 `invalid_request`, an unknown entitlement with 404 `entitlement_not_found`.

Both responses and the view's record list describe each movement with its business `record_id`, `department_id`, `amount`, `type` (`disburse` or `return`) and `created_at`. Disbursement and return are reconciliation, not grant issuance: like settlement and reversal they stay allowed after the entitlement closes, and they are never blocked by a pause.

### Query an entitlement

`GET /entitlements/{entitlement_id}` returns 200 with the quota breakdown, reservation list, adjustment list, reschedule list and seat adjustment list:

```json
{
  "entitlement_id": "team-alpha",
  "seats_total": 120,
  "quota_total": 150,
  "used_amount": 0,
  "reserved_amount": 0,
  "available_amount": 150,
  "status": "active",
  "valid_from": "2026-01-01T00:00:00Z",
  "valid_to": "2027-01-01T00:00:00Z",
  "pause_resume_events": [
    {"id": "freeze-2026-0031", "type": "pause", "at": "2026-06-01T00:00:00Z", "created_at": "..."},
    {"id": "unfreeze-2026-0031", "type": "resume", "at": "2026-06-03T00:00:00Z", "created_at": "..."}
  ],
  "reservations": [
    {"reservation_id": "sub-a-001", "department_id": "dept-platform", "amount": 30, "status": "confirmed", "settled_at": "...", "created_at": "..."},
    {"reservation_id": "corr-2026-0001", "amount": 30, "status": "reversed", "settled_at": "...", "created_at": "..."}
  ],
  "adjustments": [
    {"adjustment_id": "upsell-2026-0042", "delta": 50, "effective_at": "2026-06-01T00:00:00Z", "status": "applied", "applied_at": "...", "created_at": "..."}
  ],
  "reschedules": [
    {"reschedule_id": "postpone-launch-2026-007", "new_valid_from": "2026-02-01T00:00:00Z", "new_valid_to": "2027-02-01T00:00:00Z", "created_at": "..."}
  ],
  "seat_adjustments": [
    {"seat_adjustment_id": "alloc-2026-0017", "delta": 20, "seat_total": 120, "created_at": "..."}
  ],
  "department_accounts": [
    {"department_id": "dept-platform", "held_amount": 25, "disbursed_amount": 40, "returned_amount": 15}
  ],
  "department_quota_records": [
    {"record_id": "alloc-dept-2026-0007", "department_id": "dept-platform", "amount": 40, "type": "disburse", "created_at": "..."},
    {"record_id": "recall-dept-2026-0003", "department_id": "dept-platform", "amount": 15, "type": "return", "created_at": "..."}
  ]
}
```

`status` is `pending`, `active` or `expired` relative to the validity window, or `closed` once the entitlement has gone through its one-way expiry closure. When closed the view also carries `closed_at` and a `closure_snapshot` object with the close-time figures (seats total, quota total, used, unsettled reserved, available, the validity window and the close time); before closure both are omitted (`closure_snapshot` is empty). The snapshot is frozen at closure and is not updated by later settlements. Each reservation entry reports its business identifier, owning `department_id`, amount, lifecycle status (`pending`, `confirmed`, `released` for reservations; `reversed` for corrective reversals) and settlement timestamp. `used_amount` is confirmed quota minus effective reversals, so a reversed reservation leaves the original confirmation and the reversal both visible while the quota figures reflect only the net effect. Each adjustment entry reports its business identifier, delta, effective time and whether it is `pending` or `applied`; `quota_total`, `available_amount` and the other figures count only applied adjustments, so they always match the quota actually in force. Each reschedule entry reports its business identifier, the new start and end of the validity window, and its creation time, listed in creation order; the view's top-level `valid_from`, `valid_to` and window-relative status always reflect the latest reschedule that has taken effect (a closed entitlement stays `closed` regardless). Each seat adjustment entry reports its business identifier, the seat delta, its creation time and `seat_total`, the cumulative seat total after that adjustment — deltas summed in creation order on top of the entitlement's initial seats; the list is ordered by creation time. Seat adjustments move only `seats_total`, never any quota figure. `pause_resume_events` lists every pause and resume in registration order, each with its business `id`, `type` (`pause` or `resume`) and caller-supplied `at` time; it is empty (`[]`) when none have been registered. `department_accounts` lists every department that ever received a disbursement, ordered by department identifier, with its current `held_amount` (disbursed minus returned — a department that returned everything stays listed with a zero holding) and the cumulative `disbursed_amount` and `returned_amount`. `department_quota_records` lists every disbursement and return in creation order, each with its business `record_id`, `department_id`, `amount`, `type` (`disburse` or `return`) and `created_at`; both lists are empty (`[]`) when no movement has been recorded. Department movements never change any quota or seat figure, so all figures above ignore them.

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
| `seat_adjustment_param_mismatch` | 409 | Seat adjustment identifier reused with a different delta |
| `entitlement_closed` | 409 | Entitlement has closed after expiry and no longer accepts this operation |
| `pause_resume_param_mismatch` | 409 | Pause/resume identifier reused with a different kind or time |
| `entitlement_paused` | 422 | New reservation refused because issuance is paused |
| `entitlement_already_paused` | 409 | Pause requested on an entitlement that is already paused |
| `entitlement_not_paused` | 409 | Resume requested on an entitlement that is not paused |
| `disbursement_param_mismatch` | 409 | Disbursement identifier reused with a different department or amount |
| `return_param_mismatch` | 409 | Return identifier reused with a different department or amount |

## Consistency

Concurrent reservations, confirmations, releases, reversals, adjustments, reschedules and seat adjustments on the same entitlement are serialized with a row lock in PostgreSQL, so used quota plus unsettled reservations never exceeds the total quota, the seat total never drops below the seats allocated to departments, repeated requests never grant, refund, adjust quota, rewrite the validity window or move the seat total twice, and failed requests leave no partial results. A reschedule only rewrites `valid_from`/`valid_to` — it never touches a quota figure, a reservation, a reversal or an adjustment record, and adjustments still apply solely by their own effective time. A seat adjustment only rewrites `seats_total` — it never touches a quota figure, a reservation, a reversal, a quota adjustment or a reschedule; the cumulative `seat_total` stored on each record always matches the entitlement's current `seats_total`. Due quota adjustments are folded into the quota total inside the same lock — by whichever operation or query touches the entitlement first after the effective time — so every adjustment applies exactly once and a decrease never crowds out occupied quota. Expiry closure happens in that same lock, on the first write or view after `valid_to`: it stamps one `closed_at` and inserts one closure snapshot atomically, so concurrent or repeated touches close it exactly once, the snapshot reads the figures actually in force at that instant, and the close either fully happens or not at all. Closure, pause and resume serialize with reservations, settlements, reversals, adjustments, reschedules and seat adjustments on the same row lock; pause and resume change only the pause flag and append their event records, never a quota figure, seat figure or the window, and failed requests leave no partial results. Department disbursements and returns run on that same row lock too: a disbursement occupies free quota (available quota minus department holdings) only when the pool covers it, a return never drops a department's holding below zero, replays never occupy or free quota twice, and neither movement ever touches a quota figure, a seat figure, a reservation or any other record. All state lives in PostgreSQL, so a restart preserves every quota and seat figure, reservation including its owning department, reversal record, adjustment record including its applied or pending state, reschedule record including the rescheduled window, every seat adjustment including its cumulative total, the closed state with its close time and frozen snapshot, the full pause/resume history including whether issuance is currently paused, and every department quota record with the per-department holdings derived from it.

The local setup uses a trusted loopback connection and C collation without ICU. Use separate database directories, database ports and HTTP ports when running multiple copies.

## Verifying the reversal flow

With the server running as above, the full correction cycle can be exercised with curl:

```sh
# Create an entitlement and confirm a reservation of 30 seats.
curl -s -X POST 127.0.0.1:8080/entitlements -d '{"entitlement_id":"team-alpha","quota_total":100,"valid_from":"2026-01-01T00:00:00Z","valid_to":"2027-01-01T00:00:00Z"}'
curl -s -X POST 127.0.0.1:8080/entitlements/team-alpha/reservations -d '{"reservation_id":"sub-a-001","department_id":"dept-platform","amount":30}'
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
curl -s -X POST 127.0.0.1:8080/entitlements/team-beta/reservations -d '{"reservation_id":"sub-b-001","department_id":"dept-sales","amount":60}'
curl -s -X POST 127.0.0.1:8080/entitlements/team-beta/reservations/sub-b-001/confirm
curl -s -X POST 127.0.0.1:8080/entitlements/team-beta/reservations -d '{"reservation_id":"sub-b-002","department_id":"dept-sales","amount":20}'

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
curl -s -X POST 127.0.0.1:8080/entitlements/team-gamma/reservations -d '{"reservation_id":"sub-g-001","department_id":"dept-sales","amount":30}'   # 422 entitlement_not_active

# First reschedule: 201, the window is rewritten immediately and reservations now work.
curl -s -X POST 127.0.0.1:8080/entitlements/team-gamma/reschedules -d "{\"reschedule_id\":\"move-enter\",\"new_valid_from\":\"$PAST\",\"new_valid_to\":\"$FUTURE\"}"
curl -s -X POST 127.0.0.1:8080/entitlements/team-gamma/reservations -d '{"reservation_id":"sub-g-001","department_id":"dept-sales","amount":30}'    # 201

# An identical replay returns the original record (200) and does not touch the window;
# the same identifier with a different window fails atomically (409).
curl -s -X POST 127.0.0.1:8080/entitlements/team-gamma/reschedules -d "{\"reschedule_id\":\"move-enter\",\"new_valid_from\":\"$PAST\",\"new_valid_to\":\"$FUTURE\"}"
curl -s -X POST 127.0.0.1:8080/entitlements/team-gamma/reschedules -d '{"reschedule_id":"move-enter","new_valid_from":"2026-02-01T00:00:00Z","new_valid_to":"2027-02-01T00:00:00Z"}' # 409 reschedule_param_mismatch

# Move the end into the past: new reservations are rejected, settlement and
# reversal stay allowed, and no quota figure moves because of the reschedule.
curl -s -X POST 127.0.0.1:8080/entitlements/team-gamma/reschedules -d "{\"reschedule_id\":\"move-expire\",\"new_valid_from\":\"$PAST\",\"new_valid_to\":\"$EXPIRED\"}"
curl -s -X POST 127.0.0.1:8080/entitlements/team-gamma/reservations -d '{"reservation_id":"sub-g-002","department_id":"dept-sales","amount":1}'      # 422 entitlement_not_active
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

## Verifying the seat allocation and recovery flow

With the server running as above, the full seat-adjustment cycle can be exercised with curl:

```sh
# 100 seats. Hand 60 to dept-sales (confirmed) and hold 20 more for dept-support.
curl -s -X POST 127.0.0.1:8080/entitlements -d '{"entitlement_id":"team-delta","seats_total":100,"quota_total":100,"valid_from":"2026-01-01T00:00:00Z","valid_to":"2027-01-01T00:00:00Z"}'
curl -s -X POST 127.0.0.1:8080/entitlements/team-delta/reservations -d '{"reservation_id":"sub-d-001","department_id":"dept-sales","amount":60}'
curl -s -X POST 127.0.0.1:8080/entitlements/team-delta/reservations/sub-d-001/confirm
curl -s -X POST 127.0.0.1:8080/entitlements/team-delta/reservations -d '{"reservation_id":"sub-d-002","department_id":"dept-support","amount":20}'
# Replaying a reservation with a different department is a mismatch.
curl -s -X POST 127.0.0.1:8080/entitlements/team-delta/reservations -d '{"reservation_id":"sub-d-002","department_id":"dept-sales","amount":20}'    # 409 reservation_param_mismatch

# Allocate 50 more seats: seats_total becomes 150 immediately.
curl -s -X POST 127.0.0.1:8080/entitlements/team-delta/seat-adjustments -d '{"seat_adjustment_id":"alloc-001","delta":50}'
# Recover 60: 150 -> 90, still at or above the 80 seats held by departments.
curl -s -X POST 127.0.0.1:8080/entitlements/team-delta/seat-adjustments -d '{"seat_adjustment_id":"recover-001","delta":-60}'
# Replays are safe: same delta returns the original record, a different one fails.
curl -s -X POST 127.0.0.1:8080/entitlements/team-delta/seat-adjustments -d '{"seat_adjustment_id":"alloc-001","delta":50}'    # 200, original result
curl -s -X POST 127.0.0.1:8080/entitlements/team-delta/seat-adjustments -d '{"seat_adjustment_id":"alloc-001","delta":40}'    # 409 seat_adjustment_param_mismatch

# 80 seats are allocated, so recovering past the floor, or to zero, fails and writes nothing.
curl -s -X POST 127.0.0.1:8080/entitlements/team-delta/seat-adjustments -d '{"seat_adjustment_id":"recover-crowd","delta":-11}'  # 422 insufficient_quota (90 -> 79 < 80)
curl -s -X POST 127.0.0.1:8080/entitlements/team-delta/seat-adjustments -d '{"seat_adjustment_id":"recover-zero","delta":-90}'   # 422 insufficient_quota (to zero)

# dept-support hands its 20 seats back; the floor drops to the 60 still with dept-sales.
curl -s -X POST 127.0.0.1:8080/entitlements/team-delta/reservations/sub-d-002/release
curl -s -X POST 127.0.0.1:8080/entitlements/team-delta/seat-adjustments -d '{"seat_adjustment_id":"recover-002","delta":-30}'   # 90 -> 60, exactly the floor
curl -s -X POST 127.0.0.1:8080/entitlements/team-delta/seat-adjustments -d '{"seat_adjustment_id":"recover-003","delta":-1}'    # 422 insufficient_quota

# A correction refunds dept-sales' quota but leaves its confirmation in place,
# so those 60 seats still count as allocated to the department.
curl -s -X POST 127.0.0.1:8080/entitlements/team-delta/reservations/sub-d-001/reverse -d '{"reversal_id":"corr-d-001"}'
curl -s -X POST 127.0.0.1:8080/entitlements/team-delta/seat-adjustments -d '{"seat_adjustment_id":"recover-004","delta":-1}'    # still 422 insufficient_quota

# The view shows the seat-adjustment ledger with cumulative totals; quota figures never moved.
curl -s 127.0.0.1:8080/entitlements/team-delta
# seats_total 60, quota_total/available_amount 100, seat_adjustments:
#   alloc-001 +50 -> 150, recover-001 -60 -> 90, recover-002 -30 -> 60

# Bad input and unknown targets use the same stable error envelope.
curl -s -X POST 127.0.0.1:8080/entitlements/team-delta/seat-adjustments -d '{"seat_adjustment_id":"bad","delta":0}'          # 400 invalid_request
curl -s -X POST 127.0.0.1:8080/entitlements/team-delta/seat-adjustments -d '{"seat_adjustment_id":"bad"}'                 # 400 invalid_request
curl -s -X POST 127.0.0.1:8080/entitlements/team-missing/seat-adjustments -d '{"seat_adjustment_id":"bad","delta":1}'     # 404 entitlement_not_found
```

After restarting the server, `GET /entitlements/team-delta` still shows `seats_total` 60 and the same cumulative seat-adjustment list; replaying `recover-002` with `delta -30` returns 200 with the original record and leaves the total at 60.

## Verifying expiry closure and the snapshot

The timestamps below use the macOS `date` syntax; fixed RFC 3339 strings work on any platform.

```sh
PAST=$(date -u -v-1H +%Y-%m-%dT%H:%M:%SZ)
FUT=$(date -u -v+1H +%Y-%m-%dT%H:%M:%SZ)
EXPIRED=$(date -u -v-2M +%Y-%m-%dT%H:%M:%SZ)

# 100 seats/quota, one 25-seat in-flight hold (reserved 25, available 75).
curl -s -X POST 127.0.0.1:8080/entitlements -d "{\"entitlement_id\":\"team-omega\",\"quota_total\":100,\"valid_from\":\"$PAST\",\"valid_to\":\"$FUT\"}"
curl -s -X POST 127.0.0.1:8080/entitlements/team-omega/reservations -d '{"reservation_id":"sub-o-001","department_id":"dept-sales","amount":25}'

# Move the end into the past while the entitlement is still open: allowed,
# and no quota figure moves.
curl -s -X POST 127.0.0.1:8080/entitlements/team-omega/reschedules -d "{\"reschedule_id\":\"end-early\",\"new_valid_from\":\"$PAST\",\"new_valid_to\":\"$EXPIRED\"}"

# The next touch (this view) performs the one-time closure: status "closed",
# closed_at set, and closure_snapshot freezes seats/quota/used/reserved/available.
curl -s 127.0.0.1:8080/entitlements/team-omega

# New reservations, adjustments, seat adjustments, reschedules and pauses now
# fail as a whole with 409 entitlement_closed and write nothing.
curl -s -X POST 127.0.0.1:8080/entitlements/team-omega/reservations -d '{"reservation_id":"sub-o-new","department_id":"dept-sales","amount":1}'     # 409 entitlement_closed
curl -s -X POST 127.0.0.1:8080/entitlements/team-omega/adjustments -d "{\"adjustment_id\":\"a-new\",\"delta\":1,\"effective_at\":\"$PAST\"}"     # 409 entitlement_closed
curl -s -X POST 127.0.0.1:8080/entitlements/team-omega/seat-adjustments -d '{"seat_adjustment_id":"s-new","delta":1}'                            # 409 entitlement_closed
curl -s -X POST 127.0.0.1:8080/entitlements/team-omega/reschedules -d "{\"reschedule_id\":\"reopen\",\"new_valid_from\":\"$PAST\",\"new_valid_to\":\"$FUT\"}" # 409, one-way
curl -s -X POST 127.0.0.1:8080/entitlements/team-omega/pauses -d "{\"pause_id\":\"p-new\",\"paused_at\":\"$PAST\"}"                               # 409 entitlement_closed

# The in-flight hold still settles and a confirmed reservation still reverses.
curl -s -X POST 127.0.0.1:8080/entitlements/team-omega/reservations/sub-o-001/confirm
curl -s -X POST 127.0.0.1:8080/entitlements/team-omega/reservations/sub-o-001/reverse -d '{"reversal_id":"corr-o-001"}'

# Live figures moved (used/reserved now 0), but closure_snapshot still shows the
# close-instant reserved_amount 25 and available_amount 75; repeated touches
# never add a second snapshot.
curl -s 127.0.0.1:8080/entitlements/team-omega

# An idempotent replay of a pre-close reservation returns its original record.
curl -s -X POST 127.0.0.1:8080/entitlements/team-omega/reservations -d '{"reservation_id":"sub-o-001","department_id":"dept-sales","amount":25}'   # 200, original
```

After restarting the server, `GET /entitlements/team-omega` still returns `status` `closed`, the same `closed_at`, the same frozen snapshot and one snapshot row; reopening and new grants stay refused.

## Verifying pause and resume

```sh
PAST=$(date -u -v-1H +%Y-%m-%dT%H:%M:%SZ)
FUT=$(date -u -v+1H +%Y-%m-%dT%H:%M:%SZ)
LATER=$(date -u -v+2H +%Y-%m-%dT%H:%M:%SZ)
PAT=$(date -u -v-5M +%Y-%m-%dT%H:%M:%SZ)
RAT=$(date -u -v-1M +%Y-%m-%dT%H:%M:%SZ)

curl -s -X POST 127.0.0.1:8080/entitlements -d "{\"entitlement_id\":\"team-sigma\",\"quota_total\":100,\"valid_from\":\"$PAST\",\"valid_to\":\"$FUT\"}"
# An in-flight hold created before the pause keeps settling afterwards.
curl -s -X POST 127.0.0.1:8080/entitlements/team-sigma/reservations -d '{"reservation_id":"sub-s-001","department_id":"dept-sales","amount":20}'

# Pause issuance: 201; the identical replay returns 200 and does not re-apply.
curl -s -X POST 127.0.0.1:8080/entitlements/team-sigma/pauses -d "{\"pause_id\":\"freeze-1\",\"paused_at\":\"$PAT\"}"
curl -s -X POST 127.0.0.1:8080/entitlements/team-sigma/pauses -d "{\"pause_id\":\"freeze-1\",\"paused_at\":\"$PAT\"}"

# New grants are refused while paused; in-flight settlement still works.
curl -s -X POST 127.0.0.1:8080/entitlements/team-sigma/reservations -d '{"reservation_id":"sub-s-new","department_id":"dept-sales","amount":1}'   # 422 entitlement_paused
curl -s -X POST 127.0.0.1:8080/entitlements/team-sigma/reservations/sub-s-001/release                              # 200

# A second pause is a state violation; bad/outside-window times are 400.
curl -s -X POST 127.0.0.1:8080/entitlements/team-sigma/pauses -d "{\"pause_id\":\"freeze-2\",\"paused_at\":\"$RAT\"}"   # 409 entitlement_already_paused
curl -s -X POST 127.0.0.1:8080/entitlements/team-sigma/pauses -d "{\"pause_id\":\"bad\",\"paused_at\":\"$LATER\"}"      # 400 invalid_request (after valid_to)

# Resume: 201. The resume time must be later than the pause time.
curl -s -X POST 127.0.0.1:8080/entitlements/team-sigma/resumes -d "{\"resume_id\":\"unfreeze-1\",\"resumed_at\":\"$RAT\"}"
curl -s -X POST 127.0.0.1:8080/entitlements/team-sigma/resumes -d "{\"resume_id\":\"again\",\"resumed_at\":\"$FUT\"}"   # 409 entitlement_not_paused
curl -s -X POST 127.0.0.1:8080/entitlements/team-sigma/pauses -d "{\"pause_id\":\"freeze-1\",\"paused_at\":\"$RAT\"}"   # 409 pause_resume_param_mismatch
curl -s -X POST 127.0.0.1:8080/entitlements/nope/pauses -d "{\"pause_id\":\"p\",\"paused_at\":\"$PAT\"}"               # 404 entitlement_not_found

# Grants flow again, and the view lists the events in registration order with
# id, type ("pause"/"resume") and at; no quota figure moved.
curl -s -X POST 127.0.0.1:8080/entitlements/team-sigma/reservations -d '{"reservation_id":"sub-s-002","department_id":"dept-sales","amount":10}'
curl -s 127.0.0.1:8080/entitlements/team-sigma
```

After restarting the server, `GET /entitlements/team-sigma` still shows the same `pause_resume_events` and the current paused flag; `freeze-1` replays as 200 without taking effect again, and resuming a persisted pause still works.

## Verifying the department disbursement and return flow

With the server running as above, the full disburse/return cycle can be exercised with curl:

```sh
# 100 seats/quota; hold 30 with an in-flight reservation (available 70).
curl -s -X POST 127.0.0.1:8080/entitlements -d '{"entitlement_id":"team-eta","quota_total":100,"valid_from":"2026-01-01T00:00:00Z","valid_to":"2027-01-01T00:00:00Z"}'
curl -s -X POST 127.0.0.1:8080/entitlements/team-eta/reservations -d '{"reservation_id":"sub-e-001","department_id":"dept-ops","amount":30}'

# Disburse 40 to dept-platform: 201, the free pool drops to 70 - 40 = 30.
curl -s -X POST 127.0.0.1:8080/entitlements/team-eta/disbursements -d '{"disbursement_id":"alloc-e-001","department_id":"dept-platform","amount":40}'
# The identical replay returns 200 with the original record and occupies nothing again.
curl -s -X POST 127.0.0.1:8080/entitlements/team-eta/disbursements -d '{"disbursement_id":"alloc-e-001","department_id":"dept-platform","amount":40}'
# Same identifier with different parameters fails atomically.
curl -s -X POST 127.0.0.1:8080/entitlements/team-eta/disbursements -d '{"disbursement_id":"alloc-e-001","department_id":"dept-platform","amount":50}'  # 409 disbursement_param_mismatch
# The free pool is 30, so 31 fails and writes nothing; 30 succeeds.
curl -s -X POST 127.0.0.1:8080/entitlements/team-eta/disbursements -d '{"disbursement_id":"alloc-e-002","department_id":"dept-support","amount":31}'   # 422 insufficient_quota
curl -s -X POST 127.0.0.1:8080/entitlements/team-eta/disbursements -d '{"disbursement_id":"alloc-e-002","department_id":"dept-support","amount":30}'   # 201

# Return 15 from dept-platform: 201, its holding drops to 25 and the free pool rises to 15.
curl -s -X POST 127.0.0.1:8080/entitlements/team-eta/returns -d '{"return_id":"recall-e-001","department_id":"dept-platform","amount":15}'
curl -s -X POST 127.0.0.1:8080/entitlements/team-eta/returns -d '{"return_id":"recall-e-001","department_id":"dept-platform","amount":15}'            # 200, original record
curl -s -X POST 127.0.0.1:8080/entitlements/team-eta/returns -d '{"return_id":"recall-e-001","department_id":"dept-platform","amount":10}'            # 409 return_param_mismatch
# Returning more than the 25 held fails as a whole; returned quota can be disbursed again.
curl -s -X POST 127.0.0.1:8080/entitlements/team-eta/returns -d '{"return_id":"recall-e-002","department_id":"dept-platform","amount":26}'            # 422 insufficient_quota
curl -s -X POST 127.0.0.1:8080/entitlements/team-eta/disbursements -d '{"disbursement_id":"alloc-e-003","department_id":"dept-ops","amount":15}'      # 201

# The view shows the department ledger and the record list; quota figures never moved.
curl -s 127.0.0.1:8080/entitlements/team-eta
# quota_total 100, reserved_amount 30, available_amount 70; department_accounts:
#   dept-platform held 25 (disbursed 40, returned 15), dept-support held 30, dept-ops held 15
# department_quota_records lists alloc-e-001, alloc-e-002, recall-e-001, alloc-e-003 in creation order.

# Bad input and unknown targets use the same stable error envelope.
curl -s -X POST 127.0.0.1:8080/entitlements/team-eta/disbursements -d '{"disbursement_id":"bad","department_id":"dept-ops"}'                            # 400 invalid_request
curl -s -X POST 127.0.0.1:8080/entitlements/team-eta/returns -d '{"return_id":"bad","department_id":"dept ops!","amount":1}'                            # 400 invalid_request
curl -s -X POST 127.0.0.1:8080/entitlements/team-missing/disbursements -d '{"disbursement_id":"d1","department_id":"dept-ops","amount":1}'              # 404 entitlement_not_found
```

After restarting the server (Ctrl+C, then the same `go run` command), `GET /entitlements/team-eta` still shows the same `department_accounts` and `department_quota_records`; replaying `alloc-e-001` or `recall-e-001` with their exact original parameters returns 200 with the original record and moves nothing again.

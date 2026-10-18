# Audible download licenses

Sync reads library metadata without requesting a download license for each title.
A title appearing in the library does not establish that Audible will grant a
license. New titles remain eligible to queue; the download checks their license.
Sync preserves existing file metadata and unavailable reasons. It does not clear
historical denials in bulk.

The ASIN diagnostic lookup also skips licenses by default. Select **Request a
download license too**, or use `?check_license=true`, to make an explicit check.
That check shares the download client's cache and cooldown.

## Request handling

License POSTs run one at a time per customer and marketplace, including requests
from separate registrations. Library reads and CDN transfers bypass this gate.

- Concurrent identical requests share a response, with a separate body per caller.
- Other title denials stay in memory for five minutes. Their key includes the
  URL, body, and a hash of the ADP token. New credentials bypass those denials.
- Granted licenses are shared only among concurrent callers. Download URLs and
  key material are not persisted by this cache.

## Refusal pauses and reconnecting

`CustomerThrottled` in a license refusal pauses requests from **that device
registration**. The code alone does not establish an account-wide rate limit.
Registration identity uses customer, marketplace, device type, and device serial.
For older credentials without a serial, a hash of the ADP token provides the
fallback identity. Bearer access-token refreshes do not change this identity.

A successful fresh sign-in with a new registration, including iPhone → Android,
can make its first request without inheriting the old registration’s refusal
pause. The old pause remains in place for the old registration. Requests stay
serialized; if the new registration also receives `CustomerThrottled`, it starts
its own pause and other queued requests stop without another upstream attempt.
Deferred downloads resume against the account’s current connection.

HTTP 429 starts a **customer-and-marketplace pause** shared by registrations.
Reconnecting does not bypass it. Both kinds of pause survive restart, and a
longer `Retry-After` is honored. Shared in-memory HTTP 429 state also protects
other registrations when saving to the database fails; restart persistence
cannot be guaranteed after a failed save.

The initial 15-minute delay and doubling up to 24 hours are Audplexus policy,
not Audible’s stated recovery times. Backoff is tracked separately for the two
kinds of refusal. A granted license clears expired backoff for the current
registration and customer. It does not clear another registration’s pause.

Earlier draft builds stored unclassified customer cooldowns. This revision uses
separate registration and HTTP 429 settings keys and ignores those old records:
they cannot identify which registration failed or whether HTTP 429 occurred.
Requests remain serialized when the new policy makes its first attempt.

The gate assumes one server process. Multiple processes sharing a database do
not share the in-memory permit. Settings keys contain hashes, not raw identities.

## Deferred downloads

A paused download moves to `deferred`, remains visible under **Waiting**, and
shows its retry time. It keeps its existing files and its book stays queued.
Workers skip deferred jobs, so another account can continue. They resume deferred
jobs when the current registration and customer pauses permit another attempt.
Cancellation and queue refills include deferred jobs. Automatic resume requires the AccountManager used by the
server; embedded callers using only a standalone SDK client do not have it.

The new status uses the existing text column; there is no schema migration. An
older release does not recognize `deferred`. Before downgrading, pause workers
and cancel these jobs in the Pipeline UI, or change only their queue status back
to `pending` while the server is stopped. An older release will not enforce this
cooldown policy.

This change reduces speculative requests and repeated attempts during a known
cooldown. It does not establish Audible's rate limit or clear a throttle that
Audible has already applied. Tests use synthetic accounts and responses; they
do not contact Audible.

## Observe requests

At the normal Info log level, look for license messages in Diagnostics logs.
Expand a row to see component `audible_license` and its structured fields.
Container logs and the rotating app log contain the same JSON events, so a
log viewer can filter them by component. File logging must be enabled for the
rotating app log.

| `license_event` | Meaning |
| --- | --- |
| `request_sent` | A license POST reached the upstream transport. |
| `request_result` | The result was granted, denied, customer_throttled, http_429, unknown, or failed to read. |
| `shared` | A concurrent caller joined an existing request. |
| `denial_cache_hit` | A recent denial answered the request without a new POST. |
| `cooldown_started` | A refusal set or extended a pause; reason distinguishes customer_throttled from http_429. |
| `cooldown_skip` | An Audplexus pause prevented a POST. |
| `cooldown_cleared` | A granted license cleared the previous backoff. |

`account_key` identifies the customer and marketplace by hash;
`registration_key` identifies the registration by hash. Logs omit
ADP tokens, request signatures, URLs, response bodies, and license keys. No raw
Audible debug logging is needed.

To check sync, use an idle download queue and compare the sync start and finish
timestamps: there should be no `request_sent` events in that interval. Active
downloads can legitimately request licenses during a sync. Default ASIN lookups
should also produce no license events. For a paused download, expect one
`request_sent`, a `customer_throttled` or `http_429` result, and `cooldown_started`.
Later attempts before
`retry_after` should show `cooldown_skip` without another `request_sent`.

## Show the issue where the user is working

An active pause for the current registration or customer appears in a warning
above each page and on the Audible account in Settings. The sidebar says
**Audible downloads paused**. These views read saved state; opening a page
does not request a license.

Deferred books say **Waiting — license requests paused** in Library,
book details, the Dashboard, worker queues, and Pipeline → Waiting. They show
the affected account and automatic retry time. Retry times use the browser's
local timezone, with an explicit UTC fallback before JavaScript runs. Failed
books show their saved display-safe error beside the title and in book details.
Completed books do not show an old failed queue entry as their current issue.

The account warning refreshes every 12 seconds and on a deferred-download event.
Book rows also consume pipeline events and snapshots, so a live update does not
replace a pause with the generic Queued label. The status JSON API reports
`audible_account_cooldowns`, `downloads_deferred`, and `has_issues=true` during an
active license pause. `/healthz` remains a process-liveness check; a provider
throttle should not trigger a container restart loop.

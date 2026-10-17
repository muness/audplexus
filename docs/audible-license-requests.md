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

Each customer and marketplace shares one license transport, including connected
accounts with the same Audible customer ID. If no customer ID is available, the
transport uses the local account ID. It intercepts the SDK's license POSTs,
including requests made inside `DownloadBook`. Library reads and CDN transfers
bypass the license gate.

- License requests run one at a time for that customer and marketplace.
- Concurrent identical requests share one response, with a separate response
  body for each caller.
- Other title denials stay in memory for five minutes. Their cache key includes
  the request URL, request body, and a hash of the ADP token. A new token skips
  an old title denial.
- Granted licenses are not cached after concurrent callers finish sharing the
  response. Download URLs and key material are not persisted by this cache.

`CustomerThrottled` or HTTP 429 starts an account cooldown. The first delay is
15 minutes; repeated throttling doubles it up to 24 hours. A longer `Retry-After`
header takes precedence. These are application defaults, not a statement about
Audible's throttle window. After expiry, requests remain serialized: if the
first probe is throttled, the others stop without contacting Audible. A granted
license clears the backoff.

The gate assumes one Audplexus server process. Multiple processes sharing a
database do not share the in-memory request permit.

The settings table stores a hashed account key, expiry, and delay. Restarting or
signing in again does not clear a saved cooldown. A database write failure keeps
the cooldown in memory and reports the persistence error; it cannot guarantee
that the cooldown survives a restart.

## Deferred downloads

A throttled download moves to `deferred`, remains visible under **Waiting**, and
shows its retry time. It keeps its existing files and its book stays queued.
Workers skip deferred jobs, so another account can continue. They resume deferred
jobs when the owning account's cooldown expires. Cancellation and queue refills
include deferred jobs. Automatic resume requires the AccountManager used by the
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

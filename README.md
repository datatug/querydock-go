# querydock-go

QueryDock.run's publishing API stores complete, immutable fiddle snapshots. It
never executes SQL. The current batch implements publishing Plan Tasks 1–3.

The deployable command **refuses startup** until Task 6's live private Firestore rule deployment and runtime wiring are
verified. Firebase and revision libraries are deliberately not
wired to the production executable yet. There is no memory-store fallback,
self-issued identity, emulator production mode, cloud write or deployment setup.
The mandatory owning Core root policy registers `querydock`; no local policy
copy, override or unregistered root is used.

## HTTP boundary

- `POST /api/querydock/fiddles`: `{snapshot,digest,idempotencyKey}`.
- `POST /api/querydock/fiddles/{id}/revisions`: additionally `expectedRevision`.
- `GET /api/querydock/fiddles/{id}` resolves latest; append
  `/revisions/{positiveRevision}` for a pinned public read.
- `DELETE /api/querydock/fiddles/{id}` requires verified owner authorization
  inside the atomic storage adapter. No list endpoint exists.

Mutation identities come solely from the injected verifier's bearer-token
result. Missing/invalid identities return 401, anonymous identities 403, and
unconfigured ports 503. The whole mutation request is bounded to 512 KiB before
JSON decoding; snapshots also have a 512 KiB canonical bound and seed content a
384 KiB UTF-8 bound. Client digests are compared with recomputed SHA-256 values.
Unknown fields/versions, duplicate decoded keys, invalid UTF-8, lone surrogates,
non-finite numbers and excessive nesting are rejected. SQL and seed content are
preserved as opaque text. Error responses expose stable codes and omit internal errors, tokens and SQL.
Public snapshot DTOs expose no owner identifiers. Public responses are `no-store,
must-revalidate`; the later edge integration must also bypass caching.

Storage ports document their atomic authorization/accounting obligations.
`internal/revisions` implements owner-checked create/append, immutable history,
expected-revision comparison and idempotency in Core-protected DALgo transactions.
Create candidate IDs and timestamps stay fixed across transaction retries; create
fingerprints exclude the candidate ID and timestamp. Idempotency ledger keys hash
length-framed owner UID/key values, so path-unsafe Firebase UIDs never become path
segments. Fingerprints bind owner, operation, target, expected revision and every
canonical snapshot byte. Existing tombstones fence append and old receipt replay.
Create/append return `{id,revision,digest,canonicalUrl}`;
the canonical URL pins `https://querydock.run/sql/{id}/{revision}`. Quota errors
use `error.code=quota_exceeded`, stable `error.limit`/`error.action` guidance,
and temporal retry seconds plus `Retry-After` for the rolling rate bound. Other stable codes are defined alongside handlers.

## Contract provenance and validation

The browser contract is pinned to
[datatug-apps@2220f62](https://github.com/datatug/datatug-apps/tree/2220f62c091488ffa229cbd787550d2c063984fa/libs/querydock/src/lib).
`internal/snapshot/testdata/provenance.json` records source/fixture SHA-256
values; the three published JSON fixtures are copied byte-for-byte. Tests check
complete canonical source bytes, digest, ordered queries/seeds, typed parameters,
rejection vectors and round trips. RFC 8785 uses UTF-16 object-key ordering,
unmodified Unicode and ECMAScript binary64 number cutovers. Additional numeric
fixtures contain RFC 8785 Appendix B bit boundaries plus deterministic binary64
samples checked against Node 24.20.0 `JSON.stringify`.

Run with Go 1.27 or newer:

```sh
GOWORK=off ./scripts/coverage.sh
```

All packages, both executable entry points, composition and error/recovery paths
are instrumented via `go test ./... -coverpkg=./... -coverprofile=coverage.out`.
The gate merges identical blocks emitted by cross-package instrumentation (as
`go tool cover` does), rejects every block with no execution, and the script
requires the tool's exact `100.0%` total. No source/package exclusions or rounded
percentage exemptions exist. A negative gate test proves that one unhit branch
fails even when a percentage display would round to 100.0%.

CI pins the same strongo/cicd reusable workflow and Go 1.27.3 as the inspected
DataTug Cloud `main`, with `min_test_coverage_percent: 100`, `coverpkg: ./...` and
version bumping disabled. This public module uses published Firebase/DALgo/Core dependencies and needs no
private dependency credentials. CI builds the static Linux artifact and the
nonroot distroless container; it does not push an image or deploy it.

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o bin/querydock-go ./cmd/querydock-go
docker build --platform linux/amd64 -t querydock-go:local .
```

The binary's expected current result is a nonzero configuration failure until
the separately gated production runtime is explicitly enabled. A built image does not establish deployed or
account-backed publishing acceptance.

## Verified identity and storage prerequisites

`internal/identity.NewProduction` accepts only the explicit DataTug project and
audience `sneat-eur3-1`. The Firebase Admin SDK verifies signatures, issuer,
audience and timestamps, then checks revoked tokens and disabled/deleted users.
Anonymous providers do not gain write authority. Both construction and each
verification reject emulator environment variables. SDK failures are redacted;
account lookup/infrastructure failures fail closed.

The later runtime needs a **dedicated service account** with
`firebaseauth.users.get` for `VerifyIDTokenAndCheckRevoked` plus only the Firestore
data permissions required by the approved storage operations. Do not reuse a
shared administrator or Stripe service account. IAM grants and deployment are
outside this batch. The private `querydock` Firestore client exclusions must be
verified live before durable production writes; merging the source rules alone
does not satisfy that gate.

Pinned SDKs: Firebase Admin 4.22.0, Firestore 1.26.0, DALgo 0.95.3 and
DALgo Firestore 0.10.40. Core 0.72.0 resolves merged commit
`cc8893075fe41046a363f435ccf094abb9f972d9`; the embedded registry SHA-256 is
`dbf50f47c7e29251cc016e431e9d0d55e4be3e50f7b53fd42ddf2f110aefd781`.
Every store constructor applies `firestorepolicy.ProtectRootCollections` and
preserves any existing policy restrictions.

Unit tests exercise the actual SDK's RSA signature, audience/issuer/expiry,
revocation and disabled-account checks through owned in-process HTTP responses.
They also inject transaction/commit failures and callback retries with rollback
assertions. Separate real Auth/Firestore emulator journeys prove SDK paths,
concurrent expected-revision conflicts, concurrent identical/changed-body
idempotency, candidate-ID collisions, immutable source bytes and owner fences.
These are demo-project receipts, not production IAM/auth-domain/rules acceptance.

CI installs the locked Firebase CLI 15.22.4 and runs the same isolated harness:

```sh
npm ci --prefix .github/tools/emulators --ignore-scripts --no-audit --no-fund
python3 scripts/sdk-emulators.py --firebase-js .github/tools/emulators/node_modules/firebase-tools/lib/bin/firebase.js --logs /tmp/querydock-sdk-receipts
```

Java 21, Node 24 and Go must be on PATH. The harness chooses its own loopback
ports, uses only a `demo-querydock-*` project, records process ownership and
teardown, and verifies its listeners are released. It never attaches to another
emulator or deploys rules. Direct client access is denied in its temporary rules;
Admin SDK transaction tests do not establish the separately gated live rules.

## Quotas, public reads and resumable deletion

Every new revision transaction updates the same hashed-owner usage record as its
snapshot/history/idempotency writes. The fixed launch bounds are 10 new revisions
in a rolling 60-second window, 100 active or cleanup-reserved fiddles, 100 revisions
per fiddle and 20 MiB of retained canonical snapshot UTF-8 bytes. A live identical
retry returns its original receipt and consumes no new quota. Changed-body key
reuse returns 409. Request timestamps come from the injected server clock; late
requests and clock rollback never move accepted quota time backwards.

| `error.limit` | `error.action` | Retry guidance |
| --- | --- | --- |
| `revision_rate` | `wait` | Wait for the stated `Retry-After` seconds. |
| `active_fiddles` | `delete_fiddle` | Finish deleting an owned fiddle, including pending cleanup. |
| `fiddle_revisions` | `fork_fiddle` | Save subsequent work in a new fiddle. |
| `retained_snapshot_bytes` | `delete_fiddle` | Finish payload cleanup to release retained storage. |

Known-ID latest/pinned public reads use a consistent transaction and return only
validated ID/revision/digest/snapshot DTOs. There is no list-all endpoint. GET/HEAD
responses permit credentialless cross-origin readers with `Access-Control-Allow-Origin: *`
and no credential grant; mutation and preflight responses receive no CORS grant.
The API uses `no-store, must-revalidate`; deployment must preserve the edge cache
bypass and these headers.

Deletion first commits an owner-checked tombstone freezing the final revision and
charged bytes. Public reads, appends and identical prior-save retries then return
404; changed-body key reuse remains 409. One bounded transaction removes at most
one payload and advances a durable cursor. A 512 KiB snapshot plus its small
scalar/index metadata remains below Firestore's 10 MiB transaction-size ceiling;
the implementation never tries to delete a full 20 MiB history in one transaction.
Missing already-cleaned records are safe. Both byte and fiddle reservations remain
charged through every batch, even when all payloads are gone before finalization.
A final atomic transaction marks cleanup complete and releases both reservations
exactly once. It never refunds revision-rate history.

DELETE returns 204 only after confirmed finalization. A failed/interrupted attempt
returns 503 `cleanup_pending` with `action=retry_delete`, preserving authenticated
owner access to resume progress although public reads return 404. Cleanup has a
bounded request deadline/step count and may need an owner retry. **No automatic
physical erasure is promised when no retry occurs.** Stale save receipts cannot
recreate payloads, and tombstone/idempotency metadata remain to fence resurrection.

Unit tests cover each exact bound, rolling-window boundary/clock rollback, failed
and uncertain commits at every deletion stage, competing deleters and no-progress
failure. Real SDK emulator tests additionally race each quota and append/delete,
then create the full 20 MiB as forty 512 KiB snapshots, interrupt cleanup, drop a
confirmed finalization acknowledgement and verify all payloads are physically
absent with exactly-once reservations and unchanged rate history. These tests do
not enable production or establish live Firebase/domain/IAM acceptance.

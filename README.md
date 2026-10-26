# querydock-go

QueryDock.run's publishing API stores complete, immutable fiddle snapshots. It
never executes SQL. This batch implements publishing Plan Task 1 only.

The deployable command **refuses startup** while production Firebase identity
and atomic DALgo storage adapters are absent. It has no memory-store fallback,
self-issued identities, anonymous-write mode, cloud writes or deployment setup.
Tasks 2–3 must wire verified non-anonymous Firebase identities, ownership,
idempotency, revision comparison, root-policy-protected durable storage, quotas
and tombstones before production can serve. Register the `querydock` extension
in the owning approved root policy before durable writes; do not bypass it.

## HTTP boundary

- `POST /api/querydock/fiddles`: `{snapshot,digest,idempotencyKey}`.
- `POST /api/querydock/fiddles/{id}/revisions`: additionally `expectedRevision`.
- `GET /api/querydock/fiddles/{id}` resolves latest; append
  `/revisions/{positiveRevision}` for a pinned public read.
- `DELETE /api/querydock/fiddles/{id}` requires verified owner authorization
  inside the later atomic adapter. No list endpoint exists.

Mutation identities come solely from the injected verifier's bearer-token
result. Missing/invalid identities return 401, anonymous identities 403, and
unconfigured ports 503. The whole mutation request is bounded to 512 KiB before
JSON decoding; snapshots also have a 512 KiB canonical bound and seed content a
384 KiB UTF-8 bound. Client digests are compared with recomputed SHA-256 values.
Unknown fields/versions, duplicate decoded keys, invalid UTF-8, lone surrogates,
non-finite numbers and excessive nesting are rejected. SQL and seed content are
preserved as opaque text. Responses expose stable error codes, never internal
errors, tokens, SQL or owner identifiers. Public responses are `no-store,
must-revalidate`; the later edge integration must also bypass caching.

Storage ports document the atomic ownership/idempotency/quota obligations.
Injected fake ports in tests prove the HTTP boundary, not production transaction
or Firebase behavior. Create/append return `{id,revision,digest,canonicalUrl}`;
the canonical URL pins `https://querydock.run/sql/{id}/{revision}`. Quota errors
use `error.code=quota_exceeded`, a stable `error.limit` and retry seconds plus
`Retry-After`. Other stable codes are defined alongside handlers.

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
version bumping disabled. This public standard-library-only module needs no
private dependency credentials. CI builds the static Linux artifact and the
nonroot distroless container; it does not push an image or deploy it.

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o bin/querydock-go ./cmd/querydock-go
docker build --platform linux/amd64 -t querydock-go:local .
```

The binary's expected current result is a nonzero configuration failure until
real adapters are implemented. A built image does not establish deployed or
account-backed publishing acceptance.

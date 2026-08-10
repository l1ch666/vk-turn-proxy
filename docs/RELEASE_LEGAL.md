# Release legal gate

## Current status

Binary-distribution obligations attach to what an artifact actually links, not
to everything listed in `go.mod`. The two artifacts this repository produces
have different dependency graphs and are therefore audited separately.

| Artifact | Ships | Audit scope | Status |
| --- | --- | --- | --- |
| Container image | `vk-turn-server` | `./server` | **passing** |
| Release archives | client, server, bench | `./...` | **blocked** |

The container image ships exactly one binary. Its full linked graph is thirteen
modules under MIT, BSD-2-Clause, BSD-3-Clause, and Apache-2.0 — all inside the
reviewed allowlist and all compatible with this repository's GPL-3.0 terms. It
links neither `fhttp` nor `tls-client`; those are reachable only from the VK
authentication code in `./client`.

Release archives additionally ship the client and remain blocked by the items
below.

This file records an engineering release gate, not legal advice.

## Known blockers (archives only)

- `github.com/bogdanfinn/fhttp v0.6.8` does not contain a discoverable license
  file. Its source refers to a BSD-style license file that is absent from the
  module, so the redistribution terms for the fork and its modifications must
  be verified or the dependency replaced.
- `github.com/bogdanfinn/tls-client v1.15.1` is classified as
  `BSD-4-Clause-UC` and contains an advertising clause. Compatibility with this
  repository's GPL-3.0 terms must be resolved by replacement, relicensing, or
  qualified legal review.
- Legacy Android installer versions consume standalone `server-linux-*`
  compatibility assets. Keep those assets covered by `SHA256SUMS` and
  applicable license/source notices until the archive-based installer
  migration has reached supported clients and the raw assets can be retired.

Both dependencies exist to reproduce a browser TLS fingerprint during VK
captcha and authentication, so neither can simply be dropped without replacing
that capability.

## How the workflow fails closed

`release.yml` runs the two audits as independent jobs, so a client-only
blocker cannot block the image and a passing image cannot unblock the archives:

- `license-image` audits `./server`, then diffs the result against the
  committed `THIRD_PARTY_LICENSES/server` bundle. It fails on an unapproved
  license **or** on a stale committed bundle. `docker` depends on this job.
- `license-archives` audits `./...` and fails on any unapproved or unknown
  license. `build` and `publish` depend on this job.

The `publish` and `docker` jobs additionally require the repository variable
`RELEASE_LEGAL_APPROVED` to be exactly `true`. That variable is a human
approval switch; it does not bypass either automated audit. With it set today,
the image would publish and the archives would still fail their own audit.

## Reproduce an audit

Run from the repository root with the Go version declared in `go.mod`:

```sh
rm -rf /tmp/vk-turn-license-server
bash scripts/audit-third-party-licenses.sh /tmp/vk-turn-license-server ./server
```

```sh
rm -rf /tmp/vk-turn-license-all
bash scripts/audit-third-party-licenses.sh /tmp/vk-turn-license-all ./...
```

The script pins `github.com/google/go-licenses` to `v1.6.0`, records the exact
`go.mod`, `go.sum`, Go version, audited scope, and the modules actually linked
by that scope, writes a sorted `REPORT.csv`, rejects every license outside the
reviewed allowlist, and saves the corresponding license texts. It is a
reproducible audit input, not a substitute for human review. The `./server`
scope passes; the `./...` and `./client` scopes intentionally fail.

## Approval checklist for archives

- Replace or obtain verified terms for `fhttp`.
- Replace, relicense, or explicitly approve the four-clause-BSD dependency.
- Run the `./...` audit successfully from a clean checkout.
- Review the generated report and license texts against the built binaries.
- Verify that every standalone Android compatibility binary is covered by the
  release checksum and by license/source-notice delivery.
- Confirm that tagged source remains available for GPL and MPL source
  obligations.
- Set the protected repository variable `RELEASE_LEGAL_APPROVED=true` only
  after review, preferably together with a protected release environment.

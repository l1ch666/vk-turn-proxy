# Release legal gate

## Current status: publishing blocked

The repository can be built and tested, but official GitHub Releases and GHCR
images must not be published yet. The release workflow fails closed in two
independent ways:

1. `scripts/audit-third-party-licenses.sh` rejects unknown or unapproved
   dependency licenses and generates the license bundle only after that audit
   passes.
2. The `publish` and `docker` jobs run only when the repository variable
   `RELEASE_LEGAL_APPROVED` is exactly `true`.

Keep that variable unset or set to any value other than `true` until every item
below has been resolved and reviewed. Setting the variable does not bypass the
automated audit.

## Known blockers

- `github.com/bogdanfinn/fhttp v0.6.8` does not contain a discoverable license
  file. Its source refers to a BSD-style license file that is absent from the
  module, so the redistribution terms for the fork and its modifications must
  be verified or the dependency replaced.
- `github.com/bogdanfinn/tls-client v1.15.1` is classified as
  `BSD-4-Clause-UC` and contains an advertising clause. Compatibility with this
  repository's GPL-3.0 terms must be resolved by replacement, relicensing, or
  qualified legal review.
- A complete binary-distribution notice bundle has not previously been shipped.
  The release workflow now generates a reproducible baseline from the exact
  module graph after the blockers above are removed, but a human must still
  review upstream `NOTICE` files and license-specific source obligations.
- Legacy Android installer versions consume standalone `server-linux-*`
  compatibility assets. Keep those assets covered by `SHA256SUMS` and
  applicable license/source notices until the archive-based installer
  migration has reached supported clients and the raw assets can be retired.

This file records an engineering release gate, not legal advice.

## Reproduce the audit

Run from the repository root with the Go version declared in `go.mod`:

```sh
rm -rf /tmp/vk-turn-license-audit
bash scripts/audit-third-party-licenses.sh /tmp/vk-turn-license-audit
```

The script pins `github.com/google/go-licenses` to `v1.6.0`, records the exact
`go.mod`, `go.sum`, Go version, and sorted module graph, writes a sorted
`REPORT.csv`, rejects every license outside the reviewed allowlist, and saves
the corresponding license texts. It is a reproducible audit input, not a
substitute for human review. Unknown licenses and the current four-clause-BSD
dependency intentionally make the command fail.

When the audit passes, release archives and the container image include the
generated report and license tree under `THIRD_PARTY_LICENSES`.

## Approval checklist

- Replace or obtain verified terms for `fhttp`.
- Replace, relicense, or explicitly approve the four-clause-BSD dependency.
- Run the audit successfully from a clean checkout.
- Review the generated report and license texts against the built binaries.
- Verify that every standalone Android compatibility binary is covered by the
  release checksum and by license/source-notice delivery.
- Confirm that tagged source remains available for GPL and MPL source
  obligations.
- Set the protected repository variable `RELEASE_LEGAL_APPROVED=true` only
  after review, preferably together with a protected release environment.

# Generated third-party license bundles

Binary-distribution obligations attach to what an artifact actually links, not
to everything listed in `go.mod`. The commands in this repository have different
dependency graphs, so they are audited and bundled separately by
`scripts/audit-third-party-licenses.sh OUTPUT_DIR PACKAGE_SCOPE`.

## `server/` — audited, complete

The container image ships exactly one binary, `./server`. Its full linked
dependency graph is thirteen modules under MIT, BSD-2-Clause, BSD-3-Clause, and
Apache-2.0 — every one inside the reviewed allowlist and compatible with this
repository's GPL-3.0 terms. `server/` holds the audit inputs (`go.mod`, `go.sum`,
`GO_VERSION.txt`, `MODULES.txt`, `SCOPE.txt`, `TOOL.txt`), the sorted
`REPORT.csv`, and the corresponding license texts under `licenses/`.

Regenerate and compare after any dependency change:

```sh
rm -rf /tmp/vk-turn-license-server
bash scripts/audit-third-party-licenses.sh /tmp/vk-turn-license-server ./server
diff -ru THIRD_PARTY_LICENSES/server /tmp/vk-turn-license-server
```

The bundle deliberately excludes this module's own source. The GPL-3.0 source
obligation is met by the public repository and the tagged source, not by
embedding a second copy of the tree inside its own license directory.

## `client/` — not present, still blocked

Release archives also ship `./client`, whose graph pulls
`github.com/bogdanfinn/fhttp` (no discoverable license file) and
`github.com/bogdanfinn/tls-client` (`BSD-4-Clause-UC`, advertising clause). The
audit fails closed for that scope, so no client bundle is generated and archive
publishing stays blocked. See `docs/RELEASE_LEGAL.md`.

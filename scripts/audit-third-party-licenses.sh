#!/bin/sh
set -eu

tool_version="v1.6.0"
module_path="github.com/l1ch666/vk-turn-proxy/v2"
output_dir="${1:-}"
scope="${2:-./...}"

if [ -z "$output_dir" ]; then
    echo "usage: $0 OUTPUT_DIR [PACKAGE_SCOPE]" >&2
    echo "" >&2
    echo "PACKAGE_SCOPE defaults to ./... (every command in the repository)." >&2
    echo "Binary-distribution obligations attach to what an artifact actually" >&2
    echo "links, so audit the scope you are shipping:" >&2
    echo "  $0 out/server ./server   # the container image ships only this" >&2
    echo "  $0 out/client ./client   # release archives also ship this" >&2
    exit 2
fi
if [ -e "$output_dir" ]; then
    echo "license audit output already exists: $output_dir" >&2
    exit 2
fi

mkdir -p "$output_dir"
raw_report="$(mktemp)"
trap 'rm -f "$raw_report"' EXIT HUP INT TERM

cp go.mod go.sum "$output_dir/"
# GOVERSION, not `go version`: the latter appends the host GOOS/GOARCH, which
# would make a bundle generated on one platform differ from the same bundle
# regenerated in CI on another, breaking the committed-bundle drift check.
go env GOVERSION >"$output_dir/GO_VERSION.txt"
printf '%s\n' "$scope" >"$output_dir/SCOPE.txt"

# Record the modules that actually provide packages linked by this scope, not
# the whole module graph. A module required by go.mod but never linked into the
# audited binaries carries no binary-distribution obligation for that artifact.
go list -mod=readonly -deps \
    -f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}}{{with .Replace}} => {{.Path}} {{.Version}}{{end}}{{end}}{{end}}' \
    "$scope" |
    sed '/^$/d' |
    LC_ALL=C sort -u >"$output_dir/MODULES.txt"

go run "github.com/google/go-licenses@${tool_version}" report "$scope" >"$raw_report"

{
    echo "package,license_url,license"
    LC_ALL=C sort -u "$raw_report"
} >"$output_dir/REPORT.csv"
printf '%s\n' "github.com/google/go-licenses ${tool_version}" >"$output_dir/TOOL.txt"

audit_failed=0
while IFS=, read -r package license_url license; do
    [ -n "$package" ] || continue
    [ "$package" != "$module_path" ] || continue

    case "$license" in
        Apache-2.0|BSD-2-Clause|BSD-3-Clause|MIT|MPL-2.0)
            ;;
        *)
            echo "unapproved or unknown license: $package ($license, $license_url)" >&2
            audit_failed=1
            ;;
    esac
done <"$raw_report"

if [ "$audit_failed" -ne 0 ]; then
    echo "third-party license audit failed for scope $scope; see docs/RELEASE_LEGAL.md" >&2
    exit 1
fi

# Ignore this module when saving. go-licenses treats GPL-3.0 as source-required
# and would otherwise copy the whole repository into the bundle. This project's
# own source obligation is met by the public repository and the tagged source,
# not by embedding a second copy of the tree inside its own license directory.
go run "github.com/google/go-licenses@${tool_version}" save "$scope" \
    --save_path="$output_dir/licenses" \
    --ignore "$module_path"

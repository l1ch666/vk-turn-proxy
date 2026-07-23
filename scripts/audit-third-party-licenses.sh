#!/bin/sh
set -eu

tool_version="v1.6.0"
module_path="github.com/l1ch666/vk-turn-proxy/v2"
output_dir="${1:-}"

if [ -z "$output_dir" ]; then
    echo "usage: $0 OUTPUT_DIR" >&2
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
go version >"$output_dir/GO_VERSION.txt"
go list -mod=readonly -m \
    -f '{{if not .Main}}{{.Path}} {{.Version}}{{with .Replace}} => {{.Path}} {{.Version}}{{end}}{{end}}' \
    all |
    sed '/^$/d' |
    LC_ALL=C sort -u >"$output_dir/MODULES.txt"

go run "github.com/google/go-licenses@${tool_version}" report ./... >"$raw_report"

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
    echo "third-party license audit failed; see docs/RELEASE_LEGAL.md" >&2
    exit 1
fi

go run "github.com/google/go-licenses@${tool_version}" save ./... \
    --save_path="$output_dir/licenses"

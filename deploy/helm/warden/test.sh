#!/bin/sh
# Chart checks: helm lint, then helm template against each testdata/values-
# <case>.yaml compared with testdata/<case>.golden.yaml. `test.sh --update`
# rewrites the goldens; review the diff before committing it. Needs helm on
# PATH; no cluster.
set -eu
chart=$(cd "$(dirname "$0")" && pwd)
update=0
[ "${1:-}" = "--update" ] && update=1

helm lint "$chart" -f "$chart/testdata/values-gvisor-loopback.yaml" --quiet

tmp=$(mktemp "${TMPDIR:-/tmp}/warden-chart.XXXXXX")
trap 'rm -f "$tmp"' EXIT
status=0
for values in "$chart"/testdata/values-*.yaml; do
	case=$(basename "$values" .yaml)
	case=${case#values-}
	golden="$chart/testdata/$case.golden.yaml"
	helm template warden "$chart" --namespace warden -f "$values" >"$tmp"
	if [ "$update" = 1 ]; then
		cp "$tmp" "$golden"
		echo "wrote $golden"
		continue
	fi
	if diff -u "$golden" "$tmp" >/dev/null; then
		echo "ok   $case"
	else
		echo "FAIL $case (run $0 --update to accept):"
		diff -u "$golden" "$tmp" || true
		status=1
	fi
done
exit $status

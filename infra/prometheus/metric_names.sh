#!/usr/bin/env bash
# Every codetrail_ metric named in alerts.yml must exist in metrics.go.
#
# This check carries the claim by itself: `promtool check rules` parses
# expressions and never contacts a server, so it cannot tell a real metric from
# a typo. An alert over a misspelled counter is silent for ever and looks clean
# in every other check there is.
set -uo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$root" || exit 1

rules=infra/prometheus/alerts.yml
source=packages/shared/metrics/metrics.go

fails=0
while read -r name; do
	base=$name
	# Three written exceptions, and they are exceptions rather than a wildcard:
	# _bucket and _count are suffixes the client library derives from a
	# histogram and are in no Go file under those names. `up` is Prometheus's
	# own synthetic metric and does not start with codetrail_, so it never
	# reaches this loop at all.
	case $name in
	*_bucket) base=${name%_bucket} ;;
	*_count) base=${name%_count} ;;
	esac
	# Whitespace-tolerant, because gofmt aligns `Name:` against a longer
	# neighbouring field in every struct that also sets Buckets — measured, a
	# single-space pattern missed both histograms and reported them as typos.
	if ! grep -qE "Name:[[:space:]]+\"$base\"" "$source"; then
		printf '  FAIL %s is named in %s and is in no metrics.go instrument\n' "$name" "$rules"
		fails=$((fails + 1))
	fi
done < <(grep -oE 'codetrail_[a-z_]+' "$rules" | sort -u)

if [ "$fails" -ne 0 ]; then
	echo "$fails metric name(s) in the rules do not exist"
	exit 1
fi
echo "every codetrail_ metric in $rules exists in $source"

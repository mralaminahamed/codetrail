#!/usr/bin/env bash
# Migrations are expand-only from P8 forward, and this is what enforces it.
#
# Why it is a check and not a convention. store.New calls migrate() before it
# returns, so migrations run inside the NEW task, at boot, before ECS marks it
# healthy — and during a rolling deploy the OLD image is still serving against
# the new schema. A rollback does not un-apply anything: there are no down
# migrations, schema_migrations keeps the row, and the circuit breaker rolls
# back the task definition while the schema stays forward.
#
# So a migration may add tables, columns, indexes and constraints the previous
# image tolerates, and a destructive change ships ONE RELEASE AFTER the code
# that stopped using the thing being dropped. A comment in the migrations
# directory saying so would be the highest-risk line in the file.
#
# An exemption is allowed and has to justify itself, on its own line:
#
#   -- expand-only-exempt: <reason>
set -uo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root" || exit 1

dir=packages/shared/store/migrations
marker='-- expand-only-exempt:'

# Each pattern is a change the previous image cannot survive: it either removes
# something that image still reads, or rejects a write it still makes.
destructive='DROP[[:space:]]+COLUMN|DROP[[:space:]]+TABLE|ALTER[[:space:]]+COLUMN[[:space:]]+.*[[:space:]]TYPE[[:space:]]|SET[[:space:]]+NOT[[:space:]]+NULL|RENAME'

fails=0
checked=0
for f in "$dir"/*.sql; do
	checked=$((checked + 1))
	hit=$(grep -niE "$destructive" "$f" | grep -v '^[0-9]*:[[:space:]]*--' || true)
	[ -z "$hit" ] && continue

	reason=$(grep -F -e "$marker" "$f" | head -1 | sed "s|.*$marker||" | sed 's/^[[:space:]]*//')
	if [ -z "$reason" ]; then
		printf '  FAIL %s is destructive and carries no exemption:\n' "$f"
		printf '       %s\n' "$hit"
		printf '       Add: %s <why the previous image cannot miss this>\n' "$marker"
		fails=$((fails + 1))
		continue
	fi
	printf '  ok   %s is exempt: %s\n' "$f" "$reason"
done

if [ "$checked" -eq 0 ]; then
	echo "no migrations found under $dir; this check would be vacuous"
	exit 1
fi

if [ "$fails" -ne 0 ]; then
	echo "$fails migration(s) are destructive without a written reason"
	exit 1
fi
echo "$checked migrations checked, all expand-only or exempt with a reason"

#!/usr/bin/env bash
# End-to-end assertions against a running codetrail.
#
#   ./infra/smoke.sh --target compose  [base-url]
#   ./infra/smoke.sh --target deployed <base-url>
#
# One script with a target-dependent subset, and the subset is printed at the
# top of every run rather than silently skipped.
#
# Why two subsets. The ALB routes /api/* to the gateway and makes the console
# the default action, but the gateway serves /health, /ready and /metrics at the
# ROOT. Against a deployed URL those four paths reach the console's nginx, and
# with SPA fallback step 1 would pass FALSELY while steps 2-4 failed on content
# — the worst possible combination. The four assertions that actually prove a
# deployment works are identical in both modes: the allowlist was not widened, a
# stranger's repository indexed end to end, call edges resolved, and citations
# point at the indexed commit.
set -uo pipefail

TARGET=""
BASE=""
INDEXER_METRICS=${INDEXER_METRICS:-}
# rs/zerolog is what README measures the resolution baseline against.
REPO=${SMOKE_REPO:-https://github.com/rs/zerolog}
REPO_REF=${SMOKE_REF:-master}
INDEX_TIMEOUT=${SMOKE_INDEX_TIMEOUT:-600}

while [ $# -gt 0 ]; do
	case $1 in
	--target)
		TARGET=$2
		shift 2
		;;
	-*)
		echo "unknown flag $1" >&2
		exit 2
		;;
	*)
		BASE=$1
		shift
		;;
	esac
done

case $TARGET in
compose) BASE=${BASE:-http://localhost:8080} ;;
deployed)
	if [ -z "$BASE" ]; then
		echo "--target deployed needs a base URL" >&2
		exit 2
	fi
	;;
*)
	echo "usage: $0 --target compose|deployed [base-url]" >&2
	exit 2
	;;
esac

fails=0
pass() { printf '  ok   %s\n' "$1"; }
fail() { printf '  FAIL %s\n' "$1"; fails=$((fails + 1)); }
note() { printf '  --   %s\n' "$1"; }

echo "codetrail smoke test, target=$TARGET base=$BASE"
echo
echo "asserted in this mode:"
if [ "$TARGET" = compose ]; then
	echo "  1-2 liveness and readiness (curl)   3 floor-gauge invariant (/metrics)"
	echo "  4   pgvector version (/metrics)     5-8 admission, index, edges, citations"
	echo "  9   indexer job counter (probe port)"
else
	echo "  1-2 NOT asserted here: /health and /ready are not published through the"
	echo "      load balancer. A healthy target group IS the readiness assertion, made"
	echo "      continuously by AWS: aws elbv2 describe-target-health, State: healthy."
	echo "  3-4 NOT asserted here: read the gateway's boot log line instead —"
	echo "      aws logs filter-log-events, which carries the floor and the datastore versions."
	echo "  5-8 admission, index, edges_resolved, citations — identical to compose"
	echo "  9   only when observability_enabled; otherwise printed as not asserted"
fi
echo

api() { printf '%s/api%s' "$BASE" "$1"; }

# --------------------------------------------------------------- 1 and 2 ---

if [ "$TARGET" = compose ]; then
	code=$(curl -fsS -o /dev/null -w '%{http_code}' "$BASE/health" 2>/dev/null)
	if [ "$code" = 200 ]; then pass "1 /health is 200"; else fail "1 /health is $code"; fi

	code=$(curl -fsS -o /dev/null -w '%{http_code}' "$BASE/ready" 2>/dev/null)
	metrics=$(curl -fsS "$BASE/metrics" 2>/dev/null)
	# The endpoint AND the gauge: spec §11 makes the gauge the signal, and an
	# endpoint answering 200 with the gauge at 0 is a wiring bug nothing else
	# sees.
	if [ "$code" = 200 ] && printf '%s' "$metrics" | grep -q '^codetrail_ready 1$'; then
		pass "2 /ready is 200 and codetrail_ready is 1"
	else
		fail "2 /ready is $code and codetrail_ready is $(printf '%s' "$metrics" | grep '^codetrail_ready ' || echo absent)"
	fi

	# ------------------------------------------------------------------- 3 ---
	# An invariant over the PAIR, not a snapshot of either. Asserting
	# calibrated 0 was true when this was written and stops being true the
	# moment P6 lands, and a smoke test an unrelated phase has to edit is one
	# that gets edited wrongly. What this catches survives both sides: a
	# deployment claiming a measured floor while serving the placeholder, or
	# serving a measured value while reporting it as unmeasured.
	floor=$(printf '%s' "$metrics" | awk '/^codetrail_score_floor /{print $2}')
	calibrated=$(printf '%s' "$metrics" | awk '/^codetrail_score_floor_calibrated /{print $2}')
	case "$calibrated" in
	1)
		if awk -v f="$floor" 'BEGIN{exit !(f > 0 && f <= 1)}'; then
			pass "3 the floor is calibrated and is a real cosine ($floor)"
		else
			fail "3 calibrated=1 but the floor is $floor, which is not a cosine in (0,1]"
		fi
		;;
	0)
		if awk -v f="$floor" 'BEGIN{exit !(f == -1)}'; then
			pass "3 the floor is uncalibrated and is the -1 placeholder"
		else
			fail "3 calibrated=0 but the floor is $floor, not the -1 placeholder"
		fi
		;;
	*) fail "3 codetrail_score_floor_calibrated is ${calibrated:-absent}" ;;
	esac

	# ------------------------------------------------------------------- 4 ---
	# Printed as well as bounded: the assertion's real job is to catch an RDS
	# minor upgrade moving pgvector under a running deployment, and RDS tops out
	# at 0.8.2 while compose runs 0.8.6 — so "the same as dev" is never the
	# expected answer and only the recorded number says what served a query.
	info=$(printf '%s' "$metrics" | grep '^codetrail_datastore_info{' | head -1)
	pgvector=$(printf '%s' "$info" | sed -n 's/.*pgvector="\([^"]*\)".*/\1/p')
	postgres=$(printf '%s' "$info" | sed -n 's/.*postgres="\([^"]*\)".*/\1/p')
	if [ -n "$pgvector" ] && printf '%s\n0.5.0\n' "$pgvector" | sort -V -C; then
		fail "4 pgvector is $pgvector, below the 0.5.0 HNSW floor"
	elif [ -n "$pgvector" ]; then
		pass "4 datastore is postgres $postgres, pgvector $pgvector (>= 0.5.0)"
	else
		fail "4 codetrail_datastore_info is absent"
	fi
else
	note "1-2 target health is the assertion here: aws elbv2 describe-target-health"
	note "3-4 read the gateway boot log: aws logs filter-log-events --filter-pattern 'retrieval configured'"
fi

# ------------------------------------------------------------------- 5 ---
# A negative assertion, and the only one that can catch an ALLOWED_HOSTS set to
# something permissive in a task definition nobody reads.
body=$(curl -sS -X POST -H 'content-type: application/json' \
	-d '{"remote":"https://gitlab.com/x/y"}' "$(api /repos)" 2>/dev/null)
if printf '%s' "$body" | grep -q '"rule":"host"'; then
	pass "5 an unlisted forge is refused naming rule=host"
else
	fail "5 an unlisted forge was not refused with rule=host: $body"
fi

# ------------------------------------------------------------------- 6 ---
# The whole untrusted path in one assertion: the indexer leased a job, cloned
# through the security group's 443 egress, resolved DNS, walked, chunked,
# embedded, type-checked under the environment allowlist, and wrote. Nothing
# else exercises the egress rules at all.
submit=$(curl -sS -X POST -H 'content-type: application/json' \
	-d "{\"remote\":\"$REPO\",\"ref\":\"$REPO_REF\"}" "$(api /repos)" 2>/dev/null)
job=$(printf '%s' "$submit" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
if [ -z "$job" ]; then
	fail "6 submitting $REPO returned no job id: $submit"
	echo
	echo "$fails assertion(s) failed"
	exit 1
fi

status=""
error=""
deadline=$(($(date +%s) + INDEX_TIMEOUT))
while [ "$(date +%s)" -lt "$deadline" ]; do
	poll=$(curl -sS "$(api "/jobs/$job")" 2>/dev/null)
	status=$(printf '%s' "$poll" | sed -n 's/.*"status":"\([^"]*\)".*/\1/p')
	error=$(printf '%s' "$poll" | sed -n 's/.*"error":"\([^"]*\)".*/\1/p')
	[ "$status" = "done" ] && break
	[ "$status" = "failed" ] && break
	sleep 5
done

if [ "$status" = "done" ]; then
	pass "6 $REPO indexed end to end"
else
	# Named as the clone rather than as the assertion beneath it, so a GitHub
	# outage does not read as a broken deployment. The clone is a real network
	# dependency and a real flake source.
	fail "6 indexing $REPO ended as ${status:-timeout}: ${error:-no reason recorded}."
	fail "  If this says the clone failed, the network reached github.com and not the deployment."
	echo
	echo "$fails assertion(s) failed"
	exit 1
fi

repo=$(curl -sS "$(api /repos)" 2>/dev/null | sed -n 's/.*"id":"\([^"]*\)".*/\1/p' | head -1)
detail=$(curl -sS "$(api "/repos/$repo")" 2>/dev/null)

# ------------------------------------------------------------------- 7 ---
# The most valuable assertion here. If the image shipped no Go toolchain, or
# shipped two, or PATH resolved a different one, the repository still indexes,
# every response is still 200, and every edge is silently syntactic — the exact
# downgrade README:540 warns P8 about and the one thing no status code reveals.
resolved=$(printf '%s' "$detail" | sed -n 's/.*"edges_resolved":\([0-9]*\).*/\1/p')
syntactic=$(printf '%s' "$detail" | sed -n 's/.*"edges_syntactic":\([0-9]*\).*/\1/p')
if [ -n "$resolved" ] && [ "$resolved" -gt 0 ]; then
	pass "7 edges_resolved is $resolved (syntactic $syntactic): the image shipped a toolchain"
else
	fail "7 edges_resolved is ${resolved:-absent}: every edge is syntactic, so no usable go is on PATH in the indexer"
fi

# ------------------------------------------------------------------- 8 ---
# A citation pointing at a different commit is the one failure mode that would
# make the product's first sentence false.
commit=$(printf '%s' "$detail" | sed -n 's/.*"commit":"\([^"]*\)".*/\1/p')
answer=$(curl -sS -X POST -H 'content-type: application/json' \
	-d '{"q":"how is a logger created"}' "$(api "/repos/$repo/ask")" 2>/dev/null)
cited=$(printf '%s' "$answer" | grep -o '"commit":"[0-9a-f]*"' | head -1 | sed 's/.*"\([0-9a-f]*\)"$/\1/')
if [ -n "$cited" ] && [ "$cited" = "$commit" ]; then
	pass "8 the answer cites the indexed commit ${commit:0:12}"
elif [ -z "$cited" ]; then
	fail "8 the answer carried no citation: $(printf '%s' "$answer" | head -c 300)"
else
	fail "8 a citation names commit $cited, the repository is at $commit"
fi

# ------------------------------------------------------------------- 9 ---
# Proves the metrics the alerts depend on actually moved, on a real job, in the
# deployed shape.
if [ "$TARGET" = compose ]; then
	INDEXER_METRICS=${INDEXER_METRICS:-http://localhost:9090/metrics}
fi
if [ -n "$INDEXER_METRICS" ]; then
	done_count=$(curl -sS "$INDEXER_METRICS" 2>/dev/null |
		awk -F' ' '/^codetrail_job_total\{outcome="done"\}/{print $2}')
	if [ -n "$done_count" ] && awk -v n="$done_count" 'BEGIN{exit !(n > 0)}'; then
		pass "9 codetrail_job_total{outcome=\"done\"} is $done_count"
	else
		fail "9 codetrail_job_total{outcome=\"done\"} is ${done_count:-absent} after a completed job"
	fi
else
	note "9 NOT asserted: no indexer metrics endpoint reachable (observability_enabled is false)"
fi

echo
if [ "$fails" -ne 0 ]; then
	echo "$fails assertion(s) failed"
	exit 1
fi
echo "all smoke assertions passed"

#!/usr/bin/env bash
# Asserts the properties the sandbox depends on against the two built images.
#
# A shell script and not Go, because every assertion here is about a built
# artifact: running them from Go would mean shelling out anyway.
#
# Run from the repository root: ./infra/image_test.sh
set -uo pipefail

GATEWAY_IMAGE=${GATEWAY_IMAGE:-codetrail/gateway:test}
INDEXER_IMAGE=${INDEXER_IMAGE:-codetrail/indexer:test}
SKIP_BUILD=${SKIP_BUILD:-0}
# Cloned to prove the CA bundle is present. Public, tiny, and stable.
CLONE_URL=${CLONE_URL:-https://github.com/octocat/Hello-World}

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root" || exit 1

fails=0
pass() { printf '  ok   %s\n' "$1"; }
fail() { printf '  FAIL %s\n' "$1"; fails=$((fails + 1)); }
# eq compares an observed value with a written expected one. Written, because
# "empty or default" would pass identically for this image and for a
# FROM golang:1.27 one, and would therefore discriminate nothing.
eq() {
	local what=$1 got=$2 want=$3
	if [ "$got" = "$want" ]; then pass "$what"; else fail "$what: got [$got], want [$want]"; fi
}
contains() {
	local what=$1 hay=$2 needle=$3
	case "$hay" in *"$needle"*) pass "$what" ;; *) fail "$what: [$needle] not in [$hay]" ;; esac
}
absent() {
	local what=$1 hay=$2 needle=$3
	case "$hay" in *"$needle"*) fail "$what: [$needle] is present" ;; *) pass "$what" ;; esac
}
# no_entry matches a whole line, so ".git" is not satisfied by ".gitignore".
no_entry() {
	local what=$1 hay=$2 name=$3
	if printf '%s\n' "$hay" | grep -qx -- "$name"; then
		fail "$what: [$name] is in the context"
	else
		pass "$what"
	fi
}

suffix=$(date +%s)-$$
net=ct-imgtest-net-$suffix
pg=ct-imgtest-pg-$suffix
gw=ct-imgtest-gw-$suffix
ix=ct-imgtest-ix-$suffix

cleanup() {
	docker rm -f "$gw" "$ix" "$pg" >/dev/null 2>&1
	docker network rm "$net" >/dev/null 2>&1
	true
}
trap cleanup EXIT

# ---------------------------------------------------------------- build ---

if [ "$SKIP_BUILD" != "1" ]; then
	echo "building both images"
	docker build -q -f infra/gateway.Dockerfile -t "$GATEWAY_IMAGE" . >/dev/null || exit 1
	docker build -q -f infra/indexer.Dockerfile -t "$INDEXER_IMAGE" . >/dev/null || exit 1
fi

# tarof lists an image's filesystem as `docker export` sees it: the only way to
# read a mode out of a distroless image, which has no shell to stat with.
tarof() {
	local image=$1 cid
	cid=$(docker create "$image") || return 1
	docker export "$cid" 2>/dev/null | tar -tvf - 2>/dev/null
	docker rm -f "$cid" >/dev/null 2>&1
}

# ------------------------------------------------------- the build context ---

echo
echo "build context"
# .dockerignore lives at the repository root and not in infra/, because with
# the context at the root BuildKit reads <dockerfile>.dockerignore or the root
# file and never infra/.dockerignore. Measured: with the file in infra/, docs/
# and assets/ were still in the context. Asserted by name for that reason.
ctx=$(docker build -q -f infra/gateway.Dockerfile --target build -t ct-imgtest-build-"$suffix" . >/dev/null 2>&1 &&
	docker run --rm --entrypoint /bin/sh ct-imgtest-build-"$suffix" -c 'ls -A /src' 2>/dev/null)
docker rmi -f ct-imgtest-build-"$suffix" >/dev/null 2>&1
contains "the build stage has the module sources" "$ctx" "packages"
no_entry "docs/ is excluded from the build context" "$ctx" "docs"
no_entry "assets/ is excluded from the build context" "$ctx" "assets"
no_entry ".git is excluded from the build context" "$ctx" ".git"

# ------------------------------------------------------------- gateway ---

echo
echo "gateway image ($GATEWAY_IMAGE)"

eq "runs as uid 65532" "$(docker inspect "$GATEWAY_IMAGE" --format '{{.Config.User}}')" "65532:65532"

# The whole set, not entry by entry: an assertion that one dangerous name is
# absent passes when a different dangerous name is added.
want_gw_env='PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
PORT=8080'
got_gw_env=$(docker inspect "$GATEWAY_IMAGE" --format '{{range .Config.Env}}{{println .}}{{end}}' | sed '/^$/d')
eq "Config.Env is exactly the written set" "$got_gw_env" "$want_gw_env"

bad=$(printf '%s\n' "$got_gw_env" | grep -E '^(GO|GIT_|[A-Za-z_]*_?(PROXY|proxy)=)' || true)
eq "no GO*, GIT_* or *_PROXY variable" "$bad" ""

# No shell at all, which is why the health check is a flag on the binary.
if docker run --rm --entrypoint /bin/sh "$GATEWAY_IMAGE" -c 'exit 0' >/dev/null 2>&1; then
	fail "the image has no shell"
else
	pass "the image has no shell"
fi

gw_tar=$(tarof "$GATEWAY_IMAGE")
bundle_line=$(printf '%s\n' "$gw_tar" | grep 'etc/ssl/certs/rds-global-bundle.pem' || true)
eq "the RDS trust store's mode is 644" "$(printf '%s' "$bundle_line" | awk '{print $1}')" "-rw-r--r--"
bundle_bytes=$(printf '%s' "$bundle_line" | awk '{print $3}')
if [ -n "$bundle_bytes" ] && [ "$bundle_bytes" -gt 100000 ]; then
	pass "the RDS trust store is non-empty ($bundle_bytes bytes)"
else
	fail "the RDS trust store is non-empty: got [$bundle_bytes] bytes"
fi

# ------------------------------------------------------------- indexer ---

echo
echo "indexer image ($INDEXER_IMAGE)"

eq "runs as uid 65532" "$(docker inspect "$INDEXER_IMAGE" --format '{{.Config.User}}')" "65532:65532"
eq "the process's own uid is 65532" "$(docker run --rm --entrypoint /bin/sh "$INDEXER_IMAGE" -c 'id -u')" "65532"

want_ix_env='PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin
HOME=/scratch
SCRATCH_DIR=/scratch
PROBE_PORT=9090'
got_ix_env=$(docker inspect "$INDEXER_IMAGE" --format '{{range .Config.Env}}{{println .}}{{end}}' | sed '/^$/d')
eq "Config.Env is exactly the written set" "$got_ix_env" "$want_ix_env"

bad=$(printf '%s\n' "$got_ix_env" | grep -E '^(GO|GIT_|[A-Za-z_]*_?(PROXY|proxy)=)' || true)
eq "no GO*, GIT_* or *_PROXY variable" "$bad" ""

sh_in_ix() { docker run --rm --entrypoint /bin/sh "$INDEXER_IMAGE" -c "$1" 2>&1; }

contains "go version is 1.27" "$(sh_in_ix 'go version')" "go1.27"
eq "command -v go resolves the copied toolchain" "$(sh_in_ix 'command -v go')" "/usr/local/go/bin/go"
# By path and by count, not by exit code: policy.go:141-147 pins
# exec.LookPath("go") to GoBin, so a second go on PATH is a worker that logs
# no_toolchain and writes an all-syntactic graph while `go version` works.
# shellcheck disable=SC2016  # $PATH must expand inside the container, not here.
gos=$(sh_in_ix 'IFS=:; for d in $PATH; do [ -x "$d/go" ] && echo "$d/go"; done')
eq "exactly one go on PATH" "$gos" "/usr/local/go/bin/go"
contains "git is installed" "$(sh_in_ix 'git --version')" "git version"

# A FROM golang:1.27 runtime base is caught here and nowhere else: it would
# answer every other assertion in this file identically.
eq "no GOPATH inherited from a golang base" "$(sh_in_ix 'go env GOPATH')" "/scratch/go"
eq "no /go tree in the image" "$(sh_in_ix 'test -d /go; echo $?')" "1"
# shellcheck disable=SC2016  # the go env call runs inside the container.
eq "no warm module cache" "$(sh_in_ix 'test -d "$(go env GOMODCACHE)"; echo $?')" "1"
eq "no /root build cache" "$(docker run --rm --user 0 --entrypoint /bin/sh "$INDEXER_IMAGE" -c 'test -d /root/.cache/go-build; echo $?')" "1"

# go env against a written expected set, value by value. The last three are
# supplied by GOROOT/go.env, which travels inside /usr/local/go: the honest
# claim is that Policy.Env's real environment variables outrank a baked-in
# defaults file, not that nothing is baked in. GOENV=off closes the *user* env
# file and not this one.
goenv_want='CGO_ENABLED=0
GOCACHE=/scratch/.cache/go-build
GOEXPERIMENT=
GOFLAGS=
GOINSECURE=
GOMODCACHE=/scratch/go/pkg/mod
GOPACKAGESDRIVER=
GOPATH=/scratch/go
GOPRIVATE=
GOPROXY=https://proxy.golang.org,direct
GOROOT=/usr/local/go
GOSUMDB=sum.golang.org
GOTMPDIR=
GOTOOLCHAIN=auto
GOVCS=
GOWORK='
goenv_got=$(docker run --rm --entrypoint /usr/local/go/bin/go "$INDEXER_IMAGE" env \
	CGO_ENABLED GOCACHE GOEXPERIMENT GOFLAGS GOINSECURE GOMODCACHE GOPACKAGESDRIVER \
	GOPATH GOPRIVATE GOPROXY GOROOT GOSUMDB GOTMPDIR GOTOOLCHAIN GOVCS GOWORK 2>/dev/null |
	paste -d= <(printf '%s\n' CGO_ENABLED GOCACHE GOEXPERIMENT GOFLAGS GOINSECURE GOMODCACHE \
		GOPACKAGESDRIVER GOPATH GOPRIVATE GOPROXY GOROOT GOSUMDB GOTMPDIR GOTOOLCHAIN GOVCS GOWORK) -)
eq "go env matches the written expected set" "$goenv_got" "$goenv_want"

ix_tar=$(tarof "$INDEXER_IMAGE")
bundle_line=$(printf '%s\n' "$ix_tar" | grep 'etc/ssl/certs/rds-global-bundle.pem' || true)
eq "the RDS trust store's mode is 644" "$(printf '%s' "$bundle_line" | awk '{print $1}')" "-rw-r--r--"
# Reading it as the runtime user, which is the assertion the mode is a proxy
# for: ADD writes 0600 root-owned by default and this process is uid 65532.
eq "the runtime user can read the RDS trust store" \
	"$(sh_in_ix 'grep -c "BEGIN CERTIFICATE" /etc/ssl/certs/rds-global-bundle.pem 2>&1')" "108"

# The one assertion that catches a missing ca-certificates, done by doing it
# rather than by asking dpkg: git only Recommends the package and
# --no-install-recommends drops it, and the image looks entirely healthy right
# up to the point it is asked to do the thing the indexer exists for.
if clone_out=$(docker run --rm --read-only --tmpfs /scratch:mode=1777 --tmpfs /tmp:mode=1777 \
	--entrypoint /bin/sh "$INDEXER_IMAGE" \
	-c "git clone --depth 1 --quiet '$CLONE_URL' /scratch/clone && ls /scratch/clone" 2>&1); then
	pass "git clone https:// succeeds from inside the image"
else
	fail "git clone https:// succeeds from inside the image: $clone_out"
fi

# ------------------------------------------------ both, under a read-only fs ---

echo
echo "both images under a read-only root filesystem"

docker network create "$net" >/dev/null || exit 1
docker run -d --name "$pg" --network "$net" \
	-e POSTGRES_USER=codetrail -e POSTGRES_PASSWORD=codetrail -e POSTGRES_DB=codetrail \
	pgvector/pgvector:pg17 >/dev/null || exit 1
# From inside the network, not with `docker exec`: the pgvector image's
# entrypoint runs initdb against a localhost-only server first, so pg_isready
# in the container passes while a TCP connect from a peer is still refused —
# which is a gateway that log.Fatals on boot and a test that reports a broken
# image. Measured, not guessed at.
for _ in $(seq 90); do
	docker run --rm --network "$net" pgvector/pgvector:pg17 \
		pg_isready -h "$pg" -U codetrail -d codetrail >/dev/null 2>&1 && break
	sleep 1
done

dsn="postgres://codetrail:codetrail@$pg:5432/codetrail?sslmode=disable"

# --read-only with /scratch and /tmp as the only writable paths, which is what
# Task 5's task definition sets. If the image cannot run this way, Task 5 will
# quietly not set it — which is why this assertion is here and not there.
docker run -d --name "$gw" --network "$net" -p 18080:8080 \
	--read-only --tmpfs /tmp:mode=1777 \
	-e DATABASE_URL="$dsn" -e EMBED_PROVIDER=fake "$GATEWAY_IMAGE" >/dev/null || exit 1
docker run -d --name "$ix" --network "$net" -p 19090:9090 \
	--read-only --tmpfs /scratch:mode=1777 --tmpfs /tmp:mode=1777 \
	-e DATABASE_URL="$dsn" -e EMBED_PROVIDER=fake "$INDEXER_IMAGE" >/dev/null || exit 1

probe_http() {
	local port=$1 path=$2
	for _ in $(seq 45); do
		code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port$path" 2>/dev/null)
		[ "$code" = "200" ] && { echo 200; return; }
		sleep 1
	done
	echo "$code"
}

for svc in "gateway:18080:$gw" "indexer:19090:$ix"; do
	name=${svc%%:*}
	rest=${svc#*:}
	port=${rest%%:*}
	cname=${rest#*:}
	for path in /health /ready /metrics; do
		got=$(probe_http "$port" "$path")
		if [ "$got" = "200" ]; then
			pass "$name answers $path read-only"
		else
			fail "$name answers $path read-only: got [$got]"
			docker logs "$cname" 2>&1 | tail -5
		fi
	done
done

# spec:270 makes the gauge the signal, so an endpoint answering 200 with the
# gauge at 0 is a wiring bug nothing else sees.
contains "the gateway publishes codetrail_ready 1" "$(curl -s http://127.0.0.1:18080/metrics)" "codetrail_ready 1"
contains "the indexer publishes codetrail_ready 1" "$(curl -s http://127.0.0.1:19090/metrics)" "codetrail_ready 1"
# P4's three, which have had nowhere to be scraped from until now.
contains "the indexer publishes its graph counters" "$(curl -s http://127.0.0.1:19090/metrics)" "codetrail_graph_edges_total"

# The health check Task 5's task definition runs. No shell in the gateway
# image, so this is also the only thing that could make the request.
if docker exec "$gw" /gateway -probe >/dev/null 2>&1; then
	pass "gateway -probe exits 0 against a live /health"
else
	fail "gateway -probe exits 0 against a live /health"
fi
if docker exec "$ix" /indexer -probe >/dev/null 2>&1; then
	pass "indexer -probe exits 0 against a live /health"
else
	fail "indexer -probe exits 0 against a live /health"
fi
# And says no when there is nothing there, or it is not a check.
if docker run --rm --entrypoint /indexer -e PROBE_PORT=9999 "$INDEXER_IMAGE" -probe >/dev/null 2>&1; then
	fail "-probe exits non-zero when nothing is listening"
else
	pass "-probe exits non-zero when nothing is listening"
fi

echo
if [ "$fails" -ne 0 ]; then
	echo "$fails assertion(s) failed"
	exit 1
fi
echo "all image assertions passed"

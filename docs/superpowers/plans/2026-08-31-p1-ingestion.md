# codetrail P1 — Ingestion Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Accept a public git URL through the API, clone it under a sandbox with enforced caps, record its files, and evict the least-recently-queried repo when the quota is reached.

**Architecture:** A `gateway` HTTP endpoint validates a submitted URL against an allowlist policy and enqueues a job row in Postgres. A separate `indexer` binary leases jobs, shells out to `git clone` under a wall-clock deadline and byte cap, walks the checkout skipping every non-regular file, records `repos` and `files`, and deletes the scratch directory. Eviction runs after each successful index.

**Tech Stack:** Go 1.27, Echo v4, pgx v5 + pgxpool, Postgres 17 + pgvector, zerolog, Prometheus client_golang. No new third-party dependencies are introduced by this plan.

**Spec:** `docs/superpowers/specs/2026-08-31-codetrail-design.md` — read §2 (architecture), §3 (data model), §4 (ingestion and the sandbox), §10 (error handling) before starting.

## Global Constraints

- Go **1.27**; module `github.com/mralaminahamed/codetrail`.
- Default branch is **`trunk`**. Branch from it, one PR per task, **merge commits, never squash**.
- Commit author and committer must be `Al Amin Ahamed <alamin.ahamed.dev@gmail.com>`. A pre-push hook at `../.githooks/pre-push` enforces this; do not bypass it.
- Comments are **minimal and short** — explain *why*, never restate *what*. No comment that a reader could derive from the line below it.
- Line numbers everywhere are **1-based and inclusive** (spec §3).
- Migrations are named `NNNN_name.sql`, applied in filename order, and must be safe to run twice (spec §3, `store.migrate`).
- Every load-bearing test must be shown to fail when the behaviour it pins is broken. A test that cannot be broken on purpose is not evidence.
- `gofmt -l apps packages` must be empty and `go vet ./...` clean before every commit.
- Admission rejections return **400 naming which rule failed**; an evicted repo returns **410 Gone**, never 404 (spec §10).

---

### Task 1: Green CI, including live Postgres

Finishes P0's outstanding work. Every later task's tests are only evidence if CI actually runs them, and the spec (§12) requires the `-tags=live` tests to run in CI from P0 rather than depending on someone exporting a connection string.

**Files:**
- Create: `.github/workflows/ci.yml`
- Modify: `README.md` — add the CI badge
- Test: existing `packages/shared/store/store_test.go`, `store_live_test.go`, `packages/shared/config/config_test.go`, `packages/shared/health/health_test.go`

**Interfaces:**
- Consumes: `store.New`, `store.MigrationNames`, `store.CheckDim` (already written in the scaffold).
- Produces: nothing importable. A working CI pipeline other tasks rely on.

- [ ] **Step 1: Confirm the existing tests pass locally against real Postgres**

```bash
docker compose -f infra/docker-compose.yml up -d postgres
until docker compose -f infra/docker-compose.yml exec -T postgres pg_isready -U codetrail -d codetrail; do sleep 1; done
go test ./... -count=1
DATABASE_URL='postgres://codetrail:codetrail@localhost:55432/codetrail?sslmode=disable' \
  go test -tags=live ./packages/shared/store/ -count=1 -v
```

Expected: both PASS. If the live run fails, fix that before writing CI — CI is not the place to discover a broken test.

- [ ] **Step 2: Write the CI workflow**

Create `.github/workflows/ci.yml`. Actions are pinned to commit SHAs: this runs on every pull request, from a branch its author controls, so what it executes must not be repointable.

```yaml
name: CI

on:
  push: { branches: [trunk] }
  pull_request:

jobs:
  go:
    runs-on: ubuntu-latest
    services:
      postgres:
        image: pgvector/pgvector:pg17
        env:
          POSTGRES_USER: codetrail
          POSTGRES_PASSWORD: codetrail
          POSTGRES_DB: codetrail
        ports: ["5432:5432"]
        options: >-
          --health-cmd "pg_isready -U codetrail -d codetrail"
          --health-interval 5s --health-timeout 5s --health-retries 20
    env:
      DATABASE_URL: postgres://codetrail:codetrail@127.0.0.1:5432/codetrail?sslmode=disable
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
      - uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0
        with: { go-version: "1.27", cache: true }
      - name: gofmt
        run: test -z "$(gofmt -l apps packages)" || { gofmt -l apps packages; exit 1; }
      - name: vet
        run: go vet ./...
      - name: build
        run: go build ./...
      - name: test
        run: go test ./... -count=1
      # The tests that matter most. Every hermetic test in the tree can pass
      # against a schema Postgres would reject.
      - name: live datastore tests
        run: go test -tags=live -count=1 ./packages/shared/...
```

- [ ] **Step 3: Add the CI badge to the README**

`README.md` already exists and is written; it needs only the badge, added to the badge block under
the title so a reader can see the build state without leaving the page:

```markdown
[![CI](https://github.com/mralaminahamed/codetrail/actions/workflows/ci.yml/badge.svg)](https://github.com/mralaminahamed/codetrail/actions/workflows/ci.yml)
```

- [ ] **Step 4: Verify the workflow parses**

```bash
python3 -c "import yaml; yaml.safe_load(open('.github/workflows/ci.yml')); print('parses')"
```

Expected: `parses`

- [ ] **Step 5: Commit and push, then confirm CI is green**

```bash
git add .github/workflows/ci.yml README.md
git commit -m "ci: run hermetic and live datastore tests

The -tags=live store tests only prove anything if something runs them.
CI gets a pgvector service container so they run on every push and pull
request rather than when someone remembers to export DATABASE_URL."
git push
gh run watch --exit-status
```

Expected: the run completes green, and its `live datastore tests` step shows `ok  .../packages/shared/store`.

---

> **On the mutation commands below.** They are written as `sed` for brevity and
> several span more than one line, which `sed` will not match. If one does not
> apply, make the same edit by hand — the mutation is the point, not the tool.
> Always confirm the file changed (`git diff --stat`) before trusting a test run,
> and always `git checkout` the file afterwards. Note that a mutated file that
> fails to *compile* is a weak kill: it proves the compiler noticed, not that a
> test did. Rewrite it so it compiles, then re-run.

### Task 2: Admission policy

Pure, hermetic, and the first line of the sandbox. No network, no filesystem, no database — which is exactly why it can be tested exhaustively.

**Files:**
- Create: `packages/shared/admit/admit.go`
- Test: `packages/shared/admit/admit_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type Rule string` with `RuleForm`, `RuleScheme`, `RuleHost`
  - `type Error struct { Rule Rule; Detail string }` implementing `error`
  - `type Remote struct { URL, Key, Host, Owner, Name string }` — `Key` is the case-folded
    identity, `URL` the submitted spelling
  - `type Policy struct{ ... }`, `func NewPolicy(hosts []string) Policy`
  - `func (p Policy) Check(raw string) (Remote, error)`
  - `var DefaultHosts = []string{"github.com", "codeberg.org"}`

- [ ] **Step 1: Write the failing test**

Create `packages/shared/admit/admit_test.go`:

```go
package admit

import (
	"errors"
	"testing"
)

func policy() Policy { return NewPolicy(DefaultHosts) }

func TestAcceptsAnAllowlistedHTTPSRemote(t *testing.T) {
	got, err := policy().Check("https://github.com/mralaminahamed/codetrail")
	if err != nil {
		t.Fatalf("want accepted, got %v", err)
	}
	if got.Host != "github.com" || got.Owner != "mralaminahamed" || got.Name != "codetrail" {
		t.Fatalf("parsed wrong: %+v", got)
	}
}

// file:// alone would turn "index a repo" into "read the indexer's disk".
func TestRejectsEverySchemeButHTTPS(t *testing.T) {
	for _, raw := range []string{
		"file:///etc/passwd",
		"git://github.com/x/y",
		"ssh://git@github.com/x/y",
		"http://github.com/x/y",
	} {
		_, err := policy().Check(raw)
		var e *Error
		if !errors.As(err, &e) || e.Rule != RuleScheme {
			t.Fatalf("%s: want a scheme rejection, got %v", raw, err)
		}
	}
}

// An allowlist, not a denylist: the interesting targets are the ones nobody
// thought to deny. These are the shapes an SSRF attempt actually takes.
func TestRejectsHostsOutsideTheAllowlist(t *testing.T) {
	for _, raw := range []string{
		"https://169.254.169.254/latest/meta-data",
		"https://localhost/x/y",
		"https://127.0.0.1/x/y",
		"https://[::1]/x/y",
		"https://10.0.0.5/x/y",
		"https://internal.corp/x/y",
		// A lookalike: the allowlist is exact hosts, not suffixes.
		"https://github.com.evil.example/x/y",
		"https://notgithub.com/x/y",
	} {
		_, err := policy().Check(raw)
		var e *Error
		if !errors.As(err, &e) || e.Rule != RuleHost {
			t.Fatalf("%s: want a host rejection, got %v", raw, err)
		}
	}
}

// A subdomain of an allowlisted host is a different host.
func TestRejectsSubdomainsOfAllowlistedHosts(t *testing.T) {
	_, err := policy().Check("https://pages.github.com/x/y")
	var e *Error
	if !errors.As(err, &e) || e.Rule != RuleHost {
		t.Fatalf("want a host rejection, got %v", err)
	}
}

// Userinfo can smuggle a different authority past a careless reader; port
// tricks do the same. Neither has a legitimate use here.
func TestRejectsUserinfoAndPorts(t *testing.T) {
	for _, raw := range []string{
		"https://github.com@evil.example/x/y",
		"https://user:pass@github.com/x/y",
		"https://github.com:8080/x/y",
	} {
		_, err := policy().Check(raw)
		var e *Error
		if !errors.As(err, &e) {
			t.Fatalf("%s: want a rejection, got %v", raw, err)
		}
	}
}

func TestRejectsMalformedAndIncompletePaths(t *testing.T) {
	for _, raw := range []string{
		"",
		"   ",
		"not a url",
		"https://github.com",
		"https://github.com/onlyowner",
		"https://github.com//",
	} {
		if _, err := policy().Check(raw); err == nil {
			t.Fatalf("%q: want a rejection, got none", raw)
		}
	}
}

// Host matching is case-insensitive, and a trailing .git or slash is the same
// repository — normalising here means the job dedupe index sees one key.
func TestNormalises(t *testing.T) {
	for _, raw := range []string{
		"https://GitHub.com/Owner/Repo",
		"https://github.com/Owner/Repo.git",
		"https://github.com/Owner/Repo/",
	} {
		got, err := policy().Check(raw)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if got.URL != "https://github.com/Owner/Repo" {
			t.Fatalf("%s normalised to %q", raw, got.URL)
		}
	}
}

// The caller has to be able to tell an operator which rule refused them, so
// the message must name the rule and the offending value.
func TestErrorNamesTheRuleAndTheValue(t *testing.T) {
	_, err := policy().Check("https://evil.example/x/y")
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	for _, want := range []string{"host", "evil.example"} {
		if !contains(msg, want) {
			t.Fatalf("error %q does not mention %q", msg, want)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./packages/shared/admit/ -count=1`
Expected: FAIL — `undefined: NewPolicy`, `undefined: DefaultHosts`, etc.

- [ ] **Step 3: Write the implementation**

Create `packages/shared/admit/admit.go`:

```go
// Package admit decides whether a submitted repository URL may be cloned.
//
// It runs before anything touches the network or the disk, and it is the only
// SSRF control codetrail has: an exact-host allowlist. It does not defend
// against a hostile allowlisted forge, and it claims no DNS-rebinding
// protection — git is a subprocess and cannot be handed a validating dialer.
package admit

import (
	"fmt"
	"net/url"
	"strings"
)

// Rule names the check that refused a URL, so a 400 can say which one.
type Rule string

const (
	RuleForm   Rule = "form"
	RuleScheme Rule = "scheme"
	RuleHost   Rule = "host"
)

// Error is a refusal. Callers match on Rule to build the response.
type Error struct {
	Rule   Rule
	Detail string
}

func (e *Error) Error() string { return fmt.Sprintf("admit: %s: %s", e.Rule, e.Detail) }

// Remote is an accepted, normalised repository reference.
//
// URL keeps the case the submitter typed, because a forge preserves the
// display case of an owner and a repository and that is what a citation has
// to show. Key is the identity: forges match owner and name
// case-insensitively, so two spellings are one repository and anything that
// keys on a repository keys on this, not on URL.
type Remote struct {
	URL   string
	Key   string
	Host  string
	Owner string
	Name  string
}

// DefaultHosts is the allowlist a deployment gets if it configures none.
//
// gitlab.com is deliberately absent: GitLab nests namespaces arbitrarily
// (group/subgroup/repo), which the /owner/name path check refuses, so shipping
// it by default would advertise a forge whose typical URL we reject. Adding it
// back means teaching Check about nested namespaces first.
var DefaultHosts = []string{"github.com", "codeberg.org"}

type Policy struct{ hosts map[string]bool }

func NewPolicy(hosts []string) Policy {
	m := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			m[h] = true
		}
	}
	return Policy{hosts: m}
}

// Check validates raw and returns the normalised remote.
func (p Policy) Check(raw string) (Remote, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Remote{}, &Error{RuleForm, "empty URL"}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Remote{}, &Error{RuleForm, fmt.Sprintf("%q is not a URL", raw)}
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return Remote{}, &Error{RuleScheme, fmt.Sprintf("scheme %q; only https is accepted", u.Scheme)}
	}
	// Userinfo before an authority is how a different host gets smuggled past
	// a careless reader, and a port is not something a forge needs here.
	if u.User != nil {
		return Remote{}, &Error{RuleForm, "credentials in the URL"}
	}
	if u.Port() != "" {
		return Remote{}, &Error{RuleForm, fmt.Sprintf("port %q", u.Port())}
	}
	host := strings.ToLower(u.Hostname())
	if !p.hosts[host] {
		return Remote{}, &Error{RuleHost, fmt.Sprintf("%s is not an allowed host", host)}
	}

	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return Remote{}, &Error{RuleForm, "path must be /owner/name"}
	}
	// Repeatedly, not once: a forge serves /owner/foo.git and /owner/foo.git.git
	// as the same repository, and one trim would leave "foo.git" as a second
	// identity for it. No forge here allows a name that really ends in ".git".
	owner, name := parts[0], parts[1]
	for strings.HasSuffix(name, ".git") {
		name = strings.TrimSuffix(name, ".git")
	}
	if name == "" {
		return Remote{}, &Error{RuleForm, "path must be /owner/name"}
	}
	if !validSegment(owner) {
		return Remote{}, &Error{RuleForm, fmt.Sprintf("owner %q is not a plain name", owner)}
	}
	if !validSegment(name) {
		return Remote{}, &Error{RuleForm, fmt.Sprintf("name %q is not a plain name", name)}
	}
	return Remote{
		URL:   "https://" + host + "/" + owner + "/" + name,
		Key:   host + "/" + strings.ToLower(owner) + "/" + strings.ToLower(name),
		Host:  host,
		Owner: owner,
		Name:  name,
	}, nil
}

// Owner and Name are exported, so anything downstream may join them into a
// path. Allowlist the charset rather than blacklisting the escapes, and refuse
// the two relative names the charset would otherwise let through.
func validSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./packages/shared/admit/ -count=1 -v`
Expected: PASS.

- [ ] **Step 5: Prove the tests discriminate**

Apply each mutation, confirm a test fails, then revert. A guard whose test still passes when the guard is gone is not a guard.

```bash
# M1: accept any scheme
sed -i 's|if !strings.EqualFold(u.Scheme, "https") {|if false {|' packages/shared/admit/admit.go
go test ./packages/shared/admit/ -count=1 2>&1 | grep -E '^--- FAIL' # expect TestRejectsEverySchemeButHTTPS
git checkout packages/shared/admit/admit.go

# M2: suffix match instead of exact host
sed -i 's|if !p.hosts\[host\] {|if !p.hosts[host] \&\& !strings.HasSuffix(host, "github.com") {|' packages/shared/admit/admit.go
go test ./packages/shared/admit/ -count=1 2>&1 | grep -E '^--- FAIL' # expect the lookalike + subdomain tests
git checkout packages/shared/admit/admit.go

# M3: ignore userinfo
sed -i 's|if u.User != nil {|if false {|' packages/shared/admit/admit.go
go test ./packages/shared/admit/ -count=1 2>&1 | grep -E '^--- FAIL' # expect TestRejectsUserinfoAndPorts
git checkout packages/shared/admit/admit.go
```

Expected: each mutation names at least one failing test. If any mutation survives, the test set is incomplete — add a case before continuing.

- [ ] **Step 6: Commit**

```bash
gofmt -l apps packages && go vet ./... && go test ./... -count=1
git add packages/shared/admit
git commit -m "feat: admission policy for submitted repository URLs

An exact-host allowlist, https only, no userinfo, no ports, /owner/name.
Refusals name the rule that fired so the API can say which one, rather
than answering a generic 400.

Exact hosts, not suffixes: github.com.evil.example and pages.github.com
are both rejected, and both have a test. Three mutations confirmed the
tests discriminate."
```

---

### Task 3: Durable job queue

**Files:**
- Create: `packages/shared/store/migrations/0003_jobs.sql`
- Create: `packages/shared/jobs/jobs.go`
- Test: `packages/shared/jobs/jobs_live_test.go`

**Interfaces:**
- Consumes: `store.New`, `(*store.Store).Pool()`.
- Produces:
  - `type Status string` with `StatusPending`, `StatusLeased`, `StatusDone`, `StatusFailed`
  - `type Job struct { ID, Remote, Ref string; Status Status; Attempts int; Error string }`
  - `func New(pool *pgxpool.Pool) *Queue`
  - `func (q *Queue) Enqueue(ctx context.Context, remote, ref string) (Job, error)`
  - `func (q *Queue) Lease(ctx context.Context, worker string, d time.Duration) (Job, bool, error)`
  - `func (q *Queue) Complete(ctx context.Context, id string) error`
  - `func (q *Queue) Fail(ctx context.Context, id, reason string, maxAttempts int) error`
  - `func (q *Queue) Get(ctx context.Context, id string) (Job, error)`
  - `var ErrNotFound = errors.New("jobs: not found")`

- [ ] **Step 1: Write the migration**

Create `packages/shared/store/migrations/0003_jobs.sql`:

```sql
CREATE TABLE jobs (
    id           TEXT PRIMARY KEY,
    remote       TEXT NOT NULL,
    ref          TEXT NOT NULL,
    status       TEXT NOT NULL,
    attempts     INTEGER NOT NULL DEFAULT 0,
    leased_by    TEXT,
    leased_until TIMESTAMPTZ,
    error        TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Submitting a repository that is already queued returns the existing job
-- rather than cloning it twice. Partial, so a finished job does not block a
-- later re-index of the same remote.
CREATE UNIQUE INDEX jobs_active_idx ON jobs (remote, ref)
    WHERE status IN ('pending', 'leased');

-- Lease() orders by created_at among claimable rows.
CREATE INDEX jobs_claimable_idx ON jobs (status, leased_until, created_at);
```

- [ ] **Step 2: Write the failing test**

Create `packages/shared/jobs/jobs_live_test.go`. These need a real Postgres — the behaviour under test *is* the SQL (the partial index, `FOR UPDATE SKIP LOCKED`, lease expiry), and a fake would be testing the fake.

```go
//go:build live

package jobs

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/mralaminahamed/codetrail/packages/shared/store"
)

func queue(t *testing.T) *Queue {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("set DATABASE_URL to run")
	}
	st, err := store.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	// Each test owns the table: leases and dedupe are global properties.
	if _, err := st.Pool().Exec(context.Background(), `DELETE FROM jobs`); err != nil {
		t.Fatal(err)
	}
	return New(st.Pool())
}

func TestEnqueueThenLeaseThenComplete(t *testing.T) {
	ctx := context.Background()
	q := queue(t)

	j, err := q.Enqueue(ctx, "https://github.com/a/b", "main")
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != StatusPending {
		t.Fatalf("want pending, got %q", j.Status)
	}

	got, ok, err := q.Lease(ctx, "worker-1", time.Minute)
	if err != nil || !ok {
		t.Fatalf("want a leased job, got ok=%v err=%v", ok, err)
	}
	if got.ID != j.ID || got.Status != StatusLeased {
		t.Fatalf("leased the wrong job: %+v", got)
	}
	if got.Attempts != 1 {
		t.Fatalf("want attempts 1, got %d", got.Attempts)
	}

	if err := q.Complete(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	after, err := q.Get(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != StatusDone {
		t.Fatalf("want done, got %q", after.Status)
	}
}

// Submitting the same repository twice while it is queued must return the
// job that already exists, not clone it twice.
func TestEnqueueIsIdempotentWhileActive(t *testing.T) {
	ctx := context.Background()
	q := queue(t)

	first, err := q.Enqueue(ctx, "https://github.com/a/b", "main")
	if err != nil {
		t.Fatal(err)
	}
	second, err := q.Enqueue(ctx, "https://github.com/a/b", "main")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("want the same job back, got %s then %s", first.ID, second.ID)
	}

	// Once terminal, the same remote may be queued again — that is a re-index.
	if err := q.Complete(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	third, err := q.Enqueue(ctx, "https://github.com/a/b", "main")
	if err != nil {
		t.Fatal(err)
	}
	if third.ID == first.ID {
		t.Fatal("a finished job must not block a re-index")
	}
}

// Two workers leasing at once must not get the same job. This is what
// FOR UPDATE SKIP LOCKED buys, and it cannot be tested without concurrency.
func TestLeaseHandsEachJobToOneWorker(t *testing.T) {
	ctx := context.Background()
	q := queue(t)
	const n = 8
	for i := range n {
		if _, err := q.Enqueue(ctx, "https://github.com/a/r"+string(rune('a'+i)), "main"); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	for w := range n {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			j, ok, err := q.Lease(ctx, "worker", time.Minute)
			if err != nil || !ok {
				return
			}
			mu.Lock()
			seen[j.ID]++
			mu.Unlock()
			_ = w
		}(w)
	}
	wg.Wait()

	for id, count := range seen {
		if count != 1 {
			t.Fatalf("job %s was leased %d times", id, count)
		}
	}
	if len(seen) == 0 {
		t.Fatal("no job was leased at all")
	}
}

// A dead worker's job must come back. Without this a crash loses work
// silently, which is the failure nobody notices until a queue stops draining.
func TestExpiredLeaseIsReclaimed(t *testing.T) {
	ctx := context.Background()
	q := queue(t)
	j, err := q.Enqueue(ctx, "https://github.com/a/b", "main")
	if err != nil {
		t.Fatal(err)
	}
	// A lease that has already expired: the worker took it and died.
	if _, ok, err := q.Lease(ctx, "dead-worker", -time.Second); err != nil || !ok {
		t.Fatalf("first lease failed: ok=%v err=%v", ok, err)
	}
	got, ok, err := q.Lease(ctx, "live-worker", time.Minute)
	if err != nil || !ok {
		t.Fatalf("want the expired lease reclaimed, got ok=%v err=%v", ok, err)
	}
	if got.ID != j.ID {
		t.Fatalf("reclaimed the wrong job: %s", got.ID)
	}
	if got.Attempts != 2 {
		t.Fatalf("want attempts 2 after a reclaim, got %d", got.Attempts)
	}
}

// Failures retry until the cap, then stop. A job that retries forever is a
// job that fails forever, loudly and expensively.
func TestFailRetriesUntilTheCap(t *testing.T) {
	ctx := context.Background()
	q := queue(t)
	j, err := q.Enqueue(ctx, "https://github.com/a/b", "main")
	if err != nil {
		t.Fatal(err)
	}
	const max = 2

	if _, _, err := q.Lease(ctx, "w", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := q.Fail(ctx, j.ID, "clone failed", max); err != nil {
		t.Fatal(err)
	}
	mid, _ := q.Get(ctx, j.ID)
	if mid.Status != StatusPending {
		t.Fatalf("attempt 1 of %d must go back to pending, got %q", max, mid.Status)
	}

	if _, _, err := q.Lease(ctx, "w", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := q.Fail(ctx, j.ID, "clone failed again", max); err != nil {
		t.Fatal(err)
	}
	end, _ := q.Get(ctx, j.ID)
	if end.Status != StatusFailed {
		t.Fatalf("want failed at the cap, got %q", end.Status)
	}
	if end.Error != "clone failed again" {
		t.Fatalf("want the terminal reason recorded, got %q", end.Error)
	}
}

func TestLeaseOnAnEmptyQueue(t *testing.T) {
	_, ok, err := queue(t).Lease(context.Background(), "w", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("want no job from an empty queue")
	}
}

func TestGetUnknownJob(t *testing.T) {
	_, err := queue(t).Get(context.Background(), "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `DATABASE_URL='postgres://codetrail:codetrail@localhost:55432/codetrail?sslmode=disable' go test -tags=live ./packages/shared/jobs/ -count=1`
Expected: FAIL — the package does not compile, `undefined: New`.

- [ ] **Step 4: Write the implementation**

Create `packages/shared/jobs/jobs.go`:

```go
// Package jobs is the indexing queue: a Postgres table, not a broker.
//
// One producer, durable, retryable, and inspectable with psql. A broker would
// be a second system to run and justify for a queue this shape.
package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Status string

const (
	StatusPending Status = "pending"
	StatusLeased  Status = "leased"
	StatusDone    Status = "done"
	StatusFailed  Status = "failed"
)

var ErrNotFound = errors.New("jobs: not found")

type Job struct {
	ID       string `json:"id"`
	Remote   string `json:"remote"`
	Ref      string `json:"ref"`
	Status   Status `json:"status"`
	Attempts int    `json:"attempts"`
	Error    string `json:"error,omitempty"`
}

type Queue struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Queue { return &Queue{pool: pool} }

const cols = `id, remote, ref, status, attempts, error`

func scan(row pgx.Row) (Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.Remote, &j.Ref, &j.Status, &j.Attempts, &j.Error)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	return j, err
}

// Enqueue adds a job, or returns the active one for the same remote and ref.
// The dedupe is the partial unique index in 0003, so two gateways racing on
// the same submission produce one job rather than one job and one error.
func (q *Queue) Enqueue(ctx context.Context, remote, ref string) (Job, error) {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%s\x00%d", remote, ref, time.Now().UnixNano()))
	id := hex.EncodeToString(sum[:16])

	j, err := scan(q.pool.QueryRow(ctx, `
		INSERT INTO jobs (id, remote, ref, status)
		VALUES ($1, $2, $3, 'pending')
		ON CONFLICT DO NOTHING
		RETURNING `+cols, id, remote, ref))
	if err == nil {
		return j, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Job{}, err
	}
	// ON CONFLICT DO NOTHING returned no row: an active job already exists.
	return scan(q.pool.QueryRow(ctx, `
		SELECT `+cols+` FROM jobs
		WHERE remote = $1 AND ref = $2 AND status IN ('pending','leased')`, remote, ref))
}

// Lease claims the oldest claimable job for d. SKIP LOCKED is what lets two
// indexers poll the same table without handing both the same row.
func (q *Queue) Lease(ctx context.Context, worker string, d time.Duration) (Job, bool, error) {
	j, err := scan(q.pool.QueryRow(ctx, `
		WITH claimed AS (
			SELECT id FROM jobs
			WHERE status = 'pending'
			   OR (status = 'leased' AND leased_until < now())
			ORDER BY created_at
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		UPDATE jobs SET
			status       = 'leased',
			attempts     = attempts + 1,
			leased_by    = $1,
			leased_until = now() + $2::interval,
			updated_at   = now()
		WHERE id IN (SELECT id FROM claimed)
		RETURNING `+cols,
		worker, fmt.Sprintf("%d milliseconds", d.Milliseconds())))
	if errors.Is(err, ErrNotFound) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	return j, true, nil
}

func (q *Queue) Complete(ctx context.Context, id string) error {
	_, err := q.pool.Exec(ctx,
		`UPDATE jobs SET status='done', leased_by=NULL, leased_until=NULL, error='', updated_at=now() WHERE id=$1`, id)
	return err
}

// Fail returns the job to the queue, or marks it terminally failed once it
// has used its attempts. The reason is kept either way: a job that retried
// and then succeeded still explains why it retried.
func (q *Queue) Fail(ctx context.Context, id, reason string, maxAttempts int) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE jobs SET
			status = CASE WHEN attempts >= $3 THEN 'failed' ELSE 'pending' END,
			leased_by = NULL, leased_until = NULL,
			error = $2, updated_at = now()
		WHERE id = $1`, id, reason, maxAttempts)
	return err
}

func (q *Queue) Get(ctx context.Context, id string) (Job, error) {
	return scan(q.pool.QueryRow(ctx, `SELECT `+cols+` FROM jobs WHERE id = $1`, id))
}
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
DATABASE_URL='postgres://codetrail:codetrail@localhost:55432/codetrail?sslmode=disable' \
  go test -tags=live ./packages/shared/jobs/ -count=1 -v
```

Expected: PASS, all seven tests.

- [ ] **Step 6: Prove the tests discriminate**

```bash
# M1: drop SKIP LOCKED — concurrent leases can double-hand a job
sed -i 's/FOR UPDATE SKIP LOCKED/FOR UPDATE/' packages/shared/jobs/jobs.go
DATABASE_URL='...' go test -tags=live ./packages/shared/jobs/ -count=1 -run Lease
git checkout packages/shared/jobs/jobs.go

# M2: never reclaim an expired lease
sed -i "s/OR (status = 'leased' AND leased_until < now())//" packages/shared/jobs/jobs.go
DATABASE_URL='...' go test -tags=live ./packages/shared/jobs/ -count=1 # expect TestExpiredLeaseIsReclaimed
git checkout packages/shared/jobs/jobs.go

# M3: retry forever
sed -i "s/WHEN attempts >= \$3 THEN 'failed'/WHEN false THEN 'failed'/" packages/shared/jobs/jobs.go
DATABASE_URL='...' go test -tags=live ./packages/shared/jobs/ -count=1 # expect TestFailRetriesUntilTheCap
git checkout packages/shared/jobs/jobs.go
```

Note M1 may pass by luck — concurrency tests do. Run it three times; if it never fails, add a test that leases in two explicit transactions rather than relying on goroutine scheduling.

- [ ] **Step 7: Commit**

```bash
gofmt -l apps packages && go vet ./... && go test ./... -count=1
git add packages/shared/jobs packages/shared/store/migrations/0003_jobs.sql
git commit -m "feat: durable indexing job queue in Postgres

Enqueue dedupes on a partial unique index over active jobs, so two
gateways racing on the same submission produce one job. Lease uses
FOR UPDATE SKIP LOCKED so two indexers polling the same table never get
the same row, and reclaims leases that expired — a crashed worker's job
comes back instead of vanishing.

Tests are live against real Postgres because the behaviour under test is
the SQL. Three mutations confirmed they discriminate."
```

---

### Task 4: Submit and poll endpoints

**Files:**
- Create: `apps/gateway/internal/handler/handler.go`
- Modify: `apps/gateway/cmd/main.go` — mount the handler, build the policy and queue
- Test: `apps/gateway/internal/handler/handler_test.go`

**Interfaces:**
- Consumes: `admit.Policy`, `admit.Error`, `jobs.Queue`, `jobs.Job`, `jobs.ErrNotFound`.
- Produces:
  - `type Enqueuer interface { Enqueue(context.Context, string, string) (jobs.Job, error); Get(context.Context, string) (jobs.Job, error) }`
  - `type Handler struct { Policy admit.Policy; Jobs Enqueuer }`
  - `func Mount(e *echo.Echo, h *Handler, mw ...echo.MiddlewareFunc)`
  - Routes: `POST /api/repos`, `GET /api/jobs/:id`

- [ ] **Step 1: Write the failing test**

Create `apps/gateway/internal/handler/handler_test.go`:

```go
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/mralaminahamed/codetrail/packages/shared/admit"
	"github.com/mralaminahamed/codetrail/packages/shared/jobs"
)

type fakeQueue struct {
	enqueued []string
	job      jobs.Job
	err      error
}

func (f *fakeQueue) Enqueue(_ context.Context, remote, ref string) (jobs.Job, error) {
	if f.err != nil {
		return jobs.Job{}, f.err
	}
	f.enqueued = append(f.enqueued, remote+"@"+ref)
	return jobs.Job{ID: "job-1", Remote: remote, Ref: ref, Status: jobs.StatusPending}, nil
}

func (f *fakeQueue) Get(_ context.Context, id string) (jobs.Job, error) {
	if f.err != nil {
		return jobs.Job{}, f.err
	}
	if id != f.job.ID {
		return jobs.Job{}, jobs.ErrNotFound
	}
	return f.job, nil
}

func router(q *fakeQueue) *echo.Echo {
	e := echo.New()
	Mount(e, &Handler{Policy: admit.NewPolicy(admit.DefaultHosts), Jobs: q})
	return e
}

func do(e *echo.Echo, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func TestSubmitEnqueuesTheNormalisedRemote(t *testing.T) {
	q := &fakeQueue{}
	rec := do(router(q), http.MethodPost, "/api/repos", `{"remote":"https://GitHub.com/Owner/Repo.git"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", rec.Code, rec.Body)
	}
	// The queue must see the normalised form, or the dedupe index sees three
	// spellings of one repository as three repositories.
	if len(q.enqueued) != 1 || q.enqueued[0] != "https://github.com/Owner/Repo@HEAD" {
		t.Fatalf("enqueued %v", q.enqueued)
	}
	var got jobs.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != "job-1" || got.Status != jobs.StatusPending {
		t.Fatalf("body %+v", got)
	}
}

// A rejection has to say which rule fired. "Bad request" tells an operator
// nothing about what to change.
func TestSubmitRejectionNamesTheRule(t *testing.T) {
	for body, wantRule := range map[string]string{
		`{"remote":"file:///etc/passwd"}`:            "scheme",
		`{"remote":"https://169.254.169.254/a/b"}`:   "host",
		`{"remote":"https://github.com/onlyowner"}`:  "form",
	} {
		q := &fakeQueue{}
		rec := do(router(q), http.MethodPost, "/api/repos", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d", body, rec.Code)
		}
		var out struct {
			Error string `json:"error"`
			Rule  string `json:"rule"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.Rule != wantRule {
			t.Fatalf("%s: want rule %q, got %q", body, wantRule, out.Rule)
		}
		if len(q.enqueued) != 0 {
			t.Fatalf("%s: a rejected URL must not be enqueued", body)
		}
	}
}

func TestSubmitRequiresARemote(t *testing.T) {
	rec := do(router(&fakeQueue{}), http.MethodPost, "/api/repos", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
}

func TestGetJob(t *testing.T) {
	q := &fakeQueue{job: jobs.Job{ID: "job-1", Status: jobs.StatusDone}}
	rec := do(router(q), http.MethodGet, "/api/jobs/job-1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
}

func TestGetUnknownJobIs404(t *testing.T) {
	q := &fakeQueue{job: jobs.Job{ID: "other"}}
	rec := do(router(q), http.MethodGet, "/api/jobs/job-1", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
}

func TestQueueFailureIs500NotABadRequest(t *testing.T) {
	q := &fakeQueue{err: errors.New("postgres is down")}
	rec := do(router(q), http.MethodPost, "/api/repos", `{"remote":"https://github.com/a/b"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 — the caller's URL was fine — got %d", rec.Code)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./apps/gateway/internal/handler/ -count=1`
Expected: FAIL — `undefined: Mount`, `undefined: Handler`.

- [ ] **Step 3: Write the implementation**

Create `apps/gateway/internal/handler/handler.go`:

```go
// Package handler implements the gateway's REST API.
package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/mralaminahamed/codetrail/packages/shared/admit"
	"github.com/mralaminahamed/codetrail/packages/shared/jobs"
)

// Enqueuer is the queue surface the API needs. An interface so the handler
// tests run without a database.
type Enqueuer interface {
	Enqueue(ctx context.Context, remote, ref string) (jobs.Job, error)
	Get(ctx context.Context, id string) (jobs.Job, error)
}

type Handler struct {
	Policy admit.Policy
	Jobs   Enqueuer
}

func Mount(e *echo.Echo, h *Handler, mw ...echo.MiddlewareFunc) {
	g := e.Group("/api", mw...)
	g.POST("/repos", h.postRepo)
	g.GET("/jobs/:id", h.getJob)
}

type repoRequest struct {
	Remote string `json:"remote"`
	Ref    string `json:"ref"`
}

func (h *Handler) postRepo(c echo.Context) error {
	var req repoRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, echo.Map{"error": err.Error(), "rule": string(admit.RuleForm)})
	}
	remote, err := h.Policy.Check(req.Remote)
	if err != nil {
		var ae *admit.Error
		if errors.As(err, &ae) {
			// Name the rule: a generic 400 tells an operator nothing about
			// what to change.
			return c.JSON(http.StatusBadRequest, echo.Map{"error": ae.Detail, "rule": string(ae.Rule)})
		}
		return c.JSON(http.StatusBadRequest, echo.Map{"error": err.Error(), "rule": string(admit.RuleForm)})
	}
	ref := strings.TrimSpace(req.Ref)
	if ref == "" {
		ref = "HEAD"
	}
	// The normalised URL, not the caller's spelling: the dedupe index would
	// otherwise see three spellings of one repository as three repositories.
	job, err := h.Jobs.Enqueue(c.Request().Context(), remote.URL, ref)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, echo.Map{"error": err.Error()})
	}
	return c.JSON(http.StatusAccepted, job)
}

func (h *Handler) getJob(c echo.Context) error {
	job, err := h.Jobs.Get(c.Request().Context(), c.Param("id"))
	if errors.Is(err, jobs.ErrNotFound) {
		return c.JSON(http.StatusNotFound, echo.Map{"error": "no such job"})
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, echo.Map{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, job)
}
```

- [ ] **Step 4: Wire it into main**

Modify `apps/gateway/cmd/main.go`. Replace the `newRouter` function and its call site:

```go
// newRouter builds the router the binary actually serves. A function rather
// than inline in main so a test can pin the composition.
func newRouter(ready func() bool, h *handler.Handler) *echo.Echo {
	e := server.New(ready)
	handler.Mount(e, h)
	return e
}
```

and in `main`, after the store opens:

```go
	q := jobs.New(st.Pool())
	h := &handler.Handler{
		Policy: admit.NewPolicy(strings.Split(config.Get("ALLOWED_HOSTS", strings.Join(admit.DefaultHosts, ",")), ",")),
		Jobs:   q,
	}
	e := newRouter(readinessFor(log, st).Ready, h)
```

`apps/gateway/cmd/store.go` gains `Pool()` on the interface so `main` can build the queue from the
same connection:

```go
type storeHandle interface {
	Ping(context.Context) error
	Pool() *pgxpool.Pool
	Close()
}
```

with `"github.com/jackc/pgx/v5/pgxpool"` added to that file's imports. `*store.Store` already has
`Pool()`, so nothing else changes.

- [ ] **Step 5: Run the tests to verify they pass**

```bash
go build ./... && go test ./apps/... -count=1 -v
```

Expected: PASS, all six handler tests.

- [ ] **Step 6: Prove the tests discriminate**

```bash
# M1: enqueue the caller's raw spelling instead of the normalised URL
sed -i 's/h.Jobs.Enqueue(c.Request().Context(), remote.URL, ref)/h.Jobs.Enqueue(c.Request().Context(), req.Remote, ref)/' apps/gateway/internal/handler/handler.go
go test ./apps/gateway/internal/handler/ -count=1 # expect TestSubmitEnqueuesTheNormalisedRemote
git checkout apps/gateway/internal/handler/handler.go

# M2: answer a queue failure as a 400
sed -i 's/return c.JSON(http.StatusInternalServerError, echo.Map{"error": err.Error()})\n\t}\n\treturn c.JSON(http.StatusAccepted/return c.JSON(http.StatusBadRequest, echo.Map{"error": err.Error()})\n\t}\n\treturn c.JSON(http.StatusAccepted/' apps/gateway/internal/handler/handler.go
go test ./apps/gateway/internal/handler/ -count=1 # expect TestQueueFailureIs500NotABadRequest
git checkout apps/gateway/internal/handler/handler.go
```

- [ ] **Step 7: Commit**

```bash
gofmt -l apps packages && go vet ./... && go test ./... -count=1
git add apps/gateway
git commit -m "feat: submit a repository and poll its job

POST /api/repos validates through the admission policy and enqueues the
normalised URL — not the caller's spelling, or the dedupe index sees
three spellings of one repository as three. A rejection answers 400 and
names the rule that fired. A queue failure is a 500: the caller's URL was
fine, and telling them otherwise sends them to fix the wrong thing."
```

---

### Task 5: Sandboxed clone

**Files:**
- Create: `apps/indexer/internal/clone/clone.go`
- Test: `apps/indexer/internal/clone/clone_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type Limits struct { MaxBytes int64; Deadline time.Duration }`
  - `type Result struct { Dir, Commit string; Bytes int64 }`
  - `func Run(ctx context.Context, remote, ref, dir string, lim Limits) (Result, error)`
  - `var ErrTooLarge = errors.New("clone: repository exceeds the size cap")`

- [ ] **Step 1: Write the failing test**

Tests run against **local bare repositories created in the test**, not the network. `Run` takes the remote verbatim; admission (Task 2) is what restricts it to https in production, so a `file://` path here exercises the clone mechanics without a forge.

Create `apps/indexer/internal/clone/clone_test.go`:

```go
package clone

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// fixture builds a real git repository on disk and returns a file:// URL.
func fixture(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main", ".")
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("add", "-A")
	run("commit", "-qm", "fixture")
	return "file://" + dir
}

func TestCloneReturnsTheCommitAndContents(t *testing.T) {
	remote := fixture(t, map[string]string{"main.go": "package main\n"})
	dst := filepath.Join(t.TempDir(), "checkout")

	got, err := Run(context.Background(), remote, "HEAD", dst, Limits{MaxBytes: 1 << 20, Deadline: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Commit) != 40 {
		t.Fatalf("want a full commit sha, got %q", got.Commit)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "main.go")); err != nil || string(b) != "package main\n" {
		t.Fatalf("checkout is wrong: %v %q", err, b)
	}
}

// The cap has to be enforced, not merely declared.
func TestCloneRefusesAnOversizedRepository(t *testing.T) {
	big := make([]byte, 256*1024)
	for i := range big {
		big[i] = 'x'
	}
	remote := fixture(t, map[string]string{"big.txt": string(big)})
	dst := filepath.Join(t.TempDir(), "checkout")

	_, err := Run(context.Background(), remote, "HEAD", dst, Limits{MaxBytes: 64 * 1024, Deadline: time.Minute})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
	// And it must not leave the oversized checkout on disk.
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatal("an over-cap clone must be removed, not left on disk")
	}
}

// A hung clone must be killed by the deadline rather than held forever.
func TestCloneRespectsTheDeadline(t *testing.T) {
	remote := fixture(t, map[string]string{"main.go": "package main\n"})
	dst := filepath.Join(t.TempDir(), "checkout")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already dead

	if _, err := Run(ctx, remote, "HEAD", dst, Limits{MaxBytes: 1 << 20, Deadline: time.Minute}); err == nil {
		t.Fatal("want an error from a cancelled context")
	}
}

// A nonexistent remote must fail promptly, not block on a credential prompt.
func TestCloneFailsOnAnUnreachableRemote(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "checkout")
	done := make(chan error, 1)
	go func() {
		_, err := Run(context.Background(), "file:///nonexistent-"+t.Name(), "HEAD", dst,
			Limits{MaxBytes: 1 << 20, Deadline: 30 * time.Second})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want an error")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("clone hung — GIT_TERMINAL_PROMPT is probably not set")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./apps/indexer/internal/clone/ -count=1`
Expected: FAIL — `undefined: Run`.

- [ ] **Step 3: Write the implementation**

Create `apps/indexer/internal/clone/clone.go`:

```go
// Package clone fetches a repository into a scratch directory under caps.
//
// This is the only code in codetrail that runs a subprocess against an
// untrusted input, so every knob here is a containment decision rather than a
// performance one.
package clone

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var ErrTooLarge = errors.New("clone: repository exceeds the size cap")

type Limits struct {
	MaxBytes int64
	Deadline time.Duration
}

type Result struct {
	Dir    string
	Commit string
	Bytes  int64
}

// Run clones remote at ref into dir. On any error dir is removed, so a failed
// job leaves nothing behind for the disk quota to trip over later.
func Run(ctx context.Context, remote, ref, dir string, lim Limits) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, lim.Deadline)
	defer cancel()

	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return Result{}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	// --depth 1 keeps history out of the fetch and --single-branch keeps every
	// other ref out. --filter=blob:none does NOT keep blobs out of a clone that
	// checks out: the checkout refetches them all, and the filtered .git ends up
	// marginally larger (measured: 29712 vs 29560 bytes). It is kept only for
	// the partial-clone promisor metadata, not as a size control.
	args := []string{
		"clone", "--quiet", "--depth", "1", "--single-branch",
		"--filter=blob:none", "--no-tags",
	}
	if ref != "" && ref != "HEAD" {
		args = append(args, "--branch", ref)
	}
	args = append(args, remote, dir)

	cmd := exec.CommandContext(ctx, "git", args...)
	// A private or mistyped URL must fail rather than block forever waiting
	// for a credential nobody is there to type.
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/false",
		"GIT_CONFIG_NOSYSTEM=1",
		"HOME="+filepath.Dir(dir),
	)
	// Its own process group, so the deadline kills git's children too — a
	// killed parent otherwise leaves a fetch running against the cap.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }

	if out, err := cmd.CombinedOutput(); err != nil {
		cleanup()
		return Result{}, fmt.Errorf("clone: %w: %s", err, strings.TrimSpace(string(out)))
	}

	size, err := dirSize(dir)
	if err != nil {
		cleanup()
		return Result{}, err
	}
	if lim.MaxBytes > 0 && size > lim.MaxBytes {
		cleanup()
		return Result{}, fmt.Errorf("%w: %d bytes > %d", ErrTooLarge, size, lim.MaxBytes)
	}

	sha, err := head(ctx, dir)
	if err != nil {
		cleanup()
		return Result{}, err
	}
	return Result{Dir: dir, Commit: sha, Bytes: size}, nil
}

func head(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("rev-parse: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// dirSize counts regular files only. Symlinks are not followed here for the
// same reason the walker does not follow them: a link out of the tree would
// otherwise be measured, and then read.
func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./apps/indexer/internal/clone/ -count=1 -v`
Expected: PASS, all four tests.

- [ ] **Step 5: Prove the tests discriminate**

```bash
# M1: cap not enforced
sed -i 's/if lim.MaxBytes > 0 \&\& size > lim.MaxBytes {/if false {/' apps/indexer/internal/clone/clone.go
go test ./apps/indexer/internal/clone/ -count=1 # expect TestCloneRefusesAnOversizedRepository
git checkout apps/indexer/internal/clone/clone.go

# M2: an over-cap clone is left on disk
sed -i 's/cleanup()\n\t\treturn Result{}, fmt.Errorf("%w: %d bytes/return Result{}, fmt.Errorf("%w: %d bytes/' apps/indexer/internal/clone/clone.go
go test ./apps/indexer/internal/clone/ -count=1 # expect the os.Stat assertion
git checkout apps/indexer/internal/clone/clone.go
```

- [ ] **Step 6: Commit**

```bash
gofmt -l apps packages && go vet ./... && go test ./... -count=1
git add apps/indexer/internal/clone
git commit -m "feat: sandboxed clone with enforced caps

Shallow, single-branch, blob-filtered, in its own process group so the
deadline kills git's children rather than orphaning a fetch that keeps
running against the cap. GIT_TERMINAL_PROMPT=0 and a false GIT_ASKPASS
so a private URL fails instead of blocking forever on a prompt.

The size cap is checked after the fetch and the checkout is removed when
it trips — a cap that leaves the oversized tree on disk has not enforced
anything. Tests clone real local git fixtures, not the network."
```

---

### Task 6: Walker that refuses to follow links

**Files:**
- Create: `apps/indexer/internal/walk/walk.go`
- Test: `apps/indexer/internal/walk/walk_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type Limits struct { MaxFiles int; MaxFileBytes int64 }`
  - `type File struct { Path string; Bytes int64; Lines int; Lang string }`
  - `func Files(root string, lim Limits) ([]File, error)`
  - `var ErrTooManyFiles = errors.New("walk: file count exceeds the cap")`

- [ ] **Step 1: Write the failing test**

Create `apps/indexer/internal/walk/walk_test.go`:

```go
package walk

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func paths(fs []File) map[string]bool {
	m := map[string]bool{}
	for _, f := range fs {
		m[f.Path] = true
	}
	return m
}

func TestListsRegularFilesWithRelativePaths(t *testing.T) {
	root := tree(t, map[string]string{
		"main.go":          "package main\n\nfunc main() {}\n",
		"internal/a/b.go":  "package a\n",
		"README.md":        "# hi\n",
	})
	got, err := Files(root, Limits{MaxFiles: 100, MaxFileBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	p := paths(got)
	for _, want := range []string{"main.go", "internal/a/b.go", "README.md"} {
		if !p[want] {
			t.Fatalf("missing %q, got %v", want, p)
		}
	}
	for _, f := range got {
		if filepath.IsAbs(f.Path) {
			t.Fatalf("paths must be repo-relative, got %q", f.Path)
		}
	}
}

// THE security test. A repository can contain `link -> /etc/passwd`; a walker
// that follows it reads and indexes the host's files.
//
// The file link is what discriminates: against a walker with no guard it is
// read and indexed with its real contents. The directory link is a second
// case, and it must be asserted SEPARATELY — a walk that follows it fails with
// EISDIR and aborts before this test's own assertions run, so a combined
// fixture kills the mutant on an unrelated error and reports coverage it does
// not have.
func TestNeverFollowsSymlinks(t *testing.T) {
	root := tree(t, map[string]string{"main.go": "package main\n"})
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape.txt")); err != nil {
		t.Skip("symlinks unavailable on this platform")
	}
	if err := os.Symlink("/etc", filepath.Join(root, "etc")); err != nil {
		t.Fatal(err)
	}

	got, err := Files(root, Limits{MaxFiles: 100, MaxFileBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	p := paths(got)
	if p["escape.txt"] || p["etc"] {
		t.Fatalf("a symlink was walked: %v", p)
	}
	for _, f := range got {
		if f.Path != "main.go" {
			t.Fatalf("unexpected entry %q — only regular files may be listed", f.Path)
		}
	}
}

// .git holds the object database; indexing it is pointless and enormous.
func TestSkipsDotGit(t *testing.T) {
	root := tree(t, map[string]string{
		"main.go":              "package main\n",
		".git/config":          "[core]\n",
		".git/objects/aa/bbbb": "binary",
	})
	got, err := Files(root, Limits{MaxFiles: 100, MaxFileBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range got {
		if len(f.Path) >= 4 && f.Path[:4] == ".git" {
			t.Fatalf("walked into .git: %q", f.Path)
		}
	}
}

func TestCountsLinesAndDetectsLanguage(t *testing.T) {
	root := tree(t, map[string]string{
		"a.go":   "package a\nfunc F() {}\n",
		"b.md":   "# t\n",
		"c.bin":  "\x00\x01\x02",
	})
	got, err := Files(root, Limits{MaxFiles: 100, MaxFileBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]File{}
	for _, f := range got {
		by[f.Path] = f
	}
	if by["a.go"].Lang != "go" {
		t.Fatalf("want lang go, got %q", by["a.go"].Lang)
	}
	if by["a.go"].Lines != 2 {
		t.Fatalf("want 2 lines, got %d", by["a.go"].Lines)
	}
	if by["b.md"].Lang != "markdown" {
		t.Fatalf("want lang markdown, got %q", by["b.md"].Lang)
	}
}

// A file over the cap is skipped, not fatal: one enormous generated file
// should not lose the repository.
func TestSkipsOversizedFiles(t *testing.T) {
	big := make([]byte, 4096)
	root := tree(t, map[string]string{"small.go": "package a\n", "huge.go": string(big)})
	got, err := Files(root, Limits{MaxFiles: 100, MaxFileBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	p := paths(got)
	if p["huge.go"] {
		t.Fatal("an over-cap file must be skipped")
	}
	if !p["small.go"] {
		t.Fatal("an over-cap file must not lose the rest of the repository")
	}
}

// Too many files IS fatal: it means the caps were wrong for this repository,
// and half an index is worse than none.
func TestRefusesTooManyFiles(t *testing.T) {
	files := map[string]string{}
	for i := range 20 {
		files[string(rune('a'+i))+".go"] = "package a\n"
	}
	_, err := Files(tree(t, files), Limits{MaxFiles: 5, MaxFileBytes: 1 << 20})
	if !errors.Is(err, ErrTooManyFiles) {
		t.Fatalf("want ErrTooManyFiles, got %v", err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./apps/indexer/internal/walk/ -count=1`
Expected: FAIL — `undefined: Files`.

- [ ] **Step 3: Write the implementation**

Create `apps/indexer/internal/walk/walk.go`:

```go
// Package walk lists the indexable regular files of a checkout.
package walk

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

var ErrTooManyFiles = errors.New("walk: file count exceeds the cap")

type Limits struct {
	MaxFiles     int
	MaxFileBytes int64
}

type File struct {
	Path  string
	Bytes int64
	Lines int
	Lang  string
}

var langByExt = map[string]string{
	".go": "go", ".md": "markdown", ".sql": "sql",
	".ts": "typescript", ".tsx": "typescript", ".js": "javascript",
	".yml": "yaml", ".yaml": "yaml", ".json": "json",
}

// Files walks root and returns its regular files, repo-relative.
//
// It uses WalkDir, whose DirEntry comes from ReadDir and therefore describes
// the link itself rather than its target — the same guarantee as Lstat. A
// repository can contain `link -> /etc/passwd`, and following it would read
// and index the host's files.
func Files(root string, lim Limits) ([]File, error) {
	var out []File
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			// The object database is enormous and meaningless to index.
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		// Not IsDir and not regular means a symlink, socket, device or fifo.
		// None of them is a file this product has any business reading.
		if !d.Type().IsRegular() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		if lim.MaxFileBytes > 0 && info.Size() > lim.MaxFileBytes {
			return nil
		}
		if lim.MaxFiles > 0 && len(out) >= lim.MaxFiles {
			return fmt.Errorf("%w: more than %d", ErrTooManyFiles, lim.MaxFiles)
		}
		body, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		out = append(out, File{
			Path:  filepath.ToSlash(rel),
			Bytes: info.Size(),
			Lines: bytes.Count(body, []byte{'\n'}),
			Lang:  langByExt[strings.ToLower(filepath.Ext(rel))],
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./apps/indexer/internal/walk/ -count=1 -v`
Expected: PASS, all six tests.

- [ ] **Step 5: Prove the symlink test discriminates**

This is the one that matters most.

```bash
# M1: follow links — the classic mistake
sed -i 's/if !d.Type().IsRegular() {/if false {/' apps/indexer/internal/walk/walk.go
go test ./apps/indexer/internal/walk/ -count=1 # MUST fail TestNeverFollowsSymlinks
git checkout apps/indexer/internal/walk/walk.go

# M2: do not skip .git
sed -i 's/if d.Name() == ".git" {/if false {/' apps/indexer/internal/walk/walk.go
go test ./apps/indexer/internal/walk/ -count=1 # expect TestSkipsDotGit
git checkout apps/indexer/internal/walk/walk.go

# M3: file cap not enforced
sed -i 's/if lim.MaxFiles > 0 \&\& len(out) >= lim.MaxFiles {/if false {/' apps/indexer/internal/walk/walk.go
go test ./apps/indexer/internal/walk/ -count=1 # expect TestRefusesTooManyFiles
git checkout apps/indexer/internal/walk/walk.go
```

If M1 does not fail the symlink test, **stop**: the test is not testing what it claims and the sandbox has no coverage.

- [ ] **Step 6: Commit**

```bash
gofmt -l apps packages && go vet ./... && go test ./... -count=1
git add apps/indexer/internal/walk
git commit -m "feat: walker that never follows a symlink

A repository can contain link -> /etc/passwd, and a walker that follows
it reads and indexes the host's files. WalkDir's DirEntry describes the
link rather than its target, and anything that is not a regular file is
skipped outright.

Oversized files are skipped so one generated blob does not lose the
repository; too many files is fatal, because half an index is worse than
none. A mutation that follows links fails the symlink test."
```

---

### Task 7: The indexer binary

**Files:**
- Create: `apps/indexer/cmd/main.go`
- Create: `packages/shared/store/repos.go`
- Test: `packages/shared/store/repos_live_test.go`

**Interfaces:**
- Consumes: `jobs.Queue`, `clone.Run`, `walk.Files`, `store.New`.
- Produces:
  - `func (s *Store) PutRepo(ctx context.Context, r models.Repo, files []models.File) error`
  - `func (s *Store) GetRepo(ctx context.Context, id string) (models.Repo, error)`
  - `func (s *Store) TouchRepo(ctx context.Context, id string) error`
  - `func RepoID(remote, commit string) string`
  - `func FileID(repoID, path string) string`

- [ ] **Step 1: Write the failing test**

Create `packages/shared/store/repos_live_test.go`:

```go
//go:build live

package store

import (
	"context"
	"testing"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

func TestPutRepoIsIdempotentLive(t *testing.T) {
	ctx := context.Background()
	s, err := New(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const remote, commit = "https://github.com/a/idem", "cafe1234cafe1234cafe1234cafe1234cafe1234"
	id := RepoID(remote, commit)
	r := models.Repo{ID: id, Remote: remote, Ref: "main", Commit: commit}
	files := []models.File{
		{ID: FileID(id, "main.go"), RepoID: id, Path: "main.go", Blob: "b1", Lang: "go", Lines: 3},
		{ID: FileID(id, "a/b.go"), RepoID: id, Path: "a/b.go", Blob: "b2", Lang: "go", Lines: 1},
	}
	t.Cleanup(func() { s.pool.Exec(ctx, `DELETE FROM repos WHERE id = $1`, id) })

	// Twice: a retried job after a crash must converge, not duplicate.
	for i := range 2 {
		if err := s.PutRepo(ctx, r, files); err != nil {
			t.Fatalf("write %d: %v", i+1, err)
		}
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM files WHERE repo_id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("want 2 files after two identical writes, got %d", n)
	}
}

// Deleting a repo must take its files with it — eviction is one DELETE, and
// an orphan sweep is a second system to disagree with the first.
func TestDeletingARepoCascadesLive(t *testing.T) {
	ctx := context.Background()
	s, err := New(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const remote, commit = "https://github.com/a/casc", "beef1234beef1234beef1234beef1234beef1234"
	id := RepoID(remote, commit)
	r := models.Repo{ID: id, Remote: remote, Ref: "main", Commit: commit}
	if err := s.PutRepo(ctx, r, []models.File{
		{ID: FileID(id, "x.go"), RepoID: id, Path: "x.go", Blob: "b", Lang: "go", Lines: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM repos WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM files WHERE repo_id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("want the files cascaded away, %d remain", n)
	}
}

// IDs are content-addressed, which is what makes the write above idempotent.
func TestIDsAreDeterministicAndDistinct(t *testing.T) {
	a := RepoID("https://github.com/a/b", "sha1")
	if a != RepoID("https://github.com/a/b", "sha1") {
		t.Fatal("RepoID is not deterministic")
	}
	if a == RepoID("https://github.com/a/b", "sha2") {
		t.Fatal("a different commit must be a different repo row")
	}
	if a == RepoID("https://github.com/a/c", "sha1") {
		t.Fatal("a different remote must be a different repo row")
	}
	if FileID(a, "x.go") == FileID(a, "y.go") {
		t.Fatal("different paths must be different files")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `DATABASE_URL='...' go test -tags=live ./packages/shared/store/ -count=1`
Expected: FAIL — `undefined: RepoID`.

- [ ] **Step 3: Write the store methods**

Create `packages/shared/store/repos.go`:

```go
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

var ErrNotFound = errors.New("store: not found")

// RepoID and FileID are content-addressed, which is what makes a retried
// index converge on the same rows instead of duplicating them.
func RepoID(remote, commit string) string { return hash(remote, commit) }
func FileID(repoID, path string) string   { return hash(repoID, path) }

func hash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// PutRepo writes a repo and its files in one transaction. Upserts throughout,
// so a job retried after a crash converges rather than failing on a conflict.
func (s *Store) PutRepo(ctx context.Context, r models.Repo, files []models.File) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
		INSERT INTO repos (id, remote, ref, commit_sha, file_count)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (id) DO UPDATE SET
			ref = EXCLUDED.ref, file_count = EXCLUDED.file_count, indexed_at = now()`,
		r.ID, r.Remote, r.Ref, r.Commit, len(files)); err != nil {
		return fmt.Errorf("repo: %w", err)
	}
	for _, f := range files {
		if _, err := tx.Exec(ctx, `
			INSERT INTO files (id, repo_id, path, blob, lang, lines)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (id) DO UPDATE SET
				blob = EXCLUDED.blob, lang = EXCLUDED.lang, lines = EXCLUDED.lines`,
			f.ID, f.RepoID, f.Path, f.Blob, f.Lang, f.Lines); err != nil {
			return fmt.Errorf("file %s: %w", f.Path, err)
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) GetRepo(ctx context.Context, id string) (models.Repo, error) {
	var r models.Repo
	err := s.pool.QueryRow(ctx,
		`SELECT id, remote, ref, commit_sha, indexed_at FROM repos WHERE id = $1`, id).
		Scan(&r.ID, &r.Remote, &r.Ref, &r.Commit, &r.IndexedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return models.Repo{}, ErrNotFound
	}
	return r, err
}

// TouchRepo records a query against a repo. This is the LRU clock: eviction
// reads exactly this column.
func (s *Store) TouchRepo(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE repos SET last_queried_at = now() WHERE id = $1`, id)
	return err
}
```

Also extend `0001_init.sql`? **No** — it has already run on real databases. Add `packages/shared/store/migrations/0004_repo_lru.sql`:

```sql
ALTER TABLE repos ADD COLUMN IF NOT EXISTS last_queried_at TIMESTAMPTZ NOT NULL DEFAULT now();
ALTER TABLE repos ADD COLUMN IF NOT EXISTS size_bytes BIGINT NOT NULL DEFAULT 0;
ALTER TABLE repos ADD COLUMN IF NOT EXISTS file_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE repos ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'ready';
CREATE INDEX IF NOT EXISTS repos_lru_idx ON repos (last_queried_at);
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `DATABASE_URL='...' go test -tags=live ./packages/shared/store/ -count=1 -v`
Expected: PASS.

- [ ] **Step 5: Write the indexer binary**

Create `apps/indexer/cmd/main.go`:

```go
// Command indexer leases indexing jobs and runs them.
//
// It is the only component that handles an untrusted URL, so it is a separate
// binary: the sandbox is then a deployment boundary rather than a promise.
package main

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/mralaminahamed/codetrail/apps/indexer/internal/clone"
	"github.com/mralaminahamed/codetrail/apps/indexer/internal/walk"
	"github.com/mralaminahamed/codetrail/packages/shared/config"
	"github.com/mralaminahamed/codetrail/packages/shared/jobs"
	"github.com/mralaminahamed/codetrail/packages/shared/logger"
	"github.com/mralaminahamed/codetrail/packages/shared/models"
	"github.com/mralaminahamed/codetrail/packages/shared/store"
)

type limits struct {
	clone clone.Limits
	walk  walk.Limits
	tries int
}

func main() {
	log := logger.New("indexer")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.New(ctx, config.Get("DATABASE_URL", "postgres://codetrail:codetrail@localhost:55432/codetrail?sslmode=disable"))
	if err != nil {
		log.Fatal().Err(err).Msg("connect postgres")
	}
	defer st.Close()

	q := jobs.New(st.Pool())
	lim := limits{
		clone: clone.Limits{
			MaxBytes: int64(config.GetInt("MAX_REPO_BYTES", 256<<20)),
			Deadline: time.Duration(config.GetInt("JOB_DEADLINE_SECONDS", 600)) * time.Second,
		},
		walk: walk.Limits{
			MaxFiles:     config.GetInt("MAX_REPO_FILES", 20000),
			MaxFileBytes: int64(config.GetInt("MAX_FILE_BYTES", 1<<20)),
		},
		tries: config.GetInt("MAX_ATTEMPTS", 3),
	}
	worker := config.Get("HOSTNAME", "indexer")
	scratch := config.Get("SCRATCH_DIR", filepath.Join(os.TempDir(), "codetrail"))
	idle := time.Duration(config.GetInt("POLL_SECONDS", 2)) * time.Second

	log.Info().Str("worker", worker).Msg("indexer up")
	for ctx.Err() == nil {
		job, ok, err := q.Lease(ctx, worker, lim.clone.Deadline+time.Minute)
		if err != nil {
			log.Error().Err(err).Msg("lease")
			sleep(ctx, idle)
			continue
		}
		if !ok {
			sleep(ctx, idle)
			continue
		}
		runJob(ctx, log, st, q, job, worker, lim, scratch)
	}
}

// fail records a job failure, logging when the lease was already lost — the
// four call sites discarded this error before Complete and Fail could report it.
func fail(ctx context.Context, l zerolog.Logger, q *jobs.Queue, id, worker, reason string, tries int) {
	if err := q.Fail(ctx, id, worker, reason, tries); err != nil {
		l.Warn().Err(err).Msg("could not record failure: lease no longer held")
	}
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func runJob(ctx context.Context, log zerolog.Logger, st *store.Store, q *jobs.Queue,
	job jobs.Job, worker string, lim limits, scratch string) {

	l := log.With().Str("job", job.ID).Str("remote", job.Remote).Logger()
	dir := filepath.Join(scratch, job.ID)
	// The scratch tree goes whether this succeeds or fails: a failed job that
	// leaves its checkout behind fills the disk one failure at a time.
	defer os.RemoveAll(dir)

	res, err := clone.Run(ctx, job.Remote, job.Ref, dir, lim.clone)
	if err != nil {
		l.Warn().Err(err).Msg("clone failed")
		fail(ctx, l, q, job.ID, worker, err.Error(), lim.tries)
		return
	}
	files, err := walk.Files(res.Dir, lim.walk)
	if err != nil {
		l.Warn().Err(err).Msg("walk failed")
		fail(ctx, l, q, job.ID, worker, err.Error(), lim.tries)
		return
	}

	repoID := store.RepoID(job.Remote, res.Commit)
	rows := make([]models.File, 0, len(files))
	for _, f := range files {
		rows = append(rows, models.File{
			ID: store.FileID(repoID, f.Path), RepoID: repoID,
			Path: f.Path, Blob: "", Lang: f.Lang, Lines: f.Lines,
		})
	}
	repo := models.Repo{ID: repoID, Remote: job.Remote, Ref: job.Ref, Commit: res.Commit}
	if err := st.PutRepo(ctx, repo, rows); err != nil {
		l.Error().Err(err).Msg("write failed")
		fail(ctx, l, q, job.ID, worker, err.Error(), lim.tries)
		return
	}
	if err := q.Complete(ctx, job.ID, worker); err != nil {
		// ErrNotLeased here means this worker's lease expired and another
		// indexer took the job. Losing the race is normal; completing someone
		// else's job would not be.
		l.Warn().Err(err).Msg("could not complete: lease no longer held")
		return
	}
	l.Info().Str("commit", res.Commit).Int("files", len(rows)).Int64("bytes", res.Bytes).Msg("indexed")
}
```

- [ ] **Step 6: Verify end to end against a real repository**

```bash
make up
go build -o bin/indexer ./apps/indexer/cmd
go build -o bin/gateway ./apps/gateway/cmd
DATABASE_URL='postgres://codetrail:codetrail@localhost:55432/codetrail?sslmode=disable' ./bin/gateway &
DATABASE_URL='postgres://codetrail:codetrail@localhost:55432/codetrail?sslmode=disable' ./bin/indexer &

JOB=$(curl -sX POST localhost:8080/api/repos -H 'content-type: application/json' \
  -d '{"remote":"https://github.com/mralaminahamed/codetrail"}' | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')
sleep 30
curl -s localhost:8080/api/jobs/$JOB
make psql <<< 'SELECT remote, commit_sha, file_count FROM repos;'
```

Expected: the job reaches `done`, and `repos` holds one row with a real commit SHA and a non-zero file count.

- [ ] **Step 7: Commit**

```bash
gofmt -l apps packages && go vet ./... && go test ./... -count=1
git add apps/indexer packages/shared/store
git commit -m "feat: indexer worker

Leases a job, clones under caps, walks skipping every non-regular file,
writes the repo and its files in one transaction, and removes the
scratch tree whether it succeeded or failed — a failed job that leaves
its checkout behind fills the disk one failure at a time.

Repo and file IDs are content-addressed, so a job retried after a crash
converges on the same rows rather than duplicating them. That is tested
by writing the same repo twice and counting."
```

---

### Task 8: LRU eviction

**Files:**
- Create: `packages/shared/store/evict.go`
- Modify: `apps/indexer/cmd/main.go` — evict after each successful index
- Test: `packages/shared/store/evict_live_test.go`

**Note on `410 Gone`.** The global constraints require an evicted repository to
answer `410`, not `404` (spec §10). P1 exposes no endpoint that reads a
repository — only `POST /api/repos` and `GET /api/jobs/:id` — so there is
nothing here to return it from, and adding a `410` with no reader would be
untestable ceremony. It lands in **P3**, with the first endpoint that serves a
repository, where `store.GetRepo` returning `ErrNotFound` for an id that once
existed becomes distinguishable from one that never did.

**Interfaces:**
- Consumes: `store.PutRepo`, `store.TouchRepo`.
- Produces:
  - `func (s *Store) Evict(ctx context.Context, keep int) (int, error)`
  - `func (s *Store) CountRepos(ctx context.Context) (int, error)`

- [ ] **Step 1: Write the failing test**

Create `packages/shared/store/evict_live_test.go`:

```go
//go:build live

package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

func seed(t *testing.T, s *Store, name string) string {
	t.Helper()
	ctx := context.Background()
	remote := "https://github.com/evict/" + name
	commit := fmt.Sprintf("%040d", len(name))
	id := RepoID(remote, commit)
	if err := s.PutRepo(ctx, models.Repo{ID: id, Remote: remote, Ref: "main", Commit: commit},
		[]models.File{{ID: FileID(id, "a.go"), RepoID: id, Path: "a.go", Lang: "go", Lines: 1}}); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestEvictKeepsTheMostRecentlyQueriedLive(t *testing.T) {
	ctx := context.Background()
	s, err := New(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.pool.Exec(ctx, `DELETE FROM repos`); err != nil {
		t.Fatal(err)
	}

	oldest := seed(t, s, "a")
	middle := seed(t, s, "bb")
	newest := seed(t, s, "ccc")

	// Touch in order, so last_queried_at ranks them.
	for _, id := range []string{oldest, middle, newest} {
		if err := s.TouchRepo(ctx, id); err != nil {
			t.Fatal(err)
		}
	}

	n, err := s.Evict(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("want 1 evicted, got %d", n)
	}
	if _, err := s.GetRepo(ctx, oldest); err == nil {
		t.Fatal("the least recently queried repo should be gone")
	}
	for _, id := range []string{middle, newest} {
		if _, err := s.GetRepo(ctx, id); err != nil {
			t.Fatalf("%s should have been kept: %v", id, err)
		}
	}
}

// Eviction must take the files with it, or the disk never actually frees.
func TestEvictCascadesLive(t *testing.T) {
	ctx := context.Background()
	s, err := New(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.pool.Exec(ctx, `DELETE FROM repos`); err != nil {
		t.Fatal(err)
	}

	doomed := seed(t, s, "d")
	if _, err := s.Evict(ctx, 0); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM files WHERE repo_id = $1`, doomed).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("want the files gone, %d remain", n)
	}
}

func TestEvictIsANoOpUnderTheLimitLive(t *testing.T) {
	ctx := context.Background()
	s, err := New(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.pool.Exec(ctx, `DELETE FROM repos`); err != nil {
		t.Fatal(err)
	}
	seed(t, s, "only")
	n, err := s.Evict(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("want nothing evicted, got %d", n)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `DATABASE_URL='...' go test -tags=live ./packages/shared/store/ -count=1 -run Evict`
Expected: FAIL — `undefined: Evict`.

- [ ] **Step 3: Write the implementation**

Create `packages/shared/store/evict.go`:

```go
package store

import "context"

// Evict deletes every repo beyond the keep most recently queried, and returns
// how many went. One DELETE, cascading to files, spans, symbols and edges —
// an orphan sweep would be a second system to disagree with this one.
func (s *Store) Evict(ctx context.Context, keep int) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM repos WHERE id IN (
			SELECT id FROM repos
			ORDER BY last_queried_at DESC
			OFFSET $1
		)`, keep)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

func (s *Store) CountRepos(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM repos`).Scan(&n)
	return n, err
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `DATABASE_URL='...' go test -tags=live ./packages/shared/store/ -count=1 -run Evict -v`
Expected: PASS, all three.

- [ ] **Step 5: Call it from the indexer**

In `apps/indexer/cmd/main.go`, at the end of `runJob`, after `q.Complete` succeeds:

```go
	// Evict here rather than on a timer: the only thing that grows the corpus
	// is a successful index, so this is exactly when the quota can be exceeded.
	if n, err := st.Evict(ctx, lim.keepRepos); err != nil {
		l.Warn().Err(err).Msg("evict failed")
	} else if n > 0 {
		l.Info().Int("evicted", n).Msg("evicted least recently queried repos")
	}
```

Add `keepRepos int` to the `limits` struct and populate it in `main`:

```go
		keepRepos: config.GetInt("KEEP_REPOS", 50),
```

- [ ] **Step 6: Prove the tests discriminate**

```bash
# M1: evict newest-first instead of oldest-first
sed -i 's/ORDER BY last_queried_at DESC/ORDER BY last_queried_at ASC/' packages/shared/store/evict.go
DATABASE_URL='...' go test -tags=live ./packages/shared/store/ -count=1 -run Evict # expect the LRU test
git checkout packages/shared/store/evict.go

# M2: eviction is a no-op
sed -i 's/OFFSET \$1/OFFSET 1000000/' packages/shared/store/evict.go
DATABASE_URL='...' go test -tags=live ./packages/shared/store/ -count=1 -run Evict
git checkout packages/shared/store/evict.go
```

- [ ] **Step 7: Commit**

```bash
gofmt -l apps packages && go vet ./... && go test ./... -count=1
git add packages/shared/store apps/indexer
git commit -m "feat: LRU eviction of indexed repositories

Anyone may submit a repository, so something has to bound the corpus.
Eviction keeps the N most recently queried and drops the rest in one
DELETE that cascades — an orphan sweep would be a second system to
disagree with the first.

It runs after a successful index rather than on a timer: a successful
index is the only thing that can push the corpus over its quota."
```

---

## Definition of done for P1

- [ ] CI green on `trunk`, including the live Postgres step.
- [ ] `POST /api/repos` accepts an allowlisted https URL and rejects every case in Task 2's tests, naming the rule.
- [ ] `GET /api/jobs/:id` reports a job through `pending → leased → done`.
- [ ] A real public repository indexes end to end: `repos` and `files` rows exist with a real commit SHA.
- [ ] A repository containing an escaping symlink indexes **without reading the link's target**, proven by a test that fails when the guard is removed.
- [ ] Re-running the same job writes the same rows — no duplicates.
- [ ] Eviction drops the least recently queried repo and its files.
- [ ] Every mutation listed in the tasks above kills at least one test.

**Not in P1, deliberately:** no chunking, no embeddings, no spans, no `blob` hashes on files (the column exists and is written empty — P2 fills it when it needs content addressing for incremental re-index). No metrics beyond `codetrail_ready`; P1 has no traffic worth alerting on yet.

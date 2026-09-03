# apps/console

The browser half of codetrail: submit a repository, watch it index, ask a
question, jump to source, and browse the symbol graph.

React 19 + TypeScript (`strict`, `noUncheckedIndexedAccess`) + Tailwind v4,
built by Vite into a static bundle.

## Running it

```bash
npm ci
npm run dev        # localhost:5173, proxying /api to localhost:8080
npm run build      # tsc --noEmit && vite build  ->  dist/
npm run preview    # serves dist/ with the same proxy
npm run test -- --run
npm run lint && npm run typecheck
```

It needs a gateway on `localhost:8080`. From the repository root:

```bash
make up                      # Postgres
DATABASE_URL=... EMBED_PROVIDER=fake ./bin/gateway
DATABASE_URL=... EMBED_PROVIDER=fake ./bin/indexer
```

`vite.config.ts` proxies `/api`, `/health`, `/ready` and `/metrics` in **both**
`server` and `preview`, so the browser only ever issues same-origin requests in
development and against the production bundle. The API base is the relative
`/api` and there is no `VITE_API_URL`: a base URL compiled into the bundle is a
bundle per environment and a CORS requirement smuggled in through configuration.

## The fixtures

`src/api/fixtures/*.json` are **emitted from the shipped gateway handlers** by
`apps/gateway/internal/handler/fixtures_live_test.go` (`//go:build live`) over a
real Postgres, and compared on every later run, so a response-shape change fails
CI rather than being discovered by a console rendering the old one.

```bash
# from the repository root, with Postgres up
DATABASE_URL='postgres://codetrail:codetrail@localhost:55432/codetrail?sslmode=disable' \
  UPDATE_CONSOLE_FIXTURES=1 go test -tags=live -run TestConsoleFixtures ./apps/gateway/internal/handler/
```

`src/api/fixtures/NOTES.md` records, per file, how far the shipped stack carried
it — because a fixture the stack cannot produce is a fixture the guard cannot
guard, and three of them are in that position for reasons worth reading.

## What it does not do

- **It does not say why a job failed.** The API does not carry a reason: the
  gateway withholds the indexer's stderr on purpose, because it has carried
  filesystem paths and credential-bearing URLs. The console says that, in those
  words.
- **It does not check whether a ref has moved.** Nothing does. It renders the
  server's staleness sentence verbatim and composes none of its own.
- **It cannot link to a forge whose URL shape codetrail does not know.** It
  renders the tuple, a runnable `git show … | sha256sum` check and the digest
  that command should print, and says why there is no link.
- **It draws no scale from the score floor.** The floor is −1 and uncalibrated
  until P6, and a bar drawn from it would render a guess as a measurement.
- **It has no deployment.** Static bundle, same-origin by construction; P8 puts
  one origin in front of the console and the gateway.
- **No test here hits a live gateway.** MSW intercepts everything. The one real
  end-to-end run is recorded in the root README.

## Accessibility

`jest-axe` runs over every view in the suite and is a **floor, not the claim**:
axe-core's `color-contrast` rule cannot run under jsdom at all, so contrast is
zero-covered by the automated suite and is checked by hand.

The manual sweep — keyboard, screen reader, reduced motion, 320px reflow,
contrast — is in `docs/a11y-sweep-2026-09-03.md`, with one finding fixed and one
pass recorded as not performed.

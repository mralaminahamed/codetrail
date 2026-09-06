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

## The design

A developer tool, so: quiet, dense, and readable at 320px. The decisions are in
`src/index.css` beside the rules that carry them, but the short version:

- **A stylesheet exists because Tailwind's preflight is a destructive reset.**
  `margin: 0` and `border: 0` on everything, headings inheriting size and
  weight, links inheriting colour with no underline, lists losing their
  markers. Importing it and opting nothing back in — which is what this console
  did — is worse than shipping no CSS at all. Every rule in `index.css` is an
  opt-back-in, and the ones that close a WCAG failure name its number.
- **Element selectors, not utilities.** The markup was already semantic — `dl`,
  `ol`, `section`, `pre`, `code`, `label`, `button` — so the stylesheet keys off
  elements, and a component carries a class only where the markup genuinely
  cannot say which of two panels it is. **No test asserts a class name**, which
  is what makes the design safe to change.
- **Type**: a 1.25 scale from a 16px base, stopped at four sizes, because a
  console with six type sizes is a console with six kinds of importance and this
  one has four. Spacing is a 4px rhythm.
- **Colour**: sRGB hex — not oklch, which a naive contrast parser reads as a
  ratio of 1.0 and did, once. It carries `assets/icon.svg`'s amber for exactly
  one thing: the rule down the side of a citation. There is a full
  `prefers-color-scheme: dark` theme.
- **The citation is the visual centre of an answer.** A bordered card, the
  location as its title in mono, the tuple as a two-column definition list, the
  `git show … | sha256sum` check folded into a `<details>` — open where the
  citation is the page's subject, shut in a list of ten — and copy buttons for
  the digest, the commit and the command. That last one is the whole product: it
  used to mean selecting a wrapped 64-character digest by hand.
- **Four panels, four treatments, on purpose.** An error is loud and
  alarm-tinted with a request id. A refusal is quiet and blue-grey with the
  floor and no request id. A rejection is a bordered card naming the rule. A
  degradation is amber and dashed. Giving any two the same treatment would undo
  a distinction the API went to some trouble to make.
- **No transitions and no animations anywhere.** The accessibility sweep's
  reduced-motion pass is recorded as vacuously true, and that is worth keeping.

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
guard, and several are in that position for reasons worth reading.

The three `class (d)` files — `ask-answered-llm`, `ask-answered-degraded` and
`ask-refused-degraded` — are the ones to read first. They exist because the
console ignored `degraded` and `llm` for a whole phase and **no fixture could
have caught it**: the emitter produced only extractive answers and refusals, so
the compare-and-fail guard never saw either field. Strip those two keys from
`ask-answered-degraded.json` and it is byte-identical to `ask-answered.json` —
which is the defect, stated as data.

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
- **It draws no scale from the score floor.** The floor is −1 and uncalibrated,
  and a bar drawn from it would render a guess as a measurement. The console's
  own sentence says the floor is a mechanism rather than a measured threshold,
  and names no phase: a plan identifier is not something a reader can act on.
  (The server's `detail` for a `below_floor` refusal used to contain one. It no
  longer does — it branches on `floor.calibrated` and names neither a phase nor
  a state it has not been told — so the two sentences agree, and the server's
  is still rendered verbatim like every other sentence the server writes.)
- **It does not send `answerer`.** The field is honoured by `/ask`, and leaving
  it out is a decision `src/api/client.ts` argues in full: `ANSWER_DEFAULT` is
  the operator's, and the console cannot see whether a provider is configured,
  so a control offering `llm` would be a 400 generator on a default deployment.
  Nothing is lost — which answerer wrote the prose, and whether one was tried
  and failed, arrive on every response and are rendered.
- **It has no deployment.** Static bundle, same-origin by construction; P8 puts
  one origin in front of the console and the gateway.
- **No test here hits a live gateway.** MSW intercepts everything. The one real
  end-to-end run is recorded in the root README.

## Accessibility

`jest-axe` runs over every view in the suite and is a **floor, not the claim**:
axe-core's `color-contrast` rule cannot run under jsdom at all, so contrast is
zero-covered by the automated suite and is checked by hand.

The manual sweep — keyboard, screen reader, reduced motion, 320px reflow,
contrast — is in `docs/a11y-sweep-2026-09-03.md`, with one finding fixed, one
pass recorded as not performed, and an addendum saying which of its five passes
the design change invalidated.

`src/contrast.test.ts` closes part of that gap: it reads the hex tokens out of
`src/index.css` and computes WCAG contrast for every pair the design draws, in
both themes — 4.5:1 for text, 3:1 for the two things 1.4.11 actually covers, a
control's boundary and the focus ring. It is a guard on the **palette**, not on
the rendering: nothing in jsdom lays out, so it cannot know which token ends up
behind which text. Decorative edges are deliberately absent from its table,
because claiming 3:1 for a hairline would be inventing a pass.

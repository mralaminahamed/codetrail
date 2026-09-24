## Summary

<!-- What does this change do, and why? -->

## Linked issue

<!-- Closes #N, or explain why there is no issue. -->

## Checklist

CI is disabled on this repository, so these run locally. Tick the ones you ran.

- [ ] `make lint` passes
- [ ] `make test` passes
- [ ] `go test -tags=live ./...` passes against a live Postgres (if the store, queue, binaries or console fixtures changed)
- [ ] Relevant infrastructure checks pass (`make policy`, `make alerts-test`, `make images`, `make image-test`, `make smoke`, `make migrations-lint`), if infrastructure changed
- [ ] Docs updated if behaviour or configuration changed
- [ ] Commits are single-scope Conventional Commits

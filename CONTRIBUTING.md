# Contributing

Thanks for taking a look. This is a small, focused CLI — bug reports and pull
requests are welcome, and so is a plain question in an issue.

## Reporting a bug

Include the output of `oko --version`, your OS and Chrome version, the exact
command you ran, and what you expected instead. A minimal HTML page that
reproduces the problem is worth more than a description. **Redact anything
private** from `read`/`snap`/`net` output before pasting it.

## Pull requests

Before opening one:

```bash
make fmt           # gofmt -w .
make vet           # go vet ./...
make e2e           # real binary vs headless Chrome and fixture pages
```

CI runs the same on Linux and macOS, so a green local run usually means a green
PR.

House rules:

- **One concern per PR.** A bug fix and a refactor in the same diff take three
  times as long to review.
- **An e2e case for every behaviour change.** Add a fixture page under
  `e2e/testdata/` rather than testing against live sites.
- **Keep the output token-lean.** Every command prints what an agent needs and
  nothing else; extra detail belongs behind a flag.
- **Never raise Chrome over the user's work.** No target activation, no focus
  stealing; new windows open in the background.
- **Update the docs in the same commit.** Any change to the CLI surface must
  also update `cmd/skill.md` (embedded in the binary, printed by `oko skill`,
  and the single source of truth for command UX) and `README.md`.
- **Conventional commit subjects** — `feat:`, `fix:`, `docs:`, `refactor:`,
  `test:`, `chore:`. Release notes are generated from them.

## Releases

Maintainer-only. Tag and push:

```bash
git tag -a v1.2.3 -m "v1.2.3"
git push origin v1.2.3
```

GoReleaser builds archives for linux/darwin on amd64 and arm64 and publishes
the GitHub release with a generated changelog.

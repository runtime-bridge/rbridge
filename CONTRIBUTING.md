# Contributing

Thanks for helping. A few things keep the project easy to work on.

## Before you start

Open an issue for anything bigger than a small fix, so we can agree on the approach first. New runtimes are usually a provider manifest plus, if the runtime does not speak ACP, an adapter; see `internal/providers`.

## Working on the code

```sh
make lint   # gofmt, go vet, staticcheck
make race   # unit tests with the race detector
make cross  # build for every supported platform
```

Unit tests run against fake runtimes and need no accounts. Tests against real runtimes are opt-in; see the README.

- Keep `internal/domain` free of imports from other rbridge packages.
- Every adapter must pass the contract suite in `internal/providers/contract`.
- Do not add a dependency without explaining in the pull request what it is for, its licence and its size. Licences must be permissive (MIT, BSD, Apache 2.0, ISC); `make licenses` checks this.
- rbridge must never read provider credential files. There is a test for this; keep it passing.
- Comments explain why, not what.

## Commits and pull requests

Short imperative subject line, a blank line, then the reason for the change. One logical change per pull request.

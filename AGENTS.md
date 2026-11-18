# foreman

One Go `package main`. `README.md` is the spec: a change in behaviour updates its section in the
same commit.

- Checks: `gofmt -l .`, `go vet ./...` and `go test -race ./...`, all clean.
- Tests stay off the machine. `useFakeRex(t, …)` puts a fake in `rex` and `tempState(t)` moves
  `stateFile` and the evidence to a temp dir; a test that loads or updates the state calls one of
  them first, or it writes the real `~/.local/state/foreman/state.json`.
- `go build -o /dev/null .` checks a build. Installing to `~/.local/bin` replaces the foreman in
  use, so it waits for the user to ask.
- README's Development section says how Rex is reached (`rexAPI`) and what to restart after an
  install: read it before adding a call to Rex or going live.
- History is `main` alone, on the Forgejo `origin`: in the main checkout, commit there, with no
  branch or PR.

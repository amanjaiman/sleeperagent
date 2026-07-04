# Contributing to SleeperAgent

Thanks for your interest! SleeperAgent is a small, focused Go project and
contributions are welcome.

## Development setup

- Go 1.23+ and (for the default backend / integration tests) `tmux`.
- Clone, then:

```bash
make build     # build ./sleeperagent
make test      # unit tests (no tmux needed)
make check     # gofmt check + go vet + tests
```

On Windows, develop inside WSL — `tmux` and the `pty` backend are Unix-only.

## Before opening a PR

- `make check` must pass (gofmt-clean, `go vet` clean, tests green).
- Add tests for new behavior. The core loop is unit-testable via the `Pane`
  interface and an injectable clock — see `internal/supervisor/supervisor_test.go`.
- For changes that touch the live tmux/pty path, run the relevant script in
  [`test/`](test/) (these need `tmux`, and some need `python3`).
- Keep new code in the style of the surrounding code: small packages, plain data
  adapters, no surprise dependencies.

## Adding a new agent

You usually don't need to write code — agents are config-driven. Add an
`[agents.<name>]` block to your `config.toml` (see
[`config.example.toml`](config.example.toml)) with `launch_cmd`, `limit_patterns`
(using `(?P<ts>)`, `(?P<time>)`, or `(?P<dur>)` named groups), and `inject_style`.
Use `sleeperagent parse --agent <name> "<a real limit message>"` to confirm your
patterns match and resolve a reset time.

## Reporting limit-string drift

The agents' usage-limit messages are undocumented and change over time. If a
pattern stops matching, please open an issue with the exact text (redact anything
sensitive) so the built-in defaults can be updated.

## License

By contributing, you agree that your contributions are licensed under the
[MIT License](LICENSE).

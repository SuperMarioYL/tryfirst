# Changelog

All notable changes to Tryfirst are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - 2026-11-25

First release.

### Added

- **Detect and plan (m1).** `tryfirst plan <repo>` copies or clones a repository,
  resolves the commit, and derives install and demo commands from `package.json`,
  `pyproject.toml`, `requirements.txt`, or a positively-matched README quickstart
  section. Every command carries its evidence (`package.json:scripts.start`,
  `README:## Quick Start > bash fence @L66`), `--json` writes `plan.json`, and a
  repository with nothing runnable returns `no_runnable_demo` with a reason
  instead of an error.
- **Isolated sandbox (m2).** `tryfirst run <repo>` renders a per-run image from a
  cached base (`python:3.12-slim`, `node:22-slim`, `debian:bookworm-slim`), starts
  a deterministic OpenAI/Anthropic-compatible stub on `127.0.0.1:8787` inside the
  container, runs the demo under `--network none`, prints the exit code and the
  captured output, then removes the container, the per-run image, and the temp
  checkout.
- **Receipt and report (m3).** Every run writes a digest-sealed
  `tryfirst/receipt@1` JSON, a self-contained offline HTML report with the verdict,
  the plan provenance, model-call counts, logs, and artifact hashes, and a `.log`
  file, all under `--report-dir` (default `tryfirst-report`). `--keep` leaves the
  cloned work directory in place for inspection.
- **Live gate and gc (m4).** `--model live` proxies model calls through a
  host-side gate that holds the operator credential, gives the container only a
  per-run ephemeral token, and stops the run at 20 requests, 50k tokens, or 90
  seconds; a missing upstream fails closed before any container starts.
  `tryfirst gc` prunes Tryfirst-labelled images older than seven days.

[0.1.0]: https://github.com/SuperMarioYL/tryfirst/releases/tag/v0.1.0

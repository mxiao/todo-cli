# End-to-end acceptance & compatibility tests

`npm run test:e2e` runs the first-release acceptance suite against the shipped product: real
`todo serve`, CLI and TUI processes on private data directories, real browsers and a real
pseudo-terminal. Only the outside world is replaced, by fixed fakes:

| Fake | File | What it stands in for |
|---|---|---|
| Mock model | `support/mock-model.mjs` | an OpenAI-compatible chat service (`TODO_CLI_MODEL_BASE_URL`). Each feature (intake, decide, summarize, agent selection, connection test) gets a deterministic answer from a fixed test set. `setDown(true)` simulates an outage (HTTP 503). |
| Mock agent | `agents/mock-agent.mjs` | a command-line agent registered with `todo agent add … -- node mock-agent.mjs <mode>`: `all` writes back text, a real file, real command output and a real git commit; `fail`, `flaky`, `slow`, `secret` script failures, retries, cancellation and redaction. |
| Terminals | `support/pty.mjs` | Terminal.app and iTerm2 profiles (`TERM`, `TERM_PROGRAM`, …) on node-pty, sending their key and SGR mouse sequences. |

Quality gate: model and agent behaviour is accepted with this fixed test set and mock responses;
no real external model is needed in CI.

## Layout

```
tests/e2e/
  browser/acceptance.spec.mjs   page acceptance — runs on chromium, firefox, webkit (+ chrome/msedge)
  terminal/tui.spec.mjs         TUI keyboard/mouse, keyboard-only + NO_COLOR, TUI ↔ web sync — per terminal profile
  system/acceptance.spec.mjs    CLI + REST API: model features, agents, permissions, redaction, outage,
                                export/import, backup/restore, crash recovery, conflicts
  support/                      harness (todo serve + CLI + mock model), fixtures, pty, mock model
  agents/mock-agent.mjs         fixed mock agent
  traceability/                 acceptance catalogue, report builder, Playwright reporter (+ unit tests)
```

Playwright projects (`playwright.config.mjs`): `system`, `terminal`, `chromium`, `firefox`, `webkit`,
optional branded browsers from `TODO_E2E_CHANNELS=chrome,msedge`, and `regression-<browser>`
(the existing web UI suite `e2e/web`, Chromium unless `TODO_E2E_BROWSERS` says otherwise).

```bash
npm install && npx playwright install chromium firefox webkit   # once
npm run test:e2e                                  # everything; builds bin/todo first
TODO_E2E_CHANNELS=chrome,msedge npm run test:e2e  # also the installed Chrome and Edge
npx playwright test --project webkit              # one project
npx playwright test --grep @AT-06                 # one acceptance item, in every project
npm run test:e2e:unit                             # unit tests of the reporter and the mock model
npm run test:e2e:report                           # open the HTML report
```

## Traceable report

Every test carries the tags of the acceptance items it proves (`@AT-01` … `@AT-15`, see
`traceability/requirements.mjs`, which also maps them to FR/NFR/验收 numbers). Each run writes
to `reports/e2e/`:

- `traceability.md` / `traceability.json` — per acceptance item: requirements, status, and every test
  with its project, terminal profile and outcome. Also the measured sync latencies (2-second limit),
  the deferred items, the compatibility matrix with the browser versions that actually ran, and the
  run metadata (time, commit, macOS version, Node, Playwright).
- `html/` (Playwright report with traces of failures), `junit.xml`, `results.json`.

The compatibility matrix follows the default decision: macOS 14+, the latest two stable versions of
Safari, Chrome, Edge and Firefox, Terminal and iTerm2. Rows are honest about what ran. A branded
browser that was not installed is reported as "通过（同引擎）" from its engine project (Chromium for
Chrome/Edge, WebKit for Safari). Previous stable versions and branded Safari are confirmed in UAT,
or by running the suite on a machine with those versions installed.

Deferred: voice input (PRD 验收标准第 9 条) is not part of the first release. It is listed as
延期 in every report. The first release accepts text input only.

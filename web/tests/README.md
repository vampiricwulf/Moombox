# Frontend JS tests

Uses Node.js's built-in test runner (`node:test` module). No dev dependencies,
no package.json — just `.test.mjs` files that import from `../public/modules/`.

## Running

From the repo root:

```bash
# Run every test file
node --test web/tests/*.test.mjs

# Run a single file
node --test web/tests/filter-parser.test.mjs

# With detailed output per test
node --test --test-reporter=spec web/tests/*.test.mjs
```

Requires Node.js 20+. The `.mjs` extension tells Node to parse files as ES
modules without needing a `package.json` with `"type": "module"`.

## Scope

These tests target the pure, dependency-free modules under
`web/public/modules/` — modules that don't touch the DOM, WebSocket, or
network. Good candidates: parsers, formatters, pure utilities. UI-heavy
modules (`settings.js`, `setup.js`, `player.js`, `trimmer.js`, `app.js`)
would need a DOM test harness (jsdom, Playwright, etc.) — out of scope here.

## Adding a test

1. Create `web/tests/<module-name>.test.mjs`.
2. Import from `../public/modules/<module-name>.js` (keep the `.js`).
3. Use `import { test } from "node:test"` and `import assert from "node:assert/strict"`.
4. Verify with `node --test web/tests/<module-name>.test.mjs`.

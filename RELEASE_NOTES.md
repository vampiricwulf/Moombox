### Internal

- **Update `bgutil-sidecar` to jsdom v29.1.1** (from v27.4.0). jsdom v28+v29 brought a complete CSSOM rewrite (replacing `@acemir/cssom` + `cssstyle` with an internal `css-tree`-based implementation), raised minimum Node to v22.13.0 (we ship v22.22.2, satisfied), and added bad-port blocking per the fetch spec. BotGuard PO token generation verified end-to-end via live mint against Google's WAA endpoint before merge — 152-byte token in 416ms, cache-hit re-mint in 502µs.
- **Bump `esbuild` to 0.28.0** in `bgutil-sidecar` devDependencies (dev-only, not embedded in the binary).
- **Bump `actions/checkout` to v6** across CI workflows. v6 persists git credentials in `$RUNNER_TEMP` instead of `.git/config`; minimum runner is v2.329.0, which GitHub-hosted runners already provide.
- **Allow Dependabot to trigger Claude code review** workflow (`allowed_bots: 'dependabot[bot]'`) so dep-bump PRs get an automated review pass — opens up the path for risky bumps like the jsdom one above to be caught at PR-time.

## Improvements

- **Accurate internet-connectivity detection on every platform.** Moombox now verifies *real* reachability with a multi-target TCP probe (Cloudflare / Google / Quad9 on port 443, first success wins) instead of the Windows heuristic that only reported network-adapter state and could claim "online" during a genuine outage. The probe is identical on Windows and Linux, and its targets are configurable via a new `[connectivity] probe_targets` setting.

## Bug Fixes

- **Upcoming and live streams are no longer wrongly errored during an internet outage.** When connectivity dropped (or the monitor briefly mis-detected it), the stream poll loops were counting the resulting network failures toward a give-up limit and moving waiting streams to **Error** after ten failures. Probe and fetch errors are now classified: network-class failures (DNS, timeouts, TLS, connection resets, and transient 401/403/429/5xx) keep the stream waiting through the outage and **never** count toward giving up — only definitive service verdicts (e.g. 404/410) do. This applies to both YouTube and Twitch, including the confirmatory fetch at the moment a stream goes live.
- **Moombox now leaves offline mode promptly when the network returns.** Recovery happens on the first confirmed-reachable poll, and a successful request self-corrects the monitor without an extra blocking probe — so the app resumes monitoring and downloading as soon as the connection is back.
- **A scheduled stream that becomes private, removed, or region-blocked while still upcoming now gives up** (confirmed via an authoritative full fetch) instead of polling indefinitely.

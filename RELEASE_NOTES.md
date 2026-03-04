### Bug Fixes

- Fixed Firefox processes surviving past timeouts — added Windows Job Object with `KILL_ON_JOB_CLOSE` to reliably kill all child processes (including reparented ones) when the timeout fires
- Added 30-second stagger between sequential Firefox launches so the profile is fully released before the next launch

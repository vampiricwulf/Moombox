### Bug Fixes

- Added `--new-instance` flag to Firefox commands to prevent "Firefox is already running" dialog when the user has Firefox open
- Fixed `runWithTimeout` blocking forever if `taskkill` fails to kill a hung Firefox process — now falls back to direct process kill after 5 seconds

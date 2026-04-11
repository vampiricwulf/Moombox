### Bug Fixes

- **Security:** Self-updater now rejects unsigned binaries — signature verification is mandatory and cannot be bypassed
- **Security:** Downloads capped at 200 MB to prevent disk exhaustion from a compromised source
- Fixed concurrent update apply requests racing on binary swap (returns 409 Conflict)

### Improvements

- GitHub API rate-limit errors (403/429) now show a clear diagnostic instead of a generic status code
- Verify signature endpoint returns proper HTTP error status (422) instead of 200 with error field
- Fixed misaligned indentation in update restart panic recovery handler

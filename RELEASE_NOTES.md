### Bug Fixes

- **Fixed ALR sig transform false match** — cipher solver was matching unrelated code 5,413 chars from the ALR marker, extracting garbage as the signature transform chain. Now iterates all ALR markers with proximity-bounded matching (200 chars)
- **Fixed identity-function cipher candidates** — `_multiTry` now rejects candidates that return input unchanged, preventing garbage transforms from being accepted
- **Hardened IIFE closing detection** — verifies the closing bracket is within 200 chars of EOF to prevent false matches in nested functions
- **Hardened `var window=this;` stripping** — uses regex for whitespace tolerance instead of exact string match

### Improvements

- **Widened URL class detection** — n-param URL class pattern now supports any dotted identifier (`h.Foo`, `_yt.xY`) and bare identifiers, not just `g.XXXX`
- **Added cipher cache invalidation on 403** — when DASH segments get 403 errors before any bytes are written (indicating cipher failure), the solver is automatically invalidated so the next attempt refetches the player
- **Extended cipher diagnostic logging** — Debug-level logging for all extraction strategy results; Warn-level when all strategies for a component fail
- **Improved real player test coverage** — sig solver output is now validated (not just checked for non-nil), catching the false ALR match bug that was previously invisible

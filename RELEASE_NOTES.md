## Bug Fixes

- **Hot-fix for v2.6.23's manifestless DASH path**: the new strategy was double-decrypting the n-param. `parseFormatsWithCipher` already decrypts n on every adaptiveFormat URL during player-response parsing; the strategy was unconditionally running `cipher.RoutedDecryptNInURL` on those already-decrypted URLs, putting them through the n cipher a second time. The composite solver has no idempotency guard, so the result was garbage URLs that YouTube 403'd on every fetch — visible in the field as a tight loop of `[POT] PO token ready` → `manifestless DASH 403 signal — invalidating solver and POT` → `segment downloaders stopped timeSinceLastSeg=5s`.

  Drop the redundant decrypt. The contract is now "URLs from `videoInfo.Formats[]` are already cipher-resolved." Cipher rotation mid-stream is still handled via the existing `OnCipherFailure` callback. Tested against the same members-only stream that triggered the 2.6.23 deployment — segments now download cleanly from sq=0.

  The 2.6.18+ ANDROID_VR DASH fallback for public streams was unaffected; this regression only impacted the new manifestless DASH path introduced in 2.6.23 for streams that need cookied auth (members-only / age-restricted / login-required).

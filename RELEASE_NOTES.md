## Improvements

- **PO-token bindings now match yt-dlp exactly.** GVS tokens are bound by yt-dlp's `get_webpo_content_binding` rule — video ID when YouTube's `html5_generate_content_po_token` experiment is active on the page (it currently is), datasync ID for authenticated sessions such as members-only captures, visitor data otherwise — resolved once at extraction and carried through every mint and credential refresh, so the binding can never drift mid-job. Player-API tokens bind to the video ID, upstream's unconditional rule for that context. The 2.8.0 interim policy (visitor data always) worked only while YouTube wasn't enforcing bindings on GVS; the stall that had parked the yt-dlp rule reproduced on a build without it and traced to the client-ranking bug fixed in 2.8.0, clearing the way to activate the correct rule.
- **Mint provenance logs report the real binding kind** (`videoID` / `datasyncID` / `visitorData` / `channelID`) instead of a fixed label, so a 403 investigation can see exactly which rule produced the failing token.

## Internal

- Token minting still uses the cached `/att/get` minter — the same sourcing yt-dlp's bgutil provider uses. The challenge-sourced fresh-per-mint minters exceed upstream behavior and remain dormant unless field evidence calls for them.
- Documentation corrected to match the shipped behavior (`docs/spec/platform-services.md` player-token section; 2.8.0 release notes).

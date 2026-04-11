### Bug Fixes

- **Fixed premature download start for rescheduled streams** — when a stream's scheduled time was pushed back, Moombox would start downloading at the original time (capturing YouTube's waiting room/offline slate instead of actual content)
- ANDROID_VR probe status is now cross-validated against a full WEB fetch before transitioning from Upcoming to Live or VOD
- `classifyStream` now uses `videoDetails.isUpcoming` as a standalone upcoming signal, preventing YouTube's waiting room from being misclassified as live

### Improvements

- Periodic full WEB fetch every 30 minutes during upcoming polling catches metadata changes (e.g., rescheduled start times) that the lightweight ANDROID_VR probe can't see

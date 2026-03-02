### Bug Fixes

- Fixed background segment mux goroutines being killed on job cancellation, leaving orphaned partial `.mp4` files in the output directory with no database record
- Background muxes now use a detached context so already-downloaded data is always muxed to completion, even during cancellation

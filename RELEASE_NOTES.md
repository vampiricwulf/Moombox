### Improvements

- Move settings change indicator to line prefix — `*>` for modified+selected, `* ` for modified, replacing the trailing ` *` that caused line wrapping
- Fix V2 textinput rendering width+1 (cursor block) causing layout overflow
- Fix button click Y detection using render-time position tracking instead of bottom-up calculation

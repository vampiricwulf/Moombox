### Improvements

- Align cipher solver JS globals with upstream ejs 0.8.0 (commit `68448fa`) — `window`, `self`, and `globalThis` now reference the same object, preventing breakage when YouTube players access `self.location.origin`

### Internal

- Add screenshots to README (Web Dashboard, TUI, Video Player)
- Add test for cipher setup code global unification (`window === self === globalThis`)

---
name: moombox-web-ui
description: Use when building or modifying Web UI features — Shoelace v2.16 component catalog, SPA module patterns, WebSocket handling, mobile breakpoints, and go:embed workflow
---

# Web UI Development

Vanilla JS SPA using **Shoelace v2.16** (CDN via jsDelivr). Static assets in `web/public/`, embedded via `go:embed` in `web/embed.go`. **Changes require `go build` to take effect.**

## Shoelace Component Catalog

**Check [Shoelace docs](https://shoelace.style/) before building custom components.**

Components currently used in the codebase are marked with *. Others are available via Shoelace but not yet used.

### Form & Input
| Component | Use For |
|-----------|---------|
| `<sl-input>` * | Text, number, password, email, search |
| `<sl-textarea>` * | Multi-line text |
| `<sl-select>` + `<sl-option>` * | Dropdown selection |
| `<sl-checkbox>` * | Boolean toggle (in lists) |
| `<sl-switch>` * | Boolean toggle (standalone) |
| `<sl-radio-group>` + `<sl-radio-button>` | Mutually exclusive options |
| `<sl-range>` | Slider input |

### Layout & Containers
| Component | Use For |
|-----------|---------|
| `<sl-dialog>` * | Modal dialogs (settings, confirmations) |
| `<sl-tab-group>` + `<sl-tab>` + `<sl-tab-panel>` * | Tabbed content |
| `<sl-details>` * | Collapsible sections |
| `<sl-divider>` * | Visual separators |
| `<sl-drawer>` | Slide-out panels |
| `<sl-card>` | Contained content blocks |

### Feedback & Status
| Component | Use For |
|-----------|---------|
| `<sl-alert>` * | Inline messages and toast notifications |
| `<sl-badge>` * | Status indicators, counts |
| `<sl-spinner>` * | Loading state |
| `<sl-progress-bar>` * | Progress indication |
| `<sl-skeleton>` * | Loading placeholder shapes |
| `<sl-tag>` * | Labels, categories |
| `<sl-icon>` * | Icons (Bootstrap Icons set) |
| `<sl-tooltip>` | Hover hints |

### Actions & Navigation
| Component | Use For |
|-----------|---------|
| `<sl-button>` * | Primary actions (supports variants, sizes, loading) |
| `<sl-icon-button>` * | Compact icon-only actions |
| `<sl-menu>` + `<sl-menu-item>` * | Context/action menus |
| `<sl-dropdown>` | Trigger + menu container |
| `<sl-copy-button>` | Click-to-copy |

### Shoelace Events
Use `sl-` prefixed events, not native DOM events:
- `sl-change` — value changed (select, checkbox, switch)
- `sl-input` — value changing (text input, textarea)
- `sl-tab-show` — tab selection changed
- `sl-after-hide` — dialog/drawer fully hidden (for cleanup)
- `sl-request-close` — dialog close requested (can prevent)
- `sl-select` — menu item selected

## Module Pattern

Each UI feature is an exported class in `web/public/modules/`:
```javascript
export class FooController {
    constructor() { /* state init */ }
    init(container) { /* render, bind events */ }
    // No formal destroy() — cleanup varies by module
}
```

### Existing Modules
Every file under `web/public/` and what it owns is in `docs/spec/user-interfaces.md` § File Layout — read that table before deciding where a feature goes (the job details dialog, filter bar, log panel, Files tab, update indicator and cookie-setup dialog each have their own module). The ones feature work touches most:

| Module | File | Purpose |
|--------|------|---------|
| App | `web/public/app.js` | Main SPA shell, WebSocket, job list, status bar |
| Job details | `web/public/modules/job-details.js` | The job details dialog: render, live updates, action buttons, per-job logs |
| Player | `web/public/modules/player.js` | Video playback, chat overlay, multi-segment seeking, chapter strip and list (`GET /api/jobs/{id}/chapters`) |
| Chapter import | `web/public/modules/chapter-import.js` | The player's Import chapters dialog: paste box, offset, debounced preview from `POST /api/jobs/{id}/chapters/preview` (no client-side parser), save and restore (`POST /api/jobs/{id}/chapters`, `/chapters/restore`) |
| Setup | `web/public/modules/setup.js` | First-run wizard, FFmpeg install |
| Settings | `web/public/modules/settings.js` | Config editor (all sections) |
| Trimmer | `web/public/modules/trimmer.js` | Clip creation |
| Stats | `web/public/modules/stats.js` | Statistics dashboard |
| Imports | `web/public/modules/imports.js` | Zip archive import |
| Utils | `web/public/modules/utils.js` | Shared formatters (`formatTimestamp`, `formatBytes`, `formatDurationSeconds`, etc.) |

## WebSocket Message Handling

`web/public/app.js` manages the WebSocket connection and routes messages by type in `handleMessage`, one `case` per frame type.

**All message types:** the table in `docs/spec/user-interfaces.md` § Server-to-Client Message Types is the one list, with each payload and sender. A new frame type needs a `case` in `handleMessage` and a row in that table; `TestWebSocketFrameTableMatchesTheClient` (`internal/docs/drift_test.go`) fails until both exist.

## Toast Notifications

```javascript
showToast(message, variant = "primary") // variants: "success", "warning", "danger", "primary"
```
Uses `<sl-alert>` with `alert.toast()`. Always go through `showToast`: it is the one place a toast is made, and `toastDuration` (`web/public/modules/utils.js`) decides how long each stays. A `danger` toast stays until closed; any other stays 60 ms per character, at least 6 s for a `warning` and 3 s for the rest, at most 15 s, with a countdown bar that runs for the same time. Icons mapped per variant.

## Theme System

Light/dark toggle persisted to `localStorage` key `"moombox-theme"`. Default: system preference via `prefers-color-scheme`. Applies `sl-theme-light` or `sl-theme-dark` class to HTML element.

## HTML Sanitization

Always use `escapeHtml()` for dynamic content to prevent XSS. Replaces `& < > " '` with HTML entities.

## Mobile Breakpoints

```css
@media (max-width: 992px)  { /* tablet — 2-column grid */ }
@media (max-width: 768px)  { /* phone — single column, larger touch targets */ }
@media (max-width: 576px)  { /* small phone — further compact */ }
@media (hover: none)        { /* touch — hide keyboard shortcut hints */ }
```

## Common Mistakes

- Using native `<select>` or `<input>` instead of Shoelace components — breaks visual consistency
- Listening for `change` instead of `sl-change` — event never fires on Shoelace elements
- Forgetting `go build` after editing assets — old version still embedded
- Not using `escapeHtml()` for user-provided content — XSS vulnerability
- Not testing mobile breakpoints — features may be unusable on phone
- Forgetting dark mode — test both themes, don't hardcode colors

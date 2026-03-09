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
| Module | File | Purpose |
|--------|------|---------|
| App | `app.js` | Main SPA shell, WebSocket, job list, status bar, logs |
| Player | `modules/player.js` | Video playback, chat overlay, multi-segment seeking |
| Setup | `modules/setup.js` | First-run wizard, FFmpeg install |
| Settings | `modules/settings.js` | Config editor (all sections) |
| Trimmer | `modules/trimmer.js` | Clip creation |
| Stats | `modules/stats.js` | Statistics dashboard |
| Imports | `modules/imports.js` | Zip archive import |
| Utils | `modules/utils.js` | Shared formatters (`formatTimestamp`, `formatBytes`, `formatDurationSeconds`, etc.) |

## WebSocket Message Handling

`app.js` manages the WebSocket connection and routes messages by type:

**All message types:** `initial_state`, `jobs_update`, `job_update`, `log`, `check_timers`, `disk_status`, `update_available`, `pong`

## Toast Notifications

```javascript
showToast(message, variant = "primary") // variants: "success", "warning", "danger", "primary"
```
Uses `<sl-alert>` with `alert.toast()`. Auto-closes after 3s with countdown animation. Icons mapped per variant.

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

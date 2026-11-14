# Writing the Moombox docs site

This folder is the source of the GitHub Pages site (MkDocs Material). Pages
live in `docs/`; the nav is in `mkdocs.yml`.

```bash
pip install -r docs-site/requirements.txt
cd docs-site && mkdocs serve          # live preview at http://127.0.0.1:8000
mkdocs build --strict                 # what CI runs: any warning fails
```

## Audience and tone

The site is for people running Moombox, not for people changing its code.
`SPEC.md` and `docs/spec/` stay the developer reference. Explain what
Moombox does, what the operator sees, and what they can change — not which
Go function does it. Mention internals only where they explain behaviour the
operator will notice.

- Plain, direct sentences. Second person ("you"). No marketing language.
- Name things the way the UIs name them: the dashboard's button and tab
  labels, the TUI's chord letters, the status names (`Queued`, `Upcoming`,
  `Live`, `Downloading`, `Muxing`, `Finished`, `Error`, `Cancelled`,
  `COOKIES?`).
- Every claim must be true of the code on `main`. When unsure, read the code;
  if still unsure, leave the claim out.
- Platform differences (Windows vs Linux vs Docker) get an admonition
  (`!!! note "Linux"`) or content tabs (`=== "Windows"`).
- No Go identifiers, file paths inside `internal/`, or test names in prose.

## Settings reference format

Every `config.toml` key has exactly one heading under `docs/configuration/`,
written as its dotted path in backticks. Arrays of tables use `[]`:

````markdown
### `downloader.segment_workers`

| Default | Range | Applies | Dashboard |
|---|---|---|---|
| `4` | 1–16 | Next download | Settings → Downloads → Segment workers |

What it does, when to change it, and what happens at the edges.

```toml
[downloader]
segment_workers = 8
```
````

`internal/docs/settings_reference_test.go` fails `go test ./...` when a key
in the config has no heading, when a heading names a key that no longer
exists, or when a key is documented twice. Adding a setting means adding its
heading here.

Link to a setting with its heading's slug — the dotted path with the dots
and brackets dropped: `configuration/downloader.md#downloadersegment_workers`,
`configuration/channels.md#channelsarchive_slots`.

## Bug Fixes

- **Settings nav strip stays inside the viewport on mobile.** The horizontal-scroll chip strip introduced in 2.6.29 was extending past the right edge instead of triggering its `overflow-x: auto`. Flex-column items default to `min-width: auto`, which let `.settings-container`, `.settings-menu`, and the inner `sl-menu` stretch to fit all 12 nav chips combined. Capping the constraint chain to viewport width (`min-width: 0` + `max-width: 100%` on each layer) makes the sl-menu actually scroll instead of pushing the page sideways.

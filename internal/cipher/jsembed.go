package cipher

import _ "embed"

// nBindingTemplate is the n-param solver binding template loaded from
// js/n_binding_template.js via go:embed. fmt.Sprintf fills in the URL class
// reference and the candidate-array literal at use sites.
//
//go:embed js/n_binding_template.js
var nBindingTemplate string

// fullPlayerSetupCode is the browser stub block prepended to YouTube
// player.js when running it inside Goja. Sourced from
// js/full_player_setup.js so the JS is editable + lintable as plain JS
// rather than buried in a Go backtick literal (audit cipher.md T2).
//
//go:embed js/full_player_setup.js
var fullPlayerSetupCode string

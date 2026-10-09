// Package redact keeps credentials out of the text Moombox shows and keeps:
// error strings, log lines (moombox.log, and the ring buffer behind
// /api/logs, the WebSocket log frames and the TUI log panel), a job's stored
// error and the notification embeds built from it.
//
// There is one helper per kind of secret, so a call site says which secret it
// is hiding and the rule for finding that secret lives in one place rather
// than in a string edit at every site that meets it:
//
//   - the GVS PO token (potoken.go): PoToken for an error, PoTokenURL for a
//     URL, PoTokenText for text already flattened;
//   - a credential inside a URL (url.go): URLOrigin for a URL whose secret can
//     sit anywhere in it, such as a webhook URL.
package redact

// Marker replaces a secret wherever one is cut out.
const Marker = "<redacted>"

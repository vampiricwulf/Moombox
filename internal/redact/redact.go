// Package redact keeps credentials out of the text Moombox shows and keeps:
// error strings, log lines (moombox.log, and the ring buffer behind
// /api/logs, the WebSocket log frames and the TUI log panel), a job's stored
// error and the notification embeds built from it.
//
// There is one helper per kind of secret, so a call site says which secret it
// is hiding and the rule for finding that secret lives in one place rather
// than in a string edit at every site that meets it:
//
//   - a media URL's credentials (media.go) — a googlevideo URL's client
//     public IP, signatures, GVS PO token and the rest of what the server
//     signed it for, and the playback session a Twitch weaver playlist's or
//     edge segment's path spells: MediaError for an error, MediaURL for a
//     URL, MediaText for text already flattened;
//   - the GVS PO token alone, in any URL (potoken.go): PoTokenURL for a URL,
//     PoTokenText for text; the media rules apply both;
//   - a credential inside a URL (url.go): URLOrigin for a URL whose secret can
//     sit anywhere in it, such as a webhook URL; URLUserinfo for one whose
//     only place for a secret is its user:password@, such as
//     network.public_url.
package redact

// Marker replaces a secret wherever one is cut out.
const Marker = "<redacted>"

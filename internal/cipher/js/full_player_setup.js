// full_player_setup.js — Cipher-specific JS prepended to YouTube player.js
// before the modified IIFE runs inside Goja. Loaded by extractor_full_player.go
// via go:embed.
//
// As of test.34 (audit cipher.md D1/D4 + goja.md C4), the DOM/Web-API stubs
// (document, navigator, location, screen, performance, storage, XHR, crypto,
// AbortController, ReadableStream, CustomEvent, CSS, Intl, MutationObserver,
// IntersectionObserver, ResizeObserver, fetch, etc.) are provided by
// internal/goja's RegisterDOMShim. Real TextEncoder/TextDecoder come from
// internal/goja's RegisterEncoding (the previous hand-stub here returned
// Uint8Array(0) which would silently miscompute any signature touching
// TextEncoder).
//
// What stays here is cipher-specific: the _multiTry helper that's referenced
// by buildSolverBindings to try multiple sig-candidate functions until one
// produces a different string from the input.

var _multiTry = function(_generators) {
    return function(_input) {
        var _errors = [];
        for (var _i = 0; _i < _generators.length; _i++) {
            try {
                var _r = _generators[_i](_input);
                if (typeof _r === "string" && _r.length > 0 && _r !== _input) return _r;
                _errors.push("candidate " + _i + ": returned " + (typeof _r) + " " + JSON.stringify(_r));
            } catch(_e) {
                _errors.push("candidate " + _i + ": " + (_e.message || _e));
            }
        }
        throw new Error("no cipher solutions found (" + _generators.length + " candidates tried: " + _errors.join("; ") + ")");
    };
};

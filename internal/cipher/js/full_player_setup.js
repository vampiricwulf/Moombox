// full_player_setup.js — Cipher-specific JS prepended to YouTube player.js
// before the modified IIFE runs inside Goja. Loaded by extractor_full_player.go
// via go:embed.
//
// As of test.34 (audit cipher.md D1/D4 + goja.md C4), the SHARED DOM/Web-API
// stubs (document, navigator, location, screen, performance, storage, XHR,
// crypto, MutationObserver / IntersectionObserver / ResizeObserver, fetch,
// matchMedia, requestAnimationFrame, …) are provided by internal/goja's
// RegisterDOMShim. Real TextEncoder/TextDecoder come from RegisterEncoding
// (the old hand-stub here returned Uint8Array(0) which silently miscomputed
// any signature touching TextEncoder).
//
// As of test.42, the cipher-specific extras (AbortController, ReadableStream,
// CustomEvent, CSS, Intl) live HERE rather than in the shared shim. They
// were briefly in dom_shim.go but bgutils pulls the same shim for BotGuard,
// and BotGuard fingerprints the existence + native-code shape of these
// globals — stub objects pass `typeof === "function"` but fail the follow-
// up probe and steer the integrity token toward the websafe fallback.
//
// Also here: the cipher-specific `_multiTry` helper referenced by
// buildSolverBindings.

if (typeof globalThis.AbortController === "undefined") {
    globalThis.AbortController = function() {
        this.signal = { aborted: false, addEventListener: function() {}, removeEventListener: function() {} };
        this.abort = function() { this.signal.aborted = true; };
    };
}
if (typeof globalThis.ReadableStream === "undefined") {
    globalThis.ReadableStream = function() { this.cancel = function() { return Promise.resolve(); }; };
}
if (typeof globalThis.CustomEvent === "undefined") {
    globalThis.CustomEvent = function(t, o) { this.type = t; this.detail = o && o.detail; };
}
if (typeof globalThis.CSS === "undefined") {
    globalThis.CSS = { supports: function() { return false; }, escape: function(s) { return String(s); } };
}
if (typeof globalThis.Intl === "undefined") {
    var _intlProto = { resolvedOptions: function() { return { timeZone: 'UTC', locale: 'en-US' }; }, format: function() { return ''; }, formatToParts: function() { return []; } };
    var _intlCtor = function() { return Object.create(_intlProto); };
    _intlCtor.supportedLocalesOf = function() { return []; };
    globalThis.Intl = {
        DateTimeFormat: _intlCtor,
        NumberFormat: _intlCtor,
        PluralRules: _intlCtor,
        RelativeTimeFormat: _intlCtor,
        Collator: _intlCtor,
        ListFormat: _intlCtor,
        Segmenter: _intlCtor
    };
}

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

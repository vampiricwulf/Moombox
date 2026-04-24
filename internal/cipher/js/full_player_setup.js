// full_player_setup.js — Browser stub block prepended to YouTube player.js
// when running it inside Goja. Provides minimum DOM/Web-API surface the
// player code touches at startup so we can extract the n-param + signature
// solvers without a real browser. Loaded by extractor_full_player.go via
// go:embed.
//
// Stubs are intentionally minimal — they answer enough for player init
// to complete, but they are not full polyfills. Anything that tries to
// actually render or fetch will quietly no-op rather than throw, which
// is exactly what we want for headless extraction.

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
if (typeof globalThis.XMLHttpRequest === "undefined") {
    globalThis.XMLHttpRequest = function() {};
    globalThis.XMLHttpRequest.prototype = {open:function(){},send:function(){},setRequestHeader:function(){}};
}
globalThis.location = {
    hash: "", host: "www.youtube.com", hostname: "www.youtube.com",
    href: "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
    origin: "https://www.youtube.com", pathname: "/watch",
    port: "", protocol: "https:", search: "?v=dQw4w9WgXcQ",
    password: "", username: "", toString: function() { return this.href; }
};
if (typeof globalThis.document === "undefined") {
    globalThis.document = Object.create(null);
    globalThis.document.addEventListener = function(){};
    globalThis.document.createElement = function(t){ return {tagName:t,style:{}}; };
    globalThis.document.getElementById = function(){ return null; };
    globalThis.document.querySelector = function(){ return null; };
    globalThis.document.querySelectorAll = function(){ return []; };
}
if (typeof globalThis.navigator === "undefined") {
    globalThis.navigator = Object.create(null);
    globalThis.navigator.userAgent = "Mozilla/5.0";
}
if (typeof globalThis.self === "undefined") {
    globalThis.self = globalThis;
}
if (typeof globalThis.window === "undefined") {
    globalThis.window = globalThis;
}
if (typeof fetch === "undefined") {
    var fetch = function() { return Promise.reject("no fetch"); };
}
if (typeof AbortController === "undefined") {
    var AbortController = function() { this.signal = {}; this.abort = function(){}; };
}
if (typeof ReadableStream === "undefined") {
    var ReadableStream = function() {};
}
if (typeof CustomEvent === "undefined") {
    var CustomEvent = function(t,o) { this.type = t; };
}
if (typeof MutationObserver === "undefined") {
    var MutationObserver = function() { this.observe = function(){}; this.disconnect = function(){}; };
}
if (typeof ResizeObserver === "undefined") {
    var ResizeObserver = function() { this.observe = function(){}; this.disconnect = function(){}; };
}
if (typeof IntersectionObserver === "undefined") {
    var IntersectionObserver = function() { this.observe = function(){}; this.disconnect = function(){}; };
}
if (typeof matchMedia === "undefined") {
    var matchMedia = function() { return {matches:false,addListener:function(){},addEventListener:function(){}}; };
}
if (typeof requestAnimationFrame === "undefined") {
    var requestAnimationFrame = function(cb) { return 0; };
    var cancelAnimationFrame = function() {};
}
if (typeof getComputedStyle === "undefined") {
    var getComputedStyle = function() { return {}; };
}
if (typeof CSS === "undefined") {
    var CSS = {supports: function() { return false; }};
}
if (typeof performance === "undefined") {
    var performance = {now: function() { return Date.now(); }, mark: function(){}, measure: function(){}};
}
if (typeof Intl === "undefined") {
    var _intlProto = {resolvedOptions:function(){return {timeZone:"UTC",locale:"en"};},format:function(){return "";}};
    var _intlCtor = function(){return Object.create(_intlProto);};
    _intlCtor.supportedLocalesOf = function(){return [];};
    var Intl = {DateTimeFormat:_intlCtor,NumberFormat:_intlCtor,PluralRules:_intlCtor,RelativeTimeFormat:_intlCtor,Collator:_intlCtor,ListFormat:_intlCtor,Segmenter:_intlCtor};
}
if (typeof queueMicrotask === "undefined") {
    var queueMicrotask = function(fn) { Promise.resolve().then(fn); };
}
if (typeof TextEncoder === "undefined") {
    var TextEncoder = function() { this.encode = function(s) { return new Uint8Array(0); }; };
}
if (typeof TextDecoder === "undefined") {
    var TextDecoder = function() { this.decode = function() { return ""; }; };
}

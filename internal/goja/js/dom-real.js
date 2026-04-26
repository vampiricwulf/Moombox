// dom-real.js — real-class DOM shim built incrementally for the Option 2
// path documented in docs/investigations/botguard-option-2-plan.md.
//
// Loaded by RegisterDOMShim AFTER the legacy test.49 hand-stub block, so
// the constructs declared here OVERRIDE the flat-object stubs while
// keeping the same global names (document / navigator / location / etc.).
// Existing cipher preprocessed-code-cache entries that reference
// `globalThis.document.querySelector` and friends keep resolving at the
// same names with compatible shapes; only the constructor / instanceof
// surface changes.
//
// Day 1 scope: EventTarget + Event + Event subclasses + dispatch with
// capture/bubble flags. Tree propagation across parent nodes is a Day-2
// extension once Node is in place — for Day 1, dispatch only fires
// listeners on the target itself.

(function() {
    'use strict';

    // Capture references to the legacy test.49 hand-stub classes BEFORE we
    // overwrite the globals. The legacy class hierarchy (Node, Document,
    // HTMLDocument, etc.) was already wired into document/navigator/etc.
    // via Object.setPrototypeOf in the test.49 IIFE; we preserve those
    // chains by linking each legacy prototype's [[Prototype]] to the
    // matching new class. Net effect: instanceof checks against EITHER
    // the old class (test.49 contract tests) OR the new class (Option-2
    // tests) succeed for the same object.
    const _legacyEventTarget = globalThis.EventTarget;
    const _legacyEvent = globalThis.Event;
    const _legacyCustomEvent = globalThis.CustomEvent;
    const _legacyMessageEvent = globalThis.MessageEvent;
    const _legacyErrorEvent = globalThis.ErrorEvent;

    function _bridgeProto(legacyCtor, newCtor) {
        if (!legacyCtor || legacyCtor === newCtor) return;
        try {
            Object.setPrototypeOf(legacyCtor.prototype, newCtor.prototype);
        } catch (e) { /* skip */ }
    }

    // -------------------------------------------------------------------
    // EventTarget — listener registry + dispatch.
    //
    // Every dispatch goes through three phases per the WHATWG spec:
    //   CAPTURE (1): root → ... → target.parent
    //   AT_TARGET (2): target
    //   BUBBLE (3): target.parent → ... → root  (only if event.bubbles)
    //
    // For Day 1 there's no parent chain (Node arrives Day 2), so dispatch
    // fires AT_TARGET listeners only. The capture/bubble flags are
    // recorded so the algorithm flips on cleanly when the tree exists.
    //
    // Listener options follow WHATWG: `capture`, `once`, `passive`,
    // `signal` (AbortSignal). `passive` is currently a no-op since we
    // don't dispatch any cancelable native events.
    // -------------------------------------------------------------------
    class EventTarget {
        constructor() {
            // Shape: Map<eventType, Array<ListenerEntry>>
            // ListenerEntry: { callback, capture, once, passive, signal, removed }
            // Each listener kept in registration order; capture vs. bubble
            // is a flag, not a separate list, so removal can match either.
            Object.defineProperty(this, '_listeners', {
                value: new Map(),
                enumerable: false,
                configurable: false,
                writable: false,
            });
        }

        addEventListener(type, callback, options) {
            if (callback == null) return;
            if (typeof callback !== 'function' && typeof callback !== 'object') return;
            type = String(type);
            const opts = _normalizeListenerOptions(options);
            // Already-aborted signal: noop per spec.
            if (opts.signal && opts.signal.aborted) return;

            let list = this._listeners.get(type);
            if (!list) {
                list = [];
                this._listeners.set(type, list);
            }
            // Spec: duplicates with same (callback, capture) are ignored.
            for (const entry of list) {
                if (entry.callback === callback && entry.capture === opts.capture) {
                    return;
                }
            }

            const entry = {
                callback: callback,
                capture: opts.capture,
                once: opts.once,
                passive: opts.passive,
                signal: opts.signal,
                removed: false,
            };
            list.push(entry);

            // Wire signal abort to remove. Real browsers walk the listener
            // registry to find entries whose signal aborted; we do the
            // same via a one-shot `abort` listener on the signal itself.
            if (opts.signal && typeof opts.signal.addEventListener === 'function') {
                const target = this;
                const onAbort = function() {
                    entry.removed = true;
                    _pruneListener(target._listeners, type, entry);
                };
                try {
                    opts.signal.addEventListener('abort', onAbort, { once: true });
                } catch (e) { /* signal might be a stub */ }
            }
        }

        removeEventListener(type, callback, options) {
            type = String(type);
            const list = this._listeners.get(type);
            if (!list) return;
            const capture = _normalizeListenerOptions(options).capture;
            for (let i = 0; i < list.length; i++) {
                const e = list[i];
                if (e.callback === callback && e.capture === capture) {
                    e.removed = true;
                    list.splice(i, 1);
                    return;
                }
            }
        }

        dispatchEvent(event) {
            if (!(event instanceof Event)) {
                throw new TypeError('dispatchEvent: argument must be an Event');
            }
            if (event._dispatched) {
                throw new Error('InvalidStateError: dispatchEvent on already-dispatched event');
            }
            event._dispatched = true;
            event._target = this;
            event._currentTarget = this;
            event._eventPhase = 2; // AT_TARGET (no parent chain in Day 1)

            const list = this._listeners.get(event.type);
            if (list) {
                // Snapshot the list so listeners added/removed during
                // dispatch don't perturb iteration. WHATWG spec is
                // explicit: only listeners present when dispatch starts
                // are invoked.
                const snapshot = list.slice();
                for (const entry of snapshot) {
                    if (entry.removed) continue;
                    if (event._stopImmediate) break;
                    try {
                        // `this` of the listener is the currentTarget per
                        // spec. The Function form binds it; the
                        // EventListener-object form has handleEvent.
                        if (typeof entry.callback === 'function') {
                            entry.callback.call(this, event);
                        } else if (typeof entry.callback.handleEvent === 'function') {
                            entry.callback.handleEvent(event);
                        }
                    } catch (e) {
                        // Real browsers report the error to window.onerror
                        // rather than letting it propagate. We swallow
                        // here too; future Day will route through a real
                        // ErrorEvent.
                    }
                    if (entry.once) {
                        entry.removed = true;
                        _pruneListener(this._listeners, event.type, entry);
                    }
                }
            }

            event._eventPhase = 0; // NONE
            event._currentTarget = null;
            return !event.defaultPrevented;
        }
    }

    function _normalizeListenerOptions(options) {
        if (options == null) return { capture: false, once: false, passive: false, signal: null };
        if (typeof options === 'boolean') return { capture: options, once: false, passive: false, signal: null };
        if (typeof options !== 'object') return { capture: false, once: false, passive: false, signal: null };
        return {
            capture: !!options.capture,
            once: !!options.once,
            passive: !!options.passive,
            signal: options.signal || null,
        };
    }

    function _pruneListener(map, type, entry) {
        const list = map.get(type);
        if (!list) return;
        const i = list.indexOf(entry);
        if (i >= 0) list.splice(i, 1);
    }

    Object.defineProperty(EventTarget.prototype, Symbol.toStringTag, {
        value: 'EventTarget',
        configurable: true,
    });

    // -------------------------------------------------------------------
    // Event — the WHATWG Event interface.
    //
    // Construction: new Event(type, init?). init has bubbles, cancelable,
    // composed (boolean defaults to false). Extra fields go on the
    // instance for subclasses to consume.
    // -------------------------------------------------------------------
    const NONE = 0;
    const CAPTURING_PHASE = 1;
    const AT_TARGET = 2;
    const BUBBLING_PHASE = 3;

    class Event {
        constructor(type, init) {
            init = init || {};
            this.type = String(type || '');
            this._target = null;
            this._currentTarget = null;
            this._eventPhase = NONE;
            this.bubbles = !!init.bubbles;
            this.cancelable = !!init.cancelable;
            this.composed = !!init.composed;
            this.defaultPrevented = false;
            this._stopPropagation = false;
            this._stopImmediate = false;
            this._dispatched = false;
            this.isTrusted = false;
            this.timeStamp = (typeof performance !== 'undefined' && performance.now) ? performance.now() : Date.now();
        }

        get target() { return this._target; }
        get currentTarget() { return this._currentTarget; }
        get eventPhase() { return this._eventPhase; }
        get srcElement() { return this._target; }    // legacy alias
        get returnValue() { return !this.defaultPrevented; } // legacy
        set returnValue(v) { if (!v) this.defaultPrevented = true; }

        preventDefault() {
            if (this.cancelable) this.defaultPrevented = true;
        }
        stopPropagation() {
            this._stopPropagation = true;
        }
        stopImmediatePropagation() {
            this._stopPropagation = true;
            this._stopImmediate = true;
        }

        composedPath() {
            if (!this._target) return [];
            return [this._target]; // Day 2 will walk the parent chain
        }
    }

    // Phase constants exposed on the constructor (per spec).
    Object.defineProperty(Event, 'NONE', { value: NONE });
    Object.defineProperty(Event, 'CAPTURING_PHASE', { value: CAPTURING_PHASE });
    Object.defineProperty(Event, 'AT_TARGET', { value: AT_TARGET });
    Object.defineProperty(Event, 'BUBBLING_PHASE', { value: BUBBLING_PHASE });
    Object.defineProperty(Event.prototype, 'NONE', { value: NONE });
    Object.defineProperty(Event.prototype, 'CAPTURING_PHASE', { value: CAPTURING_PHASE });
    Object.defineProperty(Event.prototype, 'AT_TARGET', { value: AT_TARGET });
    Object.defineProperty(Event.prototype, 'BUBBLING_PHASE', { value: BUBBLING_PHASE });
    Object.defineProperty(Event.prototype, Symbol.toStringTag, {
        value: 'Event',
        configurable: true,
    });

    // CustomEvent extending Event — re-exposed so test.49's hand stub
    // is replaced by a properly-classed version that participates in
    // instanceof chains.
    class CustomEvent extends Event {
        constructor(type, init) {
            super(type, init);
            this.detail = (init && init.detail !== undefined) ? init.detail : null;
        }
    }
    Object.defineProperty(CustomEvent.prototype, Symbol.toStringTag, { value: 'CustomEvent', configurable: true });

    class MessageEvent extends Event {
        constructor(type, init) {
            super(type, init);
            init = init || {};
            this.data = init.data;
            this.origin = init.origin || '';
            this.lastEventId = init.lastEventId || '';
            this.source = init.source || null;
            this.ports = init.ports || [];
        }
    }
    Object.defineProperty(MessageEvent.prototype, Symbol.toStringTag, { value: 'MessageEvent', configurable: true });

    class ErrorEvent extends Event {
        constructor(type, init) {
            super(type, init);
            init = init || {};
            this.message = init.message || '';
            this.filename = init.filename || '';
            this.lineno = init.lineno || 0;
            this.colno = init.colno || 0;
            this.error = init.error || null;
        }
    }
    Object.defineProperty(ErrorEvent.prototype, Symbol.toStringTag, { value: 'ErrorEvent', configurable: true });

    // Bridge legacy classes -> new classes. The test.49 hierarchy
    // (Node -> EventTarget) had document chained at Node.prototype ->
    // _legacyEventTarget.prototype; bridging _legacyEventTarget.prototype
    // to the new EventTarget.prototype keeps `document instanceof
    // _legacyEventTarget` true AND makes `document instanceof
    // EventTarget(new)` true at the same time.
    _bridgeProto(_legacyEventTarget, EventTarget);
    _bridgeProto(_legacyEvent, Event);
    _bridgeProto(_legacyCustomEvent, CustomEvent);
    _bridgeProto(_legacyMessageEvent, MessageEvent);
    _bridgeProto(_legacyErrorEvent, ErrorEvent);

    // Expose globally. These overwrite the test.49 hand-stub constructors
    // (function-based with prototype rewrites). The classes here have a
    // proper inheritance chain that survives instanceof checks.
    globalThis.EventTarget = EventTarget;
    globalThis.Event = Event;
    globalThis.CustomEvent = CustomEvent;
    globalThis.MessageEvent = MessageEvent;
    globalThis.ErrorEvent = ErrorEvent;
})();

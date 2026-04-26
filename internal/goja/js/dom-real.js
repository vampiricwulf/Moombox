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

    // -------------------------------------------------------------------
    // Day 2: Node tree + Element + HTMLElement.
    //
    // Node is the foundation of the DOM tree. Children are stored in a
    // simple Array; sibling/parent links are computed via index when
    // requested. Operations mutate both sides of the parent/child
    // relationship atomically.
    // -------------------------------------------------------------------
    const NODE_TYPE_ELEMENT = 1;
    const NODE_TYPE_TEXT = 3;
    const NODE_TYPE_COMMENT = 8;
    const NODE_TYPE_DOCUMENT = 9;
    const NODE_TYPE_DOCUMENT_FRAGMENT = 11;

    class Node extends EventTarget {
        constructor() {
            super();
            Object.defineProperty(this, '_parent', { value: null, writable: true, enumerable: false, configurable: true });
            Object.defineProperty(this, '_children', { value: [], writable: true, enumerable: false, configurable: true });
            Object.defineProperty(this, '_nodeValue', { value: null, writable: true, enumerable: false, configurable: true });
            Object.defineProperty(this, '_ownerDocument', { value: null, writable: true, enumerable: false, configurable: true });
        }

        get nodeType() { return NODE_TYPE_ELEMENT; } // overridden by Text/Comment/Document
        get nodeName() { return ''; }
        get nodeValue() { return this._nodeValue; }
        set nodeValue(v) { this._nodeValue = v == null ? null : String(v); }

        get parentNode() { return this._parent; }
        get parentElement() {
            return (this._parent && this._parent.nodeType === NODE_TYPE_ELEMENT) ? this._parent : null;
        }
        get childNodes() { return this._children.slice(); } // live list optional; copy for safety
        get firstChild() { return this._children[0] || null; }
        get lastChild() { return this._children[this._children.length - 1] || null; }
        get previousSibling() {
            if (!this._parent) return null;
            const i = this._parent._children.indexOf(this);
            return i > 0 ? this._parent._children[i - 1] : null;
        }
        get nextSibling() {
            if (!this._parent) return null;
            const i = this._parent._children.indexOf(this);
            return (i >= 0 && i + 1 < this._parent._children.length) ? this._parent._children[i + 1] : null;
        }
        get ownerDocument() { return this._ownerDocument; }

        hasChildNodes() { return this._children.length > 0; }

        appendChild(node) {
            if (!(node instanceof Node)) {
                throw new TypeError('appendChild: argument must be a Node');
            }
            if (node === this) {
                throw new Error('HierarchyRequestError: cannot append node to itself');
            }
            if (node._parent) node._parent.removeChild(node);
            node._parent = this;
            // Inherit ownerDocument from the new parent so query-by-id etc. work.
            if (this._ownerDocument || (this.nodeType === NODE_TYPE_DOCUMENT)) {
                node._ownerDocument = (this.nodeType === NODE_TYPE_DOCUMENT) ? this : this._ownerDocument;
                _propagateOwnerDocument(node);
            }
            this._children.push(node);
            return node;
        }

        removeChild(node) {
            const i = this._children.indexOf(node);
            if (i < 0) {
                throw new Error('NotFoundError: removeChild called for node that is not a child');
            }
            this._children.splice(i, 1);
            node._parent = null;
            return node;
        }

        insertBefore(node, ref) {
            if (!(node instanceof Node)) {
                throw new TypeError('insertBefore: argument must be a Node');
            }
            if (ref == null) return this.appendChild(node);
            const i = this._children.indexOf(ref);
            if (i < 0) {
                throw new Error('NotFoundError: insertBefore reference is not a child');
            }
            if (node._parent) node._parent.removeChild(node);
            node._parent = this;
            if (this._ownerDocument || (this.nodeType === NODE_TYPE_DOCUMENT)) {
                node._ownerDocument = (this.nodeType === NODE_TYPE_DOCUMENT) ? this : this._ownerDocument;
                _propagateOwnerDocument(node);
            }
            this._children.splice(i, 0, node);
            return node;
        }

        replaceChild(newNode, oldNode) {
            const i = this._children.indexOf(oldNode);
            if (i < 0) {
                throw new Error('NotFoundError: replaceChild oldChild is not a child');
            }
            if (newNode._parent) newNode._parent.removeChild(newNode);
            newNode._parent = this;
            if (this._ownerDocument || (this.nodeType === NODE_TYPE_DOCUMENT)) {
                newNode._ownerDocument = (this.nodeType === NODE_TYPE_DOCUMENT) ? this : this._ownerDocument;
                _propagateOwnerDocument(newNode);
            }
            this._children[i] = newNode;
            oldNode._parent = null;
            return oldNode;
        }

        contains(node) {
            let n = node;
            while (n) {
                if (n === this) return true;
                n = n._parent;
            }
            return false;
        }

        cloneNode(deep) {
            // Minimal: returns a new Node of the same kind. Subclasses
            // override to preserve attributes / data.
            const clone = new this.constructor();
            if (deep) {
                for (const c of this._children) {
                    clone.appendChild(c.cloneNode(true));
                }
            }
            return clone;
        }

        get textContent() {
            if (this.nodeType === NODE_TYPE_TEXT || this.nodeType === NODE_TYPE_COMMENT) {
                return this._nodeValue || '';
            }
            let s = '';
            for (const c of this._children) {
                if (c.nodeType !== NODE_TYPE_COMMENT) s += c.textContent;
            }
            return s;
        }
        set textContent(v) {
            this._children = [];
            if (v != null && v !== '') {
                const t = new Text(String(v));
                t._parent = this;
                t._ownerDocument = this._ownerDocument || (this.nodeType === NODE_TYPE_DOCUMENT ? this : null);
                this._children.push(t);
            }
        }
    }
    Object.defineProperty(Node.prototype, Symbol.toStringTag, { value: 'Node', configurable: true });
    Object.defineProperty(Node, 'ELEMENT_NODE', { value: NODE_TYPE_ELEMENT });
    Object.defineProperty(Node, 'TEXT_NODE', { value: NODE_TYPE_TEXT });
    Object.defineProperty(Node, 'COMMENT_NODE', { value: NODE_TYPE_COMMENT });
    Object.defineProperty(Node, 'DOCUMENT_NODE', { value: NODE_TYPE_DOCUMENT });
    Object.defineProperty(Node, 'DOCUMENT_FRAGMENT_NODE', { value: NODE_TYPE_DOCUMENT_FRAGMENT });

    function _propagateOwnerDocument(node) {
        for (const c of node._children) {
            c._ownerDocument = node._ownerDocument;
            _propagateOwnerDocument(c);
        }
    }

    // -------------------------------------------------------------------
    // Text + Comment + DocumentFragment node types
    // -------------------------------------------------------------------
    class Text extends Node {
        constructor(data) {
            super();
            this._nodeValue = data == null ? '' : String(data);
        }
        get nodeType() { return NODE_TYPE_TEXT; }
        get nodeName() { return '#text'; }
        get data() { return this._nodeValue; }
        set data(v) { this._nodeValue = v == null ? '' : String(v); }
        get length() { return this._nodeValue.length; }
    }
    Object.defineProperty(Text.prototype, Symbol.toStringTag, { value: 'Text', configurable: true });

    class Comment extends Node {
        constructor(data) {
            super();
            this._nodeValue = data == null ? '' : String(data);
        }
        get nodeType() { return NODE_TYPE_COMMENT; }
        get nodeName() { return '#comment'; }
        get data() { return this._nodeValue; }
        set data(v) { this._nodeValue = v == null ? '' : String(v); }
    }
    Object.defineProperty(Comment.prototype, Symbol.toStringTag, { value: 'Comment', configurable: true });

    class DocumentFragment extends Node {
        get nodeType() { return NODE_TYPE_DOCUMENT_FRAGMENT; }
        get nodeName() { return '#document-fragment'; }
    }
    Object.defineProperty(DocumentFragment.prototype, Symbol.toStringTag, { value: 'DocumentFragment', configurable: true });

    // -------------------------------------------------------------------
    // Element + HTMLElement
    // -------------------------------------------------------------------
    class Element extends Node {
        constructor(tagName) {
            super();
            Object.defineProperty(this, '_tagName', { value: String(tagName || '').toUpperCase(), writable: false, enumerable: false, configurable: true });
            Object.defineProperty(this, '_attributes', { value: Object.create(null), writable: false, enumerable: false, configurable: true });
        }

        get nodeType() { return NODE_TYPE_ELEMENT; }
        get nodeName() { return this._tagName; }
        get tagName() { return this._tagName; }
        get localName() { return this._tagName.toLowerCase(); }

        get id() { return this._attributes.id || ''; }
        set id(v) { this._attributes.id = v == null ? '' : String(v); }

        get className() { return this._attributes['class'] || ''; }
        set className(v) { this._attributes['class'] = v == null ? '' : String(v); }

        getAttribute(name) {
            const v = this._attributes[String(name).toLowerCase()];
            return v == null ? null : v;
        }
        setAttribute(name, value) {
            this._attributes[String(name).toLowerCase()] = String(value);
        }
        removeAttribute(name) {
            delete this._attributes[String(name).toLowerCase()];
        }
        hasAttribute(name) {
            return Object.prototype.hasOwnProperty.call(this._attributes, String(name).toLowerCase());
        }
        getAttributeNames() {
            return Object.keys(this._attributes);
        }

        // Stub (no real selector parser yet — Day 4)
        querySelector(_sel) { return null; }
        querySelectorAll(_sel) { return []; }
        getElementsByTagName(tag) {
            const upper = String(tag).toUpperCase();
            const out = [];
            const walk = (n) => {
                for (const c of n._children) {
                    if (c.nodeType === NODE_TYPE_ELEMENT && (upper === '*' || c.tagName === upper)) out.push(c);
                    walk(c);
                }
            };
            walk(this);
            return out;
        }
        getElementsByClassName(name) {
            const want = String(name).split(/\s+/).filter(Boolean);
            const out = [];
            const walk = (n) => {
                for (const c of n._children) {
                    if (c.nodeType === NODE_TYPE_ELEMENT) {
                        const cls = (c._attributes['class'] || '').split(/\s+/);
                        if (want.every(w => cls.includes(w))) out.push(c);
                    }
                    walk(c);
                }
            };
            walk(this);
            return out;
        }

        // String-only innerHTML; serialises children for getter, no real
        // parser for setter (just creates a single Text node holding the
        // string verbatim — sufficient for the BotGuard fingerprint
        // surface, which never reads parsed inner-HTML structure back).
        get innerHTML() {
            return this._children.map(c => _serialiseNode(c)).join('');
        }
        set innerHTML(html) {
            this._children = [];
            if (html != null && html !== '') {
                const t = new Text(String(html));
                t._parent = this;
                t._ownerDocument = this._ownerDocument;
                this._children.push(t);
            }
        }
        get outerHTML() { return _serialiseNode(this); }

        // Bounding-rect / getBoundingClientRect — return zeroed values
        // since we have no layout. Real browsers' all-zeros result for
        // a detached element is identical, so this matches at least one
        // valid case.
        getBoundingClientRect() {
            return { x: 0, y: 0, top: 0, left: 0, right: 0, bottom: 0, width: 0, height: 0 };
        }
        getClientRects() { return [this.getBoundingClientRect()]; }
    }
    Object.defineProperty(Element.prototype, Symbol.toStringTag, { value: 'Element', configurable: true });

    function _serialiseNode(n) {
        if (n.nodeType === NODE_TYPE_TEXT) return n._nodeValue || '';
        if (n.nodeType === NODE_TYPE_COMMENT) return '<!--' + (n._nodeValue || '') + '-->';
        if (n.nodeType === NODE_TYPE_ELEMENT) {
            const tag = n._tagName.toLowerCase();
            let s = '<' + tag;
            for (const k of Object.keys(n._attributes)) {
                s += ' ' + k + '="' + String(n._attributes[k]).replace(/"/g, '&quot;') + '"';
            }
            s += '>';
            for (const c of n._children) s += _serialiseNode(c);
            s += '</' + tag + '>';
            return s;
        }
        return '';
    }

    class HTMLElement extends Element {
        constructor(tagName) {
            super(tagName);
            Object.defineProperty(this, '_dataset', { value: Object.create(null), writable: true, enumerable: false, configurable: true });
        }

        get hidden() { return this._attributes.hidden != null; }
        set hidden(v) { if (v) this._attributes.hidden = ''; else delete this._attributes.hidden; }

        get tabIndex() {
            const v = this._attributes.tabindex;
            return v == null ? -1 : parseInt(v, 10) || 0;
        }
        set tabIndex(v) { this._attributes.tabindex = String(v); }

        get title() { return this._attributes.title || ''; }
        set title(v) { this._attributes.title = String(v); }

        get lang() { return this._attributes.lang || ''; }
        set lang(v) { this._attributes.lang = String(v); }

        get dataset() { return this._dataset; }

        focus() {}
        blur() {}
        click() {
            // Real browsers dispatch a click event; stub fires a basic
            // Event so any registered listeners still see the call.
            this.dispatchEvent(new Event('click', { bubbles: true, cancelable: true }));
        }
    }
    Object.defineProperty(HTMLElement.prototype, Symbol.toStringTag, { value: 'HTMLElement', configurable: true });

    // -------------------------------------------------------------------
    // Specific HTML element subclasses (for instanceof checks)
    // -------------------------------------------------------------------
    function _makeHTMLSubclass(name, tagDefault) {
        const ctor = class extends HTMLElement {
            constructor(tagName) {
                super(tagName || tagDefault);
            }
        };
        Object.defineProperty(ctor, 'name', { value: name, configurable: true });
        Object.defineProperty(ctor.prototype, Symbol.toStringTag, { value: name, configurable: true });
        return ctor;
    }
    const HTMLHtmlElement     = _makeHTMLSubclass('HTMLHtmlElement', 'html');
    const HTMLHeadElement     = _makeHTMLSubclass('HTMLHeadElement', 'head');
    const HTMLBodyElement     = _makeHTMLSubclass('HTMLBodyElement', 'body');
    const HTMLDivElement      = _makeHTMLSubclass('HTMLDivElement', 'div');
    const HTMLSpanElement     = _makeHTMLSubclass('HTMLSpanElement', 'span');
    const HTMLParagraphElement= _makeHTMLSubclass('HTMLParagraphElement', 'p');
    const HTMLHeadingElement  = _makeHTMLSubclass('HTMLHeadingElement', 'h1');
    const HTMLImageElement    = _makeHTMLSubclass('HTMLImageElement', 'img');
    const HTMLScriptElement   = _makeHTMLSubclass('HTMLScriptElement', 'script');
    const HTMLLinkElement     = _makeHTMLSubclass('HTMLLinkElement', 'link');
    const HTMLStyleElement    = _makeHTMLSubclass('HTMLStyleElement', 'style');
    const HTMLMetaElement     = _makeHTMLSubclass('HTMLMetaElement', 'meta');
    const HTMLTitleElement    = _makeHTMLSubclass('HTMLTitleElement', 'title');
    const HTMLAnchorElement   = _makeHTMLSubclass('HTMLAnchorElement', 'a');
    const HTMLInputElement    = _makeHTMLSubclass('HTMLInputElement', 'input');
    const HTMLButtonElement   = _makeHTMLSubclass('HTMLButtonElement', 'button');
    const HTMLFormElement     = _makeHTMLSubclass('HTMLFormElement', 'form');
    const HTMLCanvasElement   = _makeHTMLSubclass('HTMLCanvasElement', 'canvas');
    const HTMLVideoElement    = _makeHTMLSubclass('HTMLVideoElement', 'video');
    const HTMLAudioElement    = _makeHTMLSubclass('HTMLAudioElement', 'audio');
    const HTMLUListElement    = _makeHTMLSubclass('HTMLUListElement', 'ul');
    const HTMLOListElement    = _makeHTMLSubclass('HTMLOListElement', 'ol');
    const HTMLLIElement       = _makeHTMLSubclass('HTMLLIElement', 'li');
    const HTMLTableElement    = _makeHTMLSubclass('HTMLTableElement', 'table');
    const HTMLIFrameElement   = _makeHTMLSubclass('HTMLIFrameElement', 'iframe');
    const HTMLUnknownElement  = _makeHTMLSubclass('HTMLUnknownElement', 'unknown');

    // Tag-name → subclass map for createElement.
    const _htmlTagMap = {
        html: HTMLHtmlElement, head: HTMLHeadElement, body: HTMLBodyElement,
        div: HTMLDivElement, span: HTMLSpanElement, p: HTMLParagraphElement,
        h1: HTMLHeadingElement, h2: HTMLHeadingElement, h3: HTMLHeadingElement,
        h4: HTMLHeadingElement, h5: HTMLHeadingElement, h6: HTMLHeadingElement,
        img: HTMLImageElement, script: HTMLScriptElement, link: HTMLLinkElement,
        style: HTMLStyleElement, meta: HTMLMetaElement, title: HTMLTitleElement,
        a: HTMLAnchorElement, input: HTMLInputElement, button: HTMLButtonElement,
        form: HTMLFormElement, canvas: HTMLCanvasElement, video: HTMLVideoElement,
        audio: HTMLAudioElement, ul: HTMLUListElement, ol: HTMLOListElement,
        li: HTMLLIElement, table: HTMLTableElement, iframe: HTMLIFrameElement,
    };

    // -------------------------------------------------------------------
    // Wire dispatchEvent to walk the parent chain (capture/target/bubble)
    // -------------------------------------------------------------------
    const _flatDispatch = EventTarget.prototype.dispatchEvent;
    EventTarget.prototype.dispatchEvent = function(event) {
        if (!(this instanceof Node) || !this._parent) {
            // No tree -- single-target dispatch.
            return _flatDispatch.call(this, event);
        }
        if (!(event instanceof Event)) {
            throw new TypeError('dispatchEvent: argument must be an Event');
        }
        if (event._dispatched) {
            throw new Error('InvalidStateError: dispatchEvent on already-dispatched event');
        }
        event._dispatched = true;
        event._target = this;

        // Build path: target -> ... -> root
        const path = [];
        for (let n = this; n; n = n._parent) path.push(n);
        event._path = path.slice();

        // Capture phase: root -> target.parent
        event._eventPhase = CAPTURING_PHASE;
        for (let i = path.length - 1; i > 0 && !event._stopPropagation; i--) {
            _runPhaseListeners(path[i], event, true);
        }

        // Target phase
        if (!event._stopPropagation) {
            event._eventPhase = AT_TARGET;
            _runPhaseListeners(this, event, null); // null = run both capture+bubble entries
        }

        // Bubble phase: target.parent -> root
        if (!event._stopPropagation && event.bubbles) {
            event._eventPhase = BUBBLING_PHASE;
            for (let i = 1; i < path.length && !event._stopPropagation; i++) {
                _runPhaseListeners(path[i], event, false);
            }
        }

        event._eventPhase = NONE;
        event._currentTarget = null;
        return !event.defaultPrevented;
    };

    function _runPhaseListeners(target, event, captureFilter) {
        event._currentTarget = target;
        const list = target._listeners ? target._listeners.get(event.type) : null;
        if (!list) return;
        const snapshot = list.slice();
        for (const entry of snapshot) {
            if (entry.removed) continue;
            // captureFilter: true = only capture entries; false = only bubble entries; null = either
            if (captureFilter !== null && entry.capture !== captureFilter) continue;
            if (event._stopImmediate) break;
            try {
                if (typeof entry.callback === 'function') entry.callback.call(target, event);
                else if (typeof entry.callback.handleEvent === 'function') entry.callback.handleEvent(event);
            } catch (e) { /* swallow */ }
            if (entry.once) {
                entry.removed = true;
                const idx = list.indexOf(entry);
                if (idx >= 0) list.splice(idx, 1);
            }
        }
    }

    // composedPath now walks the parent chain.
    Event.prototype.composedPath = function() {
        return this._path ? this._path.slice() : (this._target ? [this._target] : []);
    };

    // Bridge legacy classes -> new classes. The test.49 hierarchy
    // (Node -> EventTarget) had document chained at Node.prototype ->
    // _legacyEventTarget.prototype; bridging the legacy chain to the new
    // hierarchy keeps `document instanceof _legacy*` true AND makes
    // `document instanceof <new>` true at the same time.
    _bridgeProto(_legacyEventTarget, EventTarget);
    _bridgeProto(_legacyEvent, Event);
    _bridgeProto(_legacyCustomEvent, CustomEvent);
    _bridgeProto(_legacyMessageEvent, MessageEvent);
    _bridgeProto(_legacyErrorEvent, ErrorEvent);
    _bridgeProto(globalThis.Node, Node);                // legacy Node from test.49 IIFE
    _bridgeProto(globalThis.Element, Element);
    _bridgeProto(globalThis.HTMLElement, HTMLElement);

    // Expose globally. These overwrite the test.49 hand-stub constructors
    // (function-based with prototype rewrites). The classes here have a
    // proper inheritance chain that survives instanceof checks.
    globalThis.EventTarget = EventTarget;
    globalThis.Event = Event;
    globalThis.CustomEvent = CustomEvent;
    globalThis.MessageEvent = MessageEvent;
    globalThis.ErrorEvent = ErrorEvent;
    globalThis.Node = Node;
    globalThis.Text = Text;
    globalThis.Comment = Comment;
    globalThis.DocumentFragment = DocumentFragment;
    globalThis.Element = Element;
    globalThis.HTMLElement = HTMLElement;
    globalThis.HTMLHtmlElement = HTMLHtmlElement;
    globalThis.HTMLHeadElement = HTMLHeadElement;
    globalThis.HTMLBodyElement = HTMLBodyElement;
    globalThis.HTMLDivElement = HTMLDivElement;
    globalThis.HTMLSpanElement = HTMLSpanElement;
    globalThis.HTMLParagraphElement = HTMLParagraphElement;
    globalThis.HTMLHeadingElement = HTMLHeadingElement;
    globalThis.HTMLImageElement = HTMLImageElement;
    globalThis.HTMLScriptElement = HTMLScriptElement;
    globalThis.HTMLLinkElement = HTMLLinkElement;
    globalThis.HTMLStyleElement = HTMLStyleElement;
    globalThis.HTMLMetaElement = HTMLMetaElement;
    globalThis.HTMLTitleElement = HTMLTitleElement;
    globalThis.HTMLAnchorElement = HTMLAnchorElement;
    globalThis.HTMLInputElement = HTMLInputElement;
    globalThis.HTMLButtonElement = HTMLButtonElement;
    globalThis.HTMLFormElement = HTMLFormElement;
    globalThis.HTMLCanvasElement = HTMLCanvasElement;
    globalThis.HTMLVideoElement = HTMLVideoElement;
    globalThis.HTMLAudioElement = HTMLAudioElement;
    globalThis.HTMLUListElement = HTMLUListElement;
    globalThis.HTMLOListElement = HTMLOListElement;
    globalThis.HTMLLIElement = HTMLLIElement;
    globalThis.HTMLTableElement = HTMLTableElement;
    globalThis.HTMLIFrameElement = HTMLIFrameElement;
    globalThis.HTMLUnknownElement = HTMLUnknownElement;

    // Expose the tag map for createElement (Day 4 will use this).
    Object.defineProperty(globalThis, '__moombox_htmlTagMap', { value: _htmlTagMap, configurable: true });
})();

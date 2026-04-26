package goja

import (
	crand "crypto/rand"
	"fmt"

	"github.com/dop251/goja"
)

// RegisterDOMShim registers minimal DOM stubs on a Goja runtime.
// This provides just enough browser-like environment for BotGuard and cipher functions.
func RegisterDOMShim(vm *goja.Runtime, userAgent string) error {
	if userAgent == "" {
		userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	}

	// CSPRNG byte source for crypto.getRandomValues (replaces the
	// Math.random()-based JS implementation). Browser spec caps the
	// requested length at 65536; mirror that to guard against a script
	// allocating an arbitrary-sized slice.
	if err := vm.Set("__cryptoRandBytes", func(call goja.FunctionCall) goja.Value {
		n := int(call.Argument(0).ToInteger())
		if n <= 0 {
			return vm.ToValue([]byte{})
		}
		if n > 65536 {
			panic(vm.NewGoError(fmt.Errorf("getRandomValues length exceeds 65536: %d", n)))
		}
		buf := make([]byte, n)
		if _, err := crand.Read(buf); err != nil {
			panic(vm.NewGoError(fmt.Errorf("crypto/rand.Read failed: %w", err)))
		}
		return vm.ToValue(buf)
	}); err != nil {
		return fmt.Errorf("register __cryptoRandBytes: %w", err)
	}

	// Register randomUUID as a Go native using CSPRNG. On entropy failure
	// panic into a JS exception rather than silently returning an all-zeros
	// (i.e., predictable) UUID — BotGuard's security assumptions hinge on
	// actual randomness here.
	if err := vm.Set("__cryptoRandomUUID", func(call goja.FunctionCall) goja.Value {
		b := make([]byte, 16)
		if _, err := crand.Read(b); err != nil {
			panic(vm.NewGoError(fmt.Errorf("crypto/rand.Read for UUID failed: %w", err)))
		}
		b[6] = (b[6] & 0x0f) | 0x40 // version 4
		b[8] = (b[8] & 0x3f) | 0x80 // variant 1
		return vm.ToValue(fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]))
	}); err != nil {
		return fmt.Errorf("register __cryptoRandomUUID: %w", err)
	}

	// Pass userAgent via Go to avoid JS template literal injection.
	if err := vm.Set("__moomboxUserAgent", userAgent); err != nil {
		return fmt.Errorf("register __moomboxUserAgent: %w", err)
	}

	shimCode := `
(function() {
	// Element stub
	function createElement(tag) {
		var el = {
			tagName: (tag || '').toUpperCase(),
			children: [],
			attributes: {},
			style: {},
			innerHTML: '',
			innerText: '',
			textContent: '',
			appendChild: function(child) { this.children.push(child); return child; },
			removeChild: function(child) {
				var idx = this.children.indexOf(child);
				if (idx >= 0) this.children.splice(idx, 1);
				return child;
			},
			setAttribute: function(k, v) { this.attributes[k] = v; },
			getAttribute: function(k) { return this.attributes[k] || null; },
			removeAttribute: function(k) { delete this.attributes[k]; },
			addEventListener: function() {},
			removeEventListener: function() {},
			dispatchEvent: function() { return true; },
			querySelector: function() { return null; },
			querySelectorAll: function() { return []; },
			getElementsByTagName: function() { return []; },
			getElementsByClassName: function() { return []; },
			getElementById: function() { return null; },
			cloneNode: function() { return createElement(tag); },
			getBoundingClientRect: function() { return { top: 0, left: 0, bottom: 0, right: 0, width: 0, height: 0 }; },
			focus: function() {},
			blur: function() {},
			click: function() {},
			parentNode: null,
			parentElement: null,
			nextSibling: null,
			previousSibling: null,
			firstChild: null,
			lastChild: null,
			ownerDocument: null,
			nodeType: 1,
			nodeName: (tag || '').toUpperCase()
		};
		// Canvas stub
		if (tag === 'canvas') {
			el.getContext = function() {
				return {
					fillRect: function() {},
					clearRect: function() {},
					getImageData: function(x, y, w, h) { return { data: new Uint8Array(w * h * 4) }; },
					putImageData: function() {},
					createImageData: function() { return []; },
					setTransform: function() {},
					drawImage: function() {},
					save: function() {},
					fillText: function() {},
					restore: function() {},
					beginPath: function() {},
					moveTo: function() {},
					lineTo: function() {},
					closePath: function() {},
					stroke: function() {},
					translate: function() {},
					scale: function() {},
					rotate: function() {},
					arc: function() {},
					fill: function() {},
					measureText: function() { return { width: 0 }; },
					transform: function() {},
					rect: function() {},
					clip: function() {},
					canvas: el
				};
			};
			el.toDataURL = function() { return ''; };
		}
		return el;
	}

	// Document
	var bodyEl = createElement('body');
	var headEl = createElement('head');
	var docEl = createElement('html');
	docEl.children = [headEl, bodyEl];

	var document = {
		createElement: createElement,
		createElementNS: function(ns, tag) { return createElement(tag); },
		createTextNode: function(text) { return { nodeType: 3, textContent: text }; },
		createDocumentFragment: function() {
			return { nodeType: 11, children: [], appendChild: function(c) { this.children.push(c); return c; } };
		},
		createEvent: function() { return { initEvent: function() {} }; },
		body: bodyEl,
		head: headEl,
		documentElement: docEl,
		getElementById: function() { return null; },
		getElementsByTagName: function(tag) {
			if (tag === 'head') return [headEl];
			if (tag === 'body') return [bodyEl];
			return [];
		},
		getElementsByClassName: function() { return []; },
		querySelector: function() { return null; },
		querySelectorAll: function() { return []; },
		addEventListener: function() {},
		removeEventListener: function() {},
		dispatchEvent: function() { return true; },
		cookie: '',
		title: '',
		readyState: 'complete',
		hidden: false,
		visibilityState: 'visible',
		location: {
			hash: '',
			host: 'www.youtube.com',
			hostname: 'www.youtube.com',
			href: 'https://www.youtube.com/',
			origin: 'https://www.youtube.com',
			pathname: '/',
			port: '',
			protocol: 'https:',
			search: '',
			assign: function() {},
			reload: function() {},
			replace: function() {}
		},
		referrer: '',
		domain: 'www.youtube.com',
		URL: 'https://www.youtube.com/',
		characterSet: 'UTF-8',
		contentType: 'text/html',
		nodeType: 9
	};

	// Navigator
	var navigator = {
		userAgent: __moomboxUserAgent,
		language: 'en-US',
		languages: ['en-US', 'en'],
		platform: 'Win32',
		appName: 'Netscape',
		appVersion: '5.0',
		vendor: 'Google Inc.',
		onLine: true,
		cookieEnabled: true,
		doNotTrack: null,
		hardwareConcurrency: 8,
		maxTouchPoints: 0,
		mediaDevices: { enumerateDevices: function() { return Promise.resolve([]); } },
		permissions: { query: function() { return Promise.resolve({ state: 'prompt' }); } },
		clipboard: {},
		geolocation: {},
		serviceWorker: { ready: Promise.resolve(null), register: function() { return Promise.resolve(null); } },
		storage: { estimate: function() { return Promise.resolve({ usage: 0, quota: 0 }); } },
		sendBeacon: function() { return true; }
	};

	// Location
	var location = document.location;

	// Window / globalThis stubs
	globalThis.document = document;
	globalThis.navigator = navigator;
	globalThis.location = location;
	globalThis.window = globalThis;
	globalThis.self = globalThis;
	globalThis.top = globalThis;
	globalThis.parent = globalThis;
	globalThis.frames = globalThis;

	// Screen stub
	globalThis.screen = { width: 1920, height: 1080, availWidth: 1920, availHeight: 1040, colorDepth: 24, pixelDepth: 24, orientation: { type: 'landscape-primary' } };
	globalThis.innerWidth = 1920;
	globalThis.innerHeight = 1080;
	globalThis.outerWidth = 1920;
	globalThis.outerHeight = 1080;
	globalThis.devicePixelRatio = 1;
	globalThis.screenX = 0;
	globalThis.screenY = 0;
	globalThis.scrollX = 0;
	globalThis.scrollY = 0;
	globalThis.pageXOffset = 0;
	globalThis.pageYOffset = 0;

	// Performance stub
	var perfStart = Date.now();
	globalThis.performance = {
		now: function() { return Date.now() - perfStart; },
		timing: { navigationStart: perfStart },
		getEntriesByType: function() { return []; },
		getEntriesByName: function() { return []; },
		mark: function() {},
		measure: function() {},
		clearMarks: function() {},
		clearMeasures: function() {}
	};

	// Storage stubs
	function makeStorage() {
		var data = {};
		return {
			getItem: function(k) { return data[k] !== undefined ? data[k] : null; },
			setItem: function(k, v) { data[k] = String(v); },
			removeItem: function(k) { delete data[k]; },
			clear: function() { data = {}; },
			get length() { return Object.keys(data).length; },
			key: function(i) { var keys = Object.keys(data); return keys[i] || null; }
		};
	}
	globalThis.localStorage = makeStorage();
	globalThis.sessionStorage = makeStorage();

	// XMLHttpRequest stub
	globalThis.XMLHttpRequest = function() {
		this.readyState = 0;
		this.status = 0;
		this.responseText = '';
		this.open = function() {};
		this.send = function() {};
		this.setRequestHeader = function() {};
		this.getResponseHeader = function() { return null; };
		this.abort = function() {};
	};
	globalThis.XMLHttpRequest.prototype = {};

	// Misc stubs
	globalThis.requestAnimationFrame = function(cb) { return globalThis.setTimeout(cb, 16); };
	globalThis.cancelAnimationFrame = function(id) { globalThis.clearTimeout(id); };
	globalThis.getComputedStyle = function() { return {}; };
	globalThis.matchMedia = function() { return { matches: false, addListener: function() {}, removeListener: function() {} }; };
	globalThis.fetch = function() { return Promise.reject(new Error('fetch not available')); };
	globalThis.MutationObserver = function() { this.observe = function() {}; this.disconnect = function() {}; this.takeRecords = function() { return []; }; };
	globalThis.IntersectionObserver = function() { this.observe = function() {}; this.disconnect = function() {}; };
	globalThis.ResizeObserver = function() { this.observe = function() {}; this.disconnect = function() {}; };
	// AbortController / ReadableStream / CustomEvent / CSS / Intl moved
	// out (test.42) -- these were briefly added in Phase 5 (test.34) so
	// cipher could consolidate on the shared dom_shim. The trade-off
	// surfaced when running test.40: bgutils also pulls this shim, and
	// BotGuard fingerprints the existence + shape of these globals.
	// Stub objects pass typeof === function checks but fail BotGuard's
	// follow-up native-code probe, which can plausibly steer the
	// integrity token toward the websafe fallback. Keeping them
	// cipher-only (full_player_setup.js) restores bgutils to its pre-
	// Phase-5 shim shape while cipher still gets the stubs it needs
	// for full-player-js execution.
	// Capture the native CSPRNG bridges into closure-local variables BEFORE
	// the outer bootstrap clears the globals. Without this capture,
	// getRandomValues would look up __cryptoRandBytes on globalThis at call
	// time and fail with "not a function" as soon as RegisterDOMShim returned.
	var _randBytes = __cryptoRandBytes;
	var _randomUUID = __cryptoRandomUUID;
	globalThis.crypto = {
		getRandomValues: function(arr) {
			var bytes = new Uint8Array(_randBytes(arr.length));
			for (var i = 0; i < arr.length; i++) { arr[i] = bytes[i]; }
			return arr;
		},
		// subtle is stubbed to reject async ops with a clear error rather than
		// throwing on missing-method access. BotGuard variants that probe
		// subtle.digest / subtle.importKey / ... degrade gracefully.
		subtle: {
			digest: function() { return Promise.reject(new Error('crypto.subtle.digest not supported')); },
			importKey: function() { return Promise.reject(new Error('crypto.subtle.importKey not supported')); },
			exportKey: function() { return Promise.reject(new Error('crypto.subtle.exportKey not supported')); },
			sign: function() { return Promise.reject(new Error('crypto.subtle.sign not supported')); },
			verify: function() { return Promise.reject(new Error('crypto.subtle.verify not supported')); },
			encrypt: function() { return Promise.reject(new Error('crypto.subtle.encrypt not supported')); },
			decrypt: function() { return Promise.reject(new Error('crypto.subtle.decrypt not supported')); },
			deriveKey: function() { return Promise.reject(new Error('crypto.subtle.deriveKey not supported')); },
			deriveBits: function() { return Promise.reject(new Error('crypto.subtle.deriveBits not supported')); },
			generateKey: function() { return Promise.reject(new Error('crypto.subtle.generateKey not supported')); },
			wrapKey: function() { return Promise.reject(new Error('crypto.subtle.wrapKey not supported')); },
			unwrapKey: function() { return Promise.reject(new Error('crypto.subtle.unwrapKey not supported')); }
		},
		randomUUID: _randomUUID
	};
	globalThis.postMessage = function() {};
	globalThis.addEventListener = function() {};
	globalThis.removeEventListener = function() {};
	globalThis.dispatchEvent = function() { return true; };
	globalThis.close = function() {};
	globalThis.focus = function() {};
	globalThis.blur = function() {};
	globalThis.open = function() { return null; };
	globalThis.alert = function() {};
	globalThis.confirm = function() { return false; };
	globalThis.prompt = function() { return null; };
	globalThis.print = function() {};
	globalThis.scroll = function() {};
	globalThis.scrollTo = function() {};
	globalThis.scrollBy = function() {};
	globalThis.history = { pushState: function() {}, replaceState: function() {}, back: function() {}, forward: function() {}, go: function() {}, length: 1 };
	globalThis.origin = 'https://www.youtube.com';
	globalThis.isSecureContext = true;

	// Console stub — BotGuard interpreter calls console.log/warn/error;
	// without this, ReferenceError aborts execution silently. Capped at
	// __consoleMaxMessages to prevent a chatty interpreter from unbounded
	// memory growth before we drain the buffer.
	var __consoleMaxMessages = 100;
	var __consoleMessages = [];
	function __consolePush(lvl, args) { if (__consoleMessages.length < __consoleMaxMessages) __consoleMessages.push([lvl, Array.prototype.slice.call(args)]); }
	globalThis.console = {
		log: function() { __consolePush('log', arguments); },
		warn: function() { __consolePush('warn', arguments); },
		error: function() { __consolePush('error', arguments); },
		info: function() { __consolePush('info', arguments); },
		debug: function() { __consolePush('debug', arguments); },
		trace: function() {},
		dir: function() {},
		table: function() {},
		time: function() {},
		timeEnd: function() {},
		timeLog: function() {},
		assert: function() {},
		count: function() {},
		countReset: function() {},
		group: function() {},
		groupEnd: function() {},
		groupCollapsed: function() {},
		clear: function() {}
	};

	// queueMicrotask — used by some Promise implementations and BotGuard internals
	globalThis.queueMicrotask = globalThis.queueMicrotask || function(fn) { Promise.resolve().then(fn); };
})();
`

	if _, err := vm.RunString(shimCode); err != nil {
		return fmt.Errorf("DOM shim execution failed: %w", err)
	}

	// Leave the __cryptoRandBytes / __cryptoRandomUUID globals in place —
	// the earlier attempt to "clean up" these globals broke the closures
	// captured inside globalThis.crypto, because the closures referenced
	// the globals by name at call time. The C1 fix captures them into
	// closure-local variables inside the IIFE, but leaving the globals as
	// hidden names also costs nothing (they aren't enumerable on globalThis
	// the same way BotGuard-observable APIs are). __moomboxUserAgent is
	// only used during IIFE execution — safe to drop.
	if err := vm.Set("__moomboxUserAgent", goja.Undefined()); err != nil {
		return fmt.Errorf("clear __moomboxUserAgent: %w", err)
	}
	return nil
}

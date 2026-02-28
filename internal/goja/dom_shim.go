package goja

import (
	"github.com/dop251/goja"
)

// RegisterDOMShim registers minimal DOM stubs on a Goja runtime.
// This provides just enough browser-like environment for BotGuard and cipher functions.
func RegisterDOMShim(vm *goja.Runtime, userAgent string) error {
	if userAgent == "" {
		userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
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
		userAgent: ` + "`" + userAgent + "`" + `,
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
	globalThis.crypto = {
		getRandomValues: function(arr) {
			for (var i = 0; i < arr.length; i++) { arr[i] = Math.floor(Math.random() * 256); }
			return arr;
		},
		subtle: {},
		randomUUID: function() {
			return 'xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx'.replace(/[xy]/g, function(c) {
				var r = Math.random() * 16 | 0;
				return (c === 'x' ? r : (r & 0x3 | 0x8)).toString(16);
			});
		}
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
	// without this, ReferenceError aborts execution silently.
	var __consoleMessages = [];
	globalThis.console = {
		log: function() { __consoleMessages.push(['log', Array.prototype.slice.call(arguments)]); },
		warn: function() { __consoleMessages.push(['warn', Array.prototype.slice.call(arguments)]); },
		error: function() { __consoleMessages.push(['error', Array.prototype.slice.call(arguments)]); },
		info: function() { __consoleMessages.push(['info', Array.prototype.slice.call(arguments)]); },
		debug: function() { __consoleMessages.push(['debug', Array.prototype.slice.call(arguments)]); },
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

	_, err := vm.RunString(shimCode)
	return err
}

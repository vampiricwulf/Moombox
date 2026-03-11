package goja

import (
	"encoding/base64"

	"github.com/dop251/goja"
)

// RegisterEncoding registers TextEncoder, TextDecoder, atob, and btoa on a Goja runtime.
func RegisterEncoding(vm *goja.Runtime) error {
	// atob: base64 string -> decoded string
	if err := vm.Set("atob", func(call goja.FunctionCall) goja.Value {
		encoded := call.Argument(0).String()
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			// Try with padding stripped
			decoded, err = base64.RawStdEncoding.DecodeString(encoded)
			if err != nil {
				panic(vm.NewGoError(err))
			}
		}
		return vm.ToValue(string(decoded))
	}); err != nil {
		return err
	}

	// btoa: string -> base64 encoded string
	if err := vm.Set("btoa", func(call goja.FunctionCall) goja.Value {
		input := call.Argument(0).String()
		encoded := base64.StdEncoding.EncodeToString([]byte(input))
		return vm.ToValue(encoded)
	}); err != nil {
		return err
	}

	// TextEncoder
	textEncoderCode := `
(function() {
	function TextEncoder() {}
	TextEncoder.prototype.encode = function(str) {
		if (typeof str !== 'string') str = String(str);
		var buf = [];
		for (var i = 0; i < str.length; i++) {
			var c = str.charCodeAt(i);
			if (c < 0x80) {
				buf.push(c);
			} else if (c < 0x800) {
				buf.push(0xc0 | (c >> 6), 0x80 | (c & 0x3f));
			} else if (c >= 0xd800 && c <= 0xdbff && i + 1 < str.length) {
				var c2 = str.charCodeAt(++i);
				if (c2 >= 0xdc00 && c2 <= 0xdfff) {
					var cp = ((c - 0xd800) << 10) + (c2 - 0xdc00) + 0x10000;
					buf.push(0xf0 | (cp >> 18), 0x80 | ((cp >> 12) & 0x3f), 0x80 | ((cp >> 6) & 0x3f), 0x80 | (cp & 0x3f));
				}
			} else {
				buf.push(0xe0 | (c >> 12), 0x80 | ((c >> 6) & 0x3f), 0x80 | (c & 0x3f));
			}
		}
		return new Uint8Array(buf);
	};
	globalThis.TextEncoder = TextEncoder;
})();
`

	// TextDecoder
	textDecoderCode := `
(function() {
	function TextDecoder(encoding) {
		this.encoding = (encoding || 'utf-8').toLowerCase();
	}
	TextDecoder.prototype.decode = function(input) {
		if (!input) return '';
		var bytes;
		if (input instanceof Uint8Array) {
			bytes = input;
		} else if (input instanceof ArrayBuffer) {
			bytes = new Uint8Array(input);
		} else if (Array.isArray(input)) {
			bytes = new Uint8Array(input);
		} else {
			bytes = new Uint8Array(input);
		}
		var result = '';
		var i = 0;
		while (i < bytes.length) {
			var b = bytes[i];
			if (b < 0x80) {
				result += String.fromCharCode(b);
				i++;
			} else if ((b & 0xe0) === 0xc0) {
				if (i + 1 >= bytes.length) { result += '\ufffd'; i++; continue; }
				result += String.fromCharCode(((b & 0x1f) << 6) | (bytes[i+1] & 0x3f));
				i += 2;
			} else if ((b & 0xf0) === 0xe0) {
				if (i + 2 >= bytes.length) { result += '\ufffd'; i++; continue; }
				result += String.fromCharCode(((b & 0x0f) << 12) | ((bytes[i+1] & 0x3f) << 6) | (bytes[i+2] & 0x3f));
				i += 3;
			} else if ((b & 0xf8) === 0xf0) {
				if (i + 3 >= bytes.length) { result += '\ufffd'; i++; continue; }
				var cp = ((b & 0x07) << 18) | ((bytes[i+1] & 0x3f) << 12) | ((bytes[i+2] & 0x3f) << 6) | (bytes[i+3] & 0x3f);
				if (cp > 0xffff) {
					cp -= 0x10000;
					result += String.fromCharCode(0xd800 | (cp >> 10), 0xdc00 | (cp & 0x3ff));
				} else {
					result += String.fromCharCode(cp);
				}
				i += 4;
			} else {
				result += '\ufffd';
				i++;
			}
		}
		return result;
	};
	globalThis.TextDecoder = TextDecoder;
})();
`

	if _, err := vm.RunString(textEncoderCode); err != nil {
		return err
	}
	if _, err := vm.RunString(textDecoderCode); err != nil {
		return err
	}

	return nil
}

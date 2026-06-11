package bgutils

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/dop251/goja"
)

// WebPoMinter mints PO tokens using the BotGuard-provided callback.
//
// The mu mutex serializes all JS-entering operations on the minter's Goja
// runtime: MintAsWebsafeString (which calls mintCallback) and Cleanup
// (which invokes the runtime's shutdownFn). Goja runtimes are not
// goroutine-safe, and the potprovider may concurrently hit a single
// minter from multiple GeneratePoToken callers + the InvalidateIT path.
type WebPoMinter struct {
	mu           sync.Mutex
	mintCallback goja.Callable
	vm           *goja.Runtime
}

// NewWebPoMinter creates a minter from the integrity token data and webPoSignalOutput.
// webPoSignalOutput is a native JS array populated by BotGuard during snapshot.
// webPoSignalOutput[0] is a function that, when called with the decoded integrity
// token, returns a mint callback function.
func NewWebPoMinter(itData *IntegrityTokenData, webPoSignalOutput *goja.Object, vm *goja.Runtime) (*WebPoMinter, error) {
	// Get the callback function from webPoSignalOutput[0]
	callbackVal := webPoSignalOutput.Get("0")
	if callbackVal == nil || goja.IsUndefined(callbackVal) {
		return nil, &BGError{Code: ErrIntegrity, Message: "PMD:Undefined - webPoSignalOutput[0] is undefined"}
	}

	callbackFn, ok := goja.AssertFunction(callbackVal)
	if !ok {
		return nil, &BGError{Code: ErrIntegrity, Message: "APF:Failed - webPoSignalOutput[0] is not a function"}
	}

	// Decode the integrity token. YouTube's GenerateIT returns standard base64.
	// Try standard first, then URL-safe (both padded and unpadded).
	decodedToken, err := base64.StdEncoding.DecodeString(itData.IntegrityToken)
	if err != nil {
		decodedToken, err = base64.URLEncoding.DecodeString(itData.IntegrityToken)
		if err != nil {
			decodedToken, err = base64.RawURLEncoding.DecodeString(itData.IntegrityToken)
			if err != nil {
				return nil, &BGError{Code: ErrIntegrity, Message: fmt.Sprintf("failed to decode integrity token: %v", err)}
			}
		}
	}

	// Call the callback with the decoded integrity token to get the mint function
	// webPoSignalOutput[0](decodedToken) -> mintCallback
	mintResult, err := callbackFn(goja.Undefined(), vm.ToValue(decodedToken))
	if err != nil {
		return nil, &BGError{Code: ErrIntegrity, Message: fmt.Sprintf("call webPoSignalOutput callback: %v", err)}
	}

	mintFn, ok := goja.AssertFunction(mintResult)
	if !ok {
		return nil, &BGError{Code: ErrIntegrity, Message: "mint result is not a function"}
	}

	return &WebPoMinter{
		mintCallback: mintFn,
		vm:           vm,
	}, nil
}

// MintAsWebsafeString mints a PO token for the given content binding (typically visitor data).
// Returns a base64url-encoded token string.
func (m *WebPoMinter) MintAsWebsafeString(contentBinding string) (string, error) {
	// Serialize entry into the Goja runtime — see struct doc.
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.mintCallback == nil {
		return "", &BGError{Code: ErrIntegrity, Message: "mint callback not initialized (shutdown?)"}
	}

	// Encode the content binding as UTF-8 bytes (via TextEncoder in JS)
	// We pass the raw string and let the mint callback handle encoding
	encoded := []byte(contentBinding)

	// Call mintCallback(encodedIdentifier) -> Uint8Array, timeboxed with
	// vm.Interrupt. The mint executes server-supplied BotGuard JS; a looping
	// callback (anti-bot rotation) would otherwise block this goroutine
	// forever while pinning m.mu — wedging every later mint AND Shutdown for
	// the process lifetime. Same goroutine+Interrupt+ClearInterrupt ordering
	// as cipher.callWithTimeout: on timeout, fire the interrupt, wait for
	// the goroutine to actually exit, and only then clear the flag so the
	// next call on this VM starts clean.
	type mintResult struct {
		val goja.Value
		err error
	}
	done := make(chan mintResult, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- mintResult{nil, fmt.Errorf("mint callback panic: %v", r)}
			}
		}()
		v, e := m.mintCallback(goja.Undefined(), m.vm.ToValue(encoded))
		done <- mintResult{v, e}
	}()

	timer := time.NewTimer(DefaultMintTimeout)
	defer timer.Stop()

	var r mintResult
	select {
	case r = <-done:
	case <-timer.C:
		m.vm.Interrupt(fmt.Sprintf("mint timeout (%v)", DefaultMintTimeout))
		r = <-done // wait for the goroutine to observe the interrupt
	}
	m.vm.ClearInterrupt()

	result, err := r.val, r.err
	if err != nil {
		return "", fmt.Errorf("mint callback: %w", err)
	}

	// Convert result to bytes
	resultBytes, err := gojaValueToBytes(result, m.vm)
	if err != nil {
		return "", fmt.Errorf("convert mint result: %w", err)
	}

	// Encode as base64url (websafe) — TS uses btoa()+replace which keeps = padding
	return base64.URLEncoding.EncodeToString(resultBytes), nil
}

// Shutdown releases the minter's Goja resources. Invokes shutdownFn (typically
// the BotGuardClient's Shutdown) under the minter's mutex so it can't race
// with an in-flight Mint. Idempotent — a second Shutdown is a no-op, and
// subsequent Mint calls return a clear error rather than panicking on a
// torn-down runtime.
func (m *WebPoMinter) Shutdown(shutdownFn func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mintCallback == nil {
		return // already shut down
	}
	m.mintCallback = nil
	if shutdownFn != nil {
		shutdownFn()
	}
}

// gojaValueToBytes converts a Goja value (Uint8Array, ArrayBuffer, or Array) to Go bytes.
func gojaValueToBytes(val goja.Value, vm *goja.Runtime) ([]byte, error) {
	if val == nil || goja.IsUndefined(val) || goja.IsNull(val) {
		return nil, fmt.Errorf("value is nil/undefined")
	}

	// Try as ArrayBuffer
	if buf, ok := val.Export().(goja.ArrayBuffer); ok {
		return buf.Bytes(), nil
	}

	// Try as object with numeric keys (Uint8Array/Array)
	obj := val.ToObject(vm)

	// Try to get length
	lengthVal := obj.Get("length")
	if lengthVal == nil || goja.IsUndefined(lengthVal) {
		// Try byteLength for ArrayBuffer views
		lengthVal = obj.Get("byteLength")
	}

	if lengthVal == nil || goja.IsUndefined(lengthVal) {
		// Fall back to string conversion
		return []byte(val.String()), nil
	}

	length := int(lengthVal.ToInteger())
	result := make([]byte, length)
	for i := range length {
		elem := obj.Get(strconv.Itoa(i))
		if elem != nil && !goja.IsUndefined(elem) {
			result[i] = byte(elem.ToInteger())
		}
	}

	return result, nil
}

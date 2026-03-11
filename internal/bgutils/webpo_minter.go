package bgutils

import (
	"encoding/base64"
	"fmt"
	"strconv"

	"github.com/dop251/goja"
)

// WebPoMinter mints PO tokens using the BotGuard-provided callback.
type WebPoMinter struct {
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
	if m.mintCallback == nil {
		return "", &BGError{Code: ErrIntegrity, Message: "mint callback not initialized"}
	}

	// Encode the content binding as UTF-8 bytes (via TextEncoder in JS)
	// We pass the raw string and let the mint callback handle encoding
	encoded := []byte(contentBinding)

	// Call mintCallback(encodedIdentifier) -> Uint8Array
	result, err := m.mintCallback(goja.Undefined(), m.vm.ToValue(encoded))
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
	for i := 0; i < length; i++ {
		elem := obj.Get(strconv.Itoa(i))
		if elem != nil && !goja.IsUndefined(elem) {
			result[i] = byte(elem.ToInteger())
		}
	}

	return result, nil
}

package bgutils

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/dop251/goja"

	gojahelpers "github.com/vampiricwulf/Moombox/internal/goja"
)

// BotGuardClient wraps a Goja runtime running the BotGuard interpreter.
type BotGuardClient struct {
	vm           *goja.Runtime
	timerMgr     *gojahelpers.TimerManager
	globalName   string
	syncSnapshot goja.Callable
	asyncSnapshot goja.Callable
	shutdownFn   goja.Callable
}

// NewBotGuardClient creates a BotGuard client by loading the interpreter and initializing the VM.
func NewBotGuardClient(ctx context.Context, challenge *DescrambledChallenge) (*BotGuardClient, error) {
	if challenge.Program == "" || challenge.GlobalName == "" {
		return nil, &BGError{Code: ErrBadConfig, Message: "program and globalName are required"}
	}

	// Get interpreter JS: prefer URL fetch, fall back to inline script
	var interpreterJS string
	if challenge.InterpreterURL != "" {
		js, err := fetchInterpreter(ctx, challenge.InterpreterURL)
		if err != nil {
			return nil, &BGError{Code: ErrRequestFailed, Message: fmt.Sprintf("fetch interpreter: %v", err)}
		}
		interpreterJS = js
	} else if challenge.InterpreterScript != "" {
		interpreterJS = challenge.InterpreterScript
	} else {
		return nil, &BGError{Code: ErrBadConfig, Message: "no interpreter URL or script in challenge"}
	}

	// Create runtime with full shims
	userAgent := UserAgentFull
	vm, tm, err := gojahelpers.NewRuntimeWithShims(userAgent)
	if err != nil {
		return nil, &BGError{Code: ErrVMInit, Message: fmt.Sprintf("create runtime: %v", err)}
	}

	client := &BotGuardClient{
		vm:         vm,
		timerMgr:   tm,
		globalName: challenge.GlobalName,
	}

	// Execute the interpreter JS with a timeout to prevent hangs from malicious/buggy scripts
	vmDone := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				vmDone <- fmt.Errorf("goja panic: %v", r)
			}
		}()
		_, runErr := vm.RunString(interpreterJS)
		vmDone <- runErr
	}()
	timer := time.NewTimer(30 * time.Second)
	select {
	case err = <-vmDone:
		timer.Stop()
	case <-timer.C:
		vm.Interrupt("BotGuard interpreter execution timeout (30s)")
		err = <-vmDone // wait for RunString to return after interrupt
	case <-ctx.Done():
		vm.Interrupt("context cancelled")
		err = ctx.Err()
	}
	if err != nil {
		client.Shutdown()
		return nil, &BGError{Code: ErrVMInit, Message: fmt.Sprintf("execute interpreter: %v", err)}
	}

	// Get the VM object at globalThis[globalName]
	vmObj := vm.Get(challenge.GlobalName)
	if vmObj == nil || goja.IsUndefined(vmObj) {
		client.Shutdown()
		return nil, &BGError{Code: ErrVMInit, Message: fmt.Sprintf("VM object %q not found", challenge.GlobalName)}
	}

	// Get the vm.a function
	vmObject := vmObj.ToObject(vm)
	aVal := vmObject.Get("a")
	if aVal == nil || goja.IsUndefined(aVal) {
		client.Shutdown()
		return nil, &BGError{Code: ErrVMInit, Message: "vm.a function not found"}
	}

	aFn, ok := goja.AssertFunction(aVal)
	if !ok {
		client.Shutdown()
		return nil, &BGError{Code: ErrVMInit, Message: "vm.a is not a function"}
	}

	// Call vm.a(program, vmFunctionsCallback, true, userInteractionElement, noOp, [[], []])
	// The callback receives: [asyncSnapshotFunction, shutdownFunction, passEventFunction, checkCameraFunction]
	type callbackResult struct {
		asyncSnapshot goja.Callable
		shutdownFn    goja.Callable
	}

	resultCh := make(chan callbackResult, 1)

	callbackFn := vm.ToValue(func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) > 0 {
			arg0 := call.Argument(0)
			if fn, ok := goja.AssertFunction(arg0); ok {
				res := callbackResult{asyncSnapshot: fn}
				// Try to get shutdown function (argument 1)
				if len(call.Arguments) > 1 {
					if shutFn, ok := goja.AssertFunction(call.Argument(1)); ok {
						res.shutdownFn = shutFn
					}
				}
				resultCh <- res
			}
		}
		return goja.Undefined()
	})

	noOp := vm.ToValue(func(goja.FunctionCall) goja.Value { return goja.Undefined() })
	emptyArrays := vm.ToValue([][]any{{}, {}})

	// Call vm.a(program, callback, true, undefined, noOp, [[], []])
	result, err := aFn(vmObj, vm.ToValue(challenge.Program), callbackFn, vm.ToValue(true), goja.Undefined(), noOp, emptyArrays)
	if err != nil {
		client.Shutdown()
		return nil, &BGError{Code: ErrSyncSnapshot, Message: fmt.Sprintf("vm.a() call failed: %v", err)}
	}

	// Extract syncSnapshot from result (first element if array)
	if result != nil && !goja.IsUndefined(result) {
		resultObj := result.ToObject(vm)
		idx0 := resultObj.Get("0")
		if idx0 != nil && !goja.IsUndefined(idx0) {
			if fn, ok := goja.AssertFunction(idx0); ok {
				client.syncSnapshot = fn
			}
		}
	}

	// Wait for async callback with timeout
	loadTimer := time.NewTimer(BotGuardLoadTimeout)
	select {
	case cb := <-resultCh:
		loadTimer.Stop()
		client.asyncSnapshot = cb.asyncSnapshot
		if cb.shutdownFn != nil {
			client.shutdownFn = cb.shutdownFn
		}
	case <-loadTimer.C:
		client.Shutdown()
		return nil, &BGError{Code: ErrTimeout, Message: "BotGuard callback timed out"}
	case <-ctx.Done():
		loadTimer.Stop()
		client.Shutdown()
		return nil, ctx.Err()
	}

	return client, nil
}

// Snapshot calls the async snapshot function to generate a BotGuard response.
// Returns the botguard response string and a JS array that BotGuard populates
// with signal data (webPoSignalOutput[0] will be the getMinter callback).
func (c *BotGuardClient) Snapshot(timeout time.Duration) (string, *goja.Object, error) {
	if c.asyncSnapshot == nil {
		return "", nil, &BGError{Code: ErrAsyncSnapshot, Message: "async snapshot function not available"}
	}

	if timeout == 0 {
		timeout = DefaultMintTimeout
	}

	// Create a proper JS array for webPoSignalOutput.
	// CRITICAL: Must be a native JS array, NOT a Go slice proxy.
	// BotGuard VM writes to webPoSignalOutput[0] during snapshot, and Go slice
	// proxies can't be grown by JS code (Go slices are value types).
	webPoSignalOutput := c.vm.NewArray()

	resultCh := make(chan string, 1)

	// Create callback for async snapshot result
	callback := c.vm.ToValue(func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) > 0 {
			resultCh <- call.Argument(0).String()
		}
		return goja.Undefined()
	})

	// Build args array: [contentBinding, signedTimestamp, webPoSignalOutput, skipPrivacyBuffer]
	// BotGuard expects a flat array, not an object
	argsArr := c.vm.NewArray(
		goja.Undefined(),      // contentBinding (nil for PO token)
		goja.Undefined(),      // signedTimestamp
		webPoSignalOutput,     // native JS array for BotGuard to populate
		goja.Undefined(),      // skipPrivacyBuffer
	)

	// Call asyncSnapshotFunction(callback, args)
	_, err := c.asyncSnapshot(goja.Undefined(), callback, argsArr)
	if err != nil {
		return "", nil, &BGError{Code: ErrAsyncSnapshot, Message: fmt.Sprintf("async snapshot call: %v", err)}
	}

	// Wait for result
	snapshotTimer := time.NewTimer(timeout)
	select {
	case result := <-resultCh:
		snapshotTimer.Stop()
		return result, webPoSignalOutput, nil
	case <-snapshotTimer.C:
		return "", nil, &BGError{Code: ErrTimeout, Message: "snapshot timed out"}
	}
}

// ClearTimers cancels BotGuard's telemetry timers without destroying the VM.
// This is safe to call while minters are still cached — the timers are only
// for monitoring and don't affect minting functionality.
func (c *BotGuardClient) ClearTimers() {
	if c.timerMgr != nil {
		c.timerMgr.CancelAll()
	}
}

// Shutdown cleans up the BotGuard VM resources.
func (c *BotGuardClient) Shutdown() {
	defer func() {
		if r := recover(); r != nil {
			// Goja VM may panic during shutdown — safe to ignore (matches TS try/catch pattern)
		}
	}()

	if c.timerMgr != nil {
		c.timerMgr.CancelAll()
	}
	if c.shutdownFn != nil {
		c.shutdownFn(goja.Undefined())
	}
	// Delete the global VM object
	if c.vm != nil && c.globalName != "" {
		c.vm.Set(c.globalName, goja.Undefined())
	}
}

// fetchInterpreter downloads the BotGuard interpreter JS.
func fetchInterpreter(ctx context.Context, interpreterURL string) (string, error) {
	if interpreterURL == "" {
		return "", fmt.Errorf("interpreter URL is empty")
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, interpreterURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", UserAgentShort)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("interpreter fetch returned status %d", resp.StatusCode)
	}

	// Limit read to 20MB — interpreter JS can be large but shouldn't be unbounded
	body, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil {
		return "", err
	}

	return string(body), nil
}

package bgutils

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/dop251/goja"

	gojahelpers "github.com/vampiricwulf/Moombox/internal/goja"
)

// botguardLogger captures the minimal logging surface used by BotGuardClient.
// Matches the logger shape already used by PotProvider / WebPoClient.
type botguardLogger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// BotGuardClient wraps a Goja runtime running the BotGuard interpreter.
type BotGuardClient struct {
	vm            *goja.Runtime
	timerMgr      *gojahelpers.TimerManager
	globalName    string
	asyncSnapshot goja.Callable
	shutdownFn    goja.Callable
	logger        botguardLogger // may be nil
}

// logWarn routes a warning through the plumbed logger if present. Previously
// the package used fmt.Fprintf(os.Stderr, ...) which bypassed the app's log
// level/file conventions and made timer-callback failures invisible.
func (c *BotGuardClient) logWarn(msg string, args ...any) {
	if c != nil && c.logger != nil {
		c.logger.Warn(msg, args...)
	}
}

// NewBotGuardClient creates a BotGuard client by loading the interpreter and
// initializing the VM. logger is optional (nil-safe).
func NewBotGuardClient(ctx context.Context, challenge *DescrambledChallenge, logger botguardLogger) (*BotGuardClient, error) {
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

	// Create runtime with full shims. Thread the parent ctx so an early
	// shutdown (caller cancels) propagates into the timer manager
	// without needing an explicit Shutdown() call. Audit goja TD-5.
	userAgent := UserAgentFull
	vm, tm, err := gojahelpers.NewRuntimeWithShims(ctx, userAgent)
	if err != nil {
		return nil, &BGError{Code: ErrVMInit, Message: fmt.Sprintf("create runtime: %v", err)}
	}

	client := &BotGuardClient{
		vm:         vm,
		timerMgr:   tm,
		globalName: challenge.GlobalName,
		logger:     logger,
	}

	// Execute the interpreter JS with a timeout to prevent hangs from
	// malicious/buggy scripts. Delegates to gojahelpers.RunStringWithTimeout
	// which encapsulates the goroutine + select + Interrupt + ClearInterrupt
	// pattern that this site used to hand-roll. Audit reports/goja.md Q1.
	if _, err := gojahelpers.RunStringWithTimeout(ctx, vm, interpreterJS, InterpreterExecutionTimeout); err != nil {
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
	// emptyArrays is the 6th vm.a() arg: upstream passes a JS literal
	// `[ [], [] ]`. The previous form here was vm.ToValue([][]any{{}, {}}),
	// which exposes Go-slice-proxies to JS. BotGuard may call .push() / [N]=
	// on those inner arrays during init; slice proxies silently fail on
	// length grow (the same reason webPoSignalOutput uses NewArray()) and
	// can leave BotGuard's internal state misaligned downstream — one
	// possible reason webPoSignalOutput[0] never gets populated by snapshot.
	// Build native JS arrays explicitly so BotGuard sees the same shape it
	// would in a real browser.
	emptyArrays := vm.NewArray(vm.NewArray(), vm.NewArray())

	// Call vm.a(program, callback, true, undefined, noOp, [[], []])
	// Return value (upstream's "syncSnapshot" position) is intentionally ignored —
	// we only need the asyncSnapshot / shutdownFn delivered via the callback arg.
	if _, err := aFn(vmObj, vm.ToValue(challenge.Program), callbackFn, vm.ToValue(true), goja.Undefined(), noOp, emptyArrays); err != nil {
		client.Shutdown()
		return nil, &BGError{Code: ErrSyncSnapshot, Message: fmt.Sprintf("vm.a() call failed: %v", err)}
	}

	// Drain any timer callbacks that were enqueued during vm.a() execution.
	// BotGuard's interpreter sets up timers during initialization; their
	// callbacks are queued (Goja is single-threaded) and must be drained on
	// the VM-owning goroutine before we proceed.
	if tm != nil {
		if _, drainErr := tm.DrainCallbacks(); drainErr != nil {
			client.logWarn("bgutils: timer callback error during init", "err", drainErr)
		}
	}

	// Wait for async callback with timeout
	loadTimer := time.NewTimer(BotGuardCallbackTimeout)
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
//
// Honours ctx.Done(): on cancel, vm.Interrupt is fired so an in-flight snapshot
// unblocks instead of burning the full timeout budget. Timeout=0 falls back to
// SnapshotDefaultTimeout (30s) — the ambient BotGuard snapshot budget — not
// the much shorter DefaultMintTimeout, which was too aggressive for a cold
// snapshot.
func (c *BotGuardClient) Snapshot(ctx context.Context, timeout time.Duration) (string, *goja.Object, error) {
	if c.asyncSnapshot == nil {
		return "", nil, &BGError{Code: ErrAsyncSnapshot, Message: "async snapshot function not available"}
	}

	if timeout == 0 {
		timeout = SnapshotDefaultTimeout
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

	// Drain any timer callbacks enqueued during the snapshot call.
	// BotGuard may set up monitoring/telemetry timers whose callbacks need
	// to be executed on the VM thread before we read the result.
	if c.timerMgr != nil {
		if _, drainErr := c.timerMgr.DrainCallbacks(); drainErr != nil {
			c.logWarn("bgutils: timer callback error during snapshot", "err", drainErr)
		}
	}

	// Wait for result, timeout, or context cancellation.
	snapshotTimer := time.NewTimer(timeout)
	defer snapshotTimer.Stop()
	select {
	case result := <-resultCh:
		return result, webPoSignalOutput, nil
	case <-snapshotTimer.C:
		c.vm.Interrupt("snapshot timeout")
		// Pair Interrupt with ClearInterrupt so a subsequent Snapshot
		// (or any other JS call) on the SAME client doesn't trip the
		// stale interrupt and fail before doing real work. Today every
		// caller follows a Snapshot timeout with Shutdown — which
		// would clear the interrupt itself — but defending here keeps
		// the client reusable across timeout boundaries. Audit
		// reports/goja.md Q1.
		c.vm.ClearInterrupt()
		return "", nil, &BGError{Code: ErrTimeout, Message: "snapshot timed out"}
	case <-ctx.Done():
		c.vm.Interrupt("context cancelled during snapshot")
		c.vm.ClearInterrupt()
		return "", nil, ctx.Err()
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
			// Goja VM may panic during shutdown — log (if we have a sink) but
			// never propagate; shutdown is always best-effort.
			c.logWarn("bgutils: BotGuardClient.Shutdown panic", "panic", r)
		}
	}()

	// Clear any pending Interrupt left over from a Snapshot/NewBotGuardClient
	// timeout or ctx-cancel before touching the VM. Without this, the next
	// goja call (shutdownFn invocation, vm.Set) trips the interrupt and the
	// shutdownFn never runs — masking is caught by the defer above but
	// defeats the cleanup. Audit reports/goja.md Q1.
	if c.vm != nil {
		c.vm.ClearInterrupt()
	}

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

// Interpreter-JS fetch cache. YouTube rotates the interpreter URL rarely
// (hours to days) but NewBotGuardClient fetched it fresh on every minter
// creation — worth ~1-3 MB + parse per call. Keyed by URL since the URL
// already encodes the content hash in the path. Capped at a handful of
// entries; old entries become unreachable once YouTube rotates anyway.
var (
	interpreterCacheMu    sync.Mutex
	interpreterCache      = make(map[string]string, interpreterCacheCap)
	interpreterCacheOrder = make([]string, 0, interpreterCacheCap)
)

const interpreterCacheCap = 4

func interpreterCacheGet(url string) (string, bool) {
	interpreterCacheMu.Lock()
	defer interpreterCacheMu.Unlock()
	js, ok := interpreterCache[url]
	return js, ok
}

func interpreterCachePut(url, js string) {
	interpreterCacheMu.Lock()
	defer interpreterCacheMu.Unlock()
	if _, exists := interpreterCache[url]; exists {
		return
	}
	if len(interpreterCacheOrder) >= interpreterCacheCap {
		// Evict oldest.
		oldest := interpreterCacheOrder[0]
		interpreterCacheOrder = interpreterCacheOrder[1:]
		delete(interpreterCache, oldest)
	}
	interpreterCache[url] = js
	interpreterCacheOrder = append(interpreterCacheOrder, url)
}

// fetchInterpreter downloads the BotGuard interpreter JS, returning the
// cached copy when the URL has been seen before.
func fetchInterpreter(ctx context.Context, interpreterURL string) (string, error) {
	if interpreterURL == "" {
		return "", fmt.Errorf("interpreter URL is empty")
	}
	if cached, ok := interpreterCacheGet(interpreterURL); ok {
		return cached, nil
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, interpreterURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", UserAgentFull)

	resp, err := bgHTTPClient.Do(req)
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

	js := string(body)
	interpreterCachePut(interpreterURL, js)
	return js, nil
}

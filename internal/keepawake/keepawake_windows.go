//go:build windows

package keepawake

import (
	"fmt"
	"os"
	"runtime"
	"sync"

	"golang.org/x/sys/windows"
)

const supported = true

// SetThreadExecutionState flags. esContinuous makes the state last until the
// next call instead of resetting the idle timer once; esSystemRequired holds
// back system sleep. ES_DISPLAY_REQUIRED is deliberately not set: the screen
// may turn off (owner ruling R2-X5).
const (
	esContinuous     = 0x80000000
	esSystemRequired = 0x00000001
)

var procSetThreadExecutionState = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetThreadExecutionState")

// setThreadExecutionState sets the calling thread's execution state and
// returns the previous one, or ok=false when Windows refused.
func setThreadExecutionState(state uint32) (prev uint32, ok bool) {
	r, _, _ := procSetThreadExecutionState.Call(uintptr(state))
	return uint32(r), r != 0
}

// setState is the call the thread makes; a test swaps it.
var setState = setThreadExecutionState

// The execution state belongs to the THREAD that set it, and ends when that
// thread does. A goroutine moves between threads, so every call is made by one
// goroutine locked to one thread for the Holder's whole life; it never unlocks,
// so the thread ends with it, and the state with the thread.
//
// dead is closed when that goroutine ends, a panic included: ask is called
// under the controller's lock from the database's job callbacks, so a request
// nobody will answer must be refused, never waited on.
type holderImpl struct {
	mu      *sync.Mutex
	reqs    chan holdReq
	closed  *bool
	started *bool
	dead    chan struct{}
}

type holdReq struct {
	on   bool
	done chan holdAns
}

// holdAns is what the thread answers: whether Windows took the call, and the
// state the thread had before it.
type holdAns struct {
	ok   bool
	prev uint32
}

func newHolderImpl() holderImpl {
	return holderImpl{mu: &sync.Mutex{}, reqs: make(chan holdReq), closed: new(bool), started: new(bool), dead: make(chan struct{})}
}

func (h holderImpl) hold(on bool) bool {
	return h.ask(on).ok
}

func (h holderImpl) ask(on bool) holdAns {
	h.mu.Lock()
	defer h.mu.Unlock()
	if *h.closed {
		return holdAns{}
	}
	if !*h.started {
		if !on {
			return holdAns{ok: true} // nothing was ever held
		}
		*h.started = true
		go h.run()
	}
	req := holdReq{on: on, done: make(chan holdAns, 1)}
	select {
	case h.reqs <- req:
	case <-h.dead:
		return holdAns{}
	}
	select {
	case ans := <-req.done:
		return ans
	case <-h.dead:
		return holdAns{}
	}
}

func (h holderImpl) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if *h.closed {
		return
	}
	*h.closed = true
	close(h.reqs)
}

func (h holderImpl) run() {
	defer func() {
		close(h.dead) // before anything else: a caller may be waiting on it
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "panic in keep-awake thread: %v\n", r)
		}
	}()
	runtime.LockOSThread() // never unlocked: see holderImpl
	for req := range h.reqs {
		state := uint32(esContinuous)
		if req.on {
			state |= esSystemRequired
		}
		prev, ok := setState(state)
		req.done <- holdAns{ok: ok, prev: prev}
	}
	setState(esContinuous)
}

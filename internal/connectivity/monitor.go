package connectivity

import (
	"context"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

var (
	wininet                  = syscall.NewLazyDLL("wininet.dll")
	procInternetGetConnected = wininet.NewProc("InternetGetConnectedState")
)

const pollInterval = 5 * time.Second

type Monitor struct {
	online       atomic.Bool
	offlinePolls int
	mu           sync.Mutex
	callbacks    map[uint64]func(online bool)
	nextID       uint64
	cancel       context.CancelFunc
	checkFn      func() bool
	passive      *PassiveTracker
	logger       logger
}

type logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

func NewMonitor(log logger) *Monitor {
	m := &Monitor{
		callbacks: make(map[uint64]func(online bool)),
		checkFn:   checkInternetConnected,
		passive:   NewPassiveTracker(),
		logger:    log,
	}
	m.online.Store(true)
	return m
}


func (m *Monitor) Start(ctx context.Context) {
	ctx2, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	go func() {
		defer func() {
			if r := recover(); r != nil {
				if m.logger != nil {
					m.logger.Error("connectivity monitor panic", "panic", r)
				}
			}
		}()
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx2.Done():
				return
			case <-ticker.C:
				m.poll()
			}
		}
	}()
}

func (m *Monitor) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
}

func (m *Monitor) IsOnline() bool {
	return m.online.Load()
}

func (m *Monitor) OnStateChange(fn func(online bool)) func() {
	m.mu.Lock()
	id := m.nextID
	m.nextID++
	m.callbacks[id] = fn
	m.mu.Unlock()

	return func() {
		m.mu.Lock()
		delete(m.callbacks, id)
		m.mu.Unlock()
	}
}

func (m *Monitor) ReportFailure(tag string) {
	m.passive.ReportFailure(tag)
	if m.online.Load() && m.passive.ShouldTriggerOffline() {
		m.transition(false)
	}
}

func (m *Monitor) ReportSuccess(tag string) {
	wasPassiveOffline := m.passive.IsTriggered()
	m.passive.ReportSuccess(tag)
	if wasPassiveOffline && !m.passive.IsTriggered() && m.checkFn() {
		m.transition(true)
	}
}

func (m *Monitor) poll() {
	windowsOnline := m.checkFn()
	passiveOffline := m.passive.IsTriggered()
	nowOnline := windowsOnline && !passiveOffline

	wasOnline := m.online.Load()

	if nowOnline {
		m.offlinePolls = 0
		if !wasOnline {
			m.transition(true)
		}
	} else {
		m.offlinePolls++
		if wasOnline && m.offlinePolls >= 2 {
			m.transition(false)
		}
	}
}

func (m *Monitor) transition(online bool) {
	old := m.online.Swap(online)
	if old == online {
		return
	}
	if online {
		m.offlinePolls = 0
	}

	if m.logger != nil {
		if online {
			m.logger.Info("Internet connectivity restored")
		} else {
			m.logger.Info("Internet connectivity lost")
		}
	}

	m.mu.Lock()
	cbs := make([]func(online bool), 0, len(m.callbacks))
	for _, fn := range m.callbacks {
		cbs = append(cbs, fn)
	}
	m.mu.Unlock()

	for _, fn := range cbs {
		fn(online)
	}
}

func checkInternetConnected() bool {
	var flags uint32
	ret, _, _ := procInternetGetConnected.Call(
		uintptr(unsafe.Pointer(&flags)),
		0,
	)
	return ret != 0
}

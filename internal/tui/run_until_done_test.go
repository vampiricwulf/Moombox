package tui

import (
	"context"
	"io"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// idleModel is a program that never quits on its own: only something outside
// it can end Run. started is closed once the program has initialised.
type idleModel struct{ started chan struct{} }

func (m idleModel) Init() tea.Cmd {
	close(m.started)
	return nil
}
func (m idleModel) Update(tea.Msg) (tea.Model, tea.Cmd) { return m, nil }
func (m idleModel) View() tea.View                      { return tea.NewView("") }

// headlessProgram builds a program with no terminal behind it: no input, no
// output, no renderer and no signal handler of its own — the last so that
// nothing but the context can end it, as nothing but the context ends the
// real one on a hangup.
func headlessProgram(m idleModel) *tea.Program {
	return tea.NewProgram(m,
		tea.WithInput(nil),
		tea.WithOutput(io.Discard),
		tea.WithoutRenderer(),
		tea.WithoutSignalHandler(),
	)
}

// TestRunUntilDoneQuitsWhenTheContextEnds pins the TUI's half of a hangup.
// bubbletea quits on SIGINT and SIGTERM by its own handler and has none for
// SIGHUP, so closing the terminal the TUI ran in left Run blocked on a key
// that could not come: the child's stop-signal handler cancelled the run's
// context, and runTUI — and the shutdown behind it — never got past Run. The
// context ending is now a quit, whether it ends while the program runs or
// before it has started (a stop signal during startup).
//
// Mutant: drop the context.AfterFunc — both rows time out.
func TestRunUntilDoneQuitsWhenTheContextEnds(t *testing.T) {
	for _, tc := range []struct {
		name         string
		cancelBefore bool
	}{
		{"the context ends while the program runs", false},
		{"the context ended before the program started", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := idleModel{started: make(chan struct{})}
			p := headlessProgram(m)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancelBefore {
				cancel()
			}
			done := make(chan error, 1)
			go func() {
				defer func() {
					if r := recover(); r != nil {
						done <- nil
						t.Errorf("runUntilDone panicked: %v", r)
					}
				}()
				done <- runUntilDone(ctx, p)
			}()
			if !tc.cancelBefore {
				select {
				case <-m.started:
				case <-time.After(10 * time.Second):
					p.Kill()
					t.Fatal("the program never started")
				}
				cancel()
			}
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("runUntilDone = %v, want nil: a context that ended is a quit, not a TUI failure", err)
				}
			case <-time.After(10 * time.Second):
				p.Kill()
				<-done
				t.Fatal("runUntilDone still running 10 s after its context ended — the TUI ignores the stop")
			}
		})
	}
}

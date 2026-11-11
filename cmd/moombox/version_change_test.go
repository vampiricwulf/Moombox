package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/logger"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/web"
)

// versionChangeNotifier records what recordVersionChange sends. The rest of
// notifications.Notifier is the owner surface run() uses elsewhere.
type versionChangeNotifier struct {
	sent []versionChangeSend
}

type versionChangeSend struct {
	title, desc string
	ntype       notifications.NotificationType
	fields      []notifications.Field
	event       string
}

func (n *versionChangeNotifier) Send(title, desc string, ntype notifications.NotificationType, fields []notifications.Field, opts notifications.SendOptions) {
	n.sent = append(n.sent, versionChangeSend{title, desc, ntype, fields, opts.Event})
}
func (n *versionChangeNotifier) HasTargets() bool             { return true }
func (n *versionChangeNotifier) Reload(*config.MoomboxConfig) {}
func (n *versionChangeNotifier) BeginShutdown()               {}
func (n *versionChangeNotifier) Wait()                        {}
func (n *versionChangeNotifier) ForgetJob(string)             {}
func (n *versionChangeNotifier) RetainJobs(map[string]struct{}) {
}

// TestAnnounceVersionChange runs run()'s update-applied step: what a boot on
// a new version sends, and that it records its version as the last run. The
// web server is built and never started, so its ActualPort is 0: the notice
// then says nothing about the dashboard unless the bind is known to have
// failed.
//
// Mutants: drop the configLoaded gate (the first-run row writes a config
// file, and the next boot skips the setup wizard); stamp only when announced
// (the rollback and first-stamp rows leave the stamp stale, and the rollback
// would be re-examined every boot); ignore webBindFailed (a dead dashboard is
// reported as a clean restart); send on the rollback row.
func TestAnnounceVersionChange(t *testing.T) {
	for _, tc := range []struct {
		name           string
		lastRun        string
		configLoaded   bool
		rolledBackFrom string
		webBindFailed  bool

		wantSend  bool
		wantType  notifications.NotificationType
		wantDash  string // the Web Dashboard field, "" for none
		wantStamp bool   // the config file is written with this version
	}{
		{name: "an update landed", lastRun: "1.0.0", configLoaded: true,
			wantSend: true, wantType: notifications.TypeSuccess, wantStamp: true},
		{name: "an update landed but the dashboard did not bind", lastRun: "1.0.0", configLoaded: true, webBindFailed: true,
			wantSend: true, wantType: notifications.TypeWarning, wantDash: "FAILED — check logs", wantStamp: true},
		{name: "the launcher rolled back to this version", lastRun: "9.9.9", configLoaded: true, rolledBackFrom: "9.9.9",
			wantStamp: true},
		{name: "the first stamp", lastRun: "", configLoaded: true,
			wantStamp: true},
		{name: "a first run with no config file", lastRun: "", configLoaded: false},
		{name: "another version but no config file", lastRun: "1.0.0", configLoaded: false},
		{name: "the same version as last run", lastRun: version, configLoaded: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.toml")
			cfg := config.Defaults()
			cfg.Updates.LastRunVersion = tc.lastRun
			store := config.NewStore(cfg, configPath)
			log, err := logger.New(filepath.Join(dir, "moombox.log"), "INFO", 1<<20, 1)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { log.Close() })
			notifier := &versionChangeNotifier{}
			s := &runState{log: log, configStore: store, notifyMgr: notifier, webServer: web.NewServer(store, log)}

			s.recordVersionChange(tc.lastRun, tc.configLoaded, tc.rolledBackFrom, tc.webBindFailed)

			if got := len(notifier.sent) == 1; got != tc.wantSend || len(notifier.sent) > 1 {
				t.Fatalf("sent %d notices (%+v), want one: %v", len(notifier.sent), notifier.sent, tc.wantSend)
			}
			if tc.wantSend {
				n := notifier.sent[0]
				if n.title != "Update Applied" || n.event != "update_applied" {
					t.Errorf("sent %q with event %q, want Update Applied / update_applied", n.title, n.event)
				}
				if n.ntype != tc.wantType {
					t.Errorf("notice type = %v, want %v", n.ntype, tc.wantType)
				}
				fields := map[string]string{}
				for _, f := range n.fields {
					fields[f.Name] = f.Value
				}
				if fields["Previous Version"] != "v"+tc.lastRun || fields["Current Version"] != "v"+version {
					t.Errorf("version fields = %v, want v%s to v%s", fields, tc.lastRun, version)
				}
				if fields["Web Dashboard"] != tc.wantDash {
					t.Errorf("Web Dashboard field = %q, want %q", fields["Web Dashboard"], tc.wantDash)
				}
			}

			_, statErr := os.Stat(configPath)
			if !tc.wantStamp {
				if statErr == nil {
					t.Error("a config file was written: on a first run the next boot would skip the setup wizard")
				}
				return
			}
			if statErr != nil {
				t.Fatalf("no config file was written: %v", statErr)
			}
			saved, err := config.Load(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if saved.Updates.LastRunVersion != version {
				t.Errorf("saved last_run_version = %q, want this version %q", saved.Updates.LastRunVersion, version)
			}
		})
	}
}

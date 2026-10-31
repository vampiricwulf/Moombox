package main

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/twitch"
	"github.com/vampiricwulf/Moombox/internal/web/routes"
)

// TestTwitchAddLookupReportsALoginThatDoesNotExist is the adapter between the
// add route and the Twitch service, driven over a real Service whose GQL
// endpoint is a local server (G53): a login Twitch answers with user:null is
// routes.ErrTwitchChannelNotFound — the route's 404 — while an offline channel
// stays (nil, nil) and a live one its metadata.
//
// Mutant: the adapter calling GetStreamInfo again — its collapse turns the
// missing login into (nil, nil), and the route creates a job that waits
// forever.
func TestTwitchAddLookupReportsALoginThatDoesNotExist(t *testing.T) {
	slots := map[string]string{
		"missing": `[{"data":{"user":null}},{"data":{"user":{"stream":null}}}]`,
		"offline": `[{"data":{"user":{"id":"2","displayName":"Off","login":"off","profileImageURL":"","stream":null}}},` +
			`{"data":{"user":{"stream":null}}}]`,
		"live": `[{"data":{"user":{"id":"1","displayName":"On","login":"on","profileImageURL":"",` +
			`"stream":{"id":"77","title":"on air","type":"live","viewersCount":1,"createdAt":"2026-10-31T00:00:00Z","game":null}}}},` +
			`{"data":{"user":{"stream":null}}}]`,
	}
	answer := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, answer)
	}))
	defer srv.Close()
	prev := constants.TwitchURLs.GQL
	constants.TwitchURLs.GQL = srv.URL
	t.Cleanup(func() { constants.TwitchURLs.GQL = prev })

	a := &twitchMetadataAdapter{svc: twitch.NewService(cookies.NewCookieJar(), newAuthLossLogger())}

	answer = slots["missing"]
	if meta, err := a.FetchStreamMetadata(context.Background(), "nosuchstreamer"); meta != nil || !errors.Is(err, routes.ErrTwitchChannelNotFound) {
		t.Errorf("missing login = %+v, %v; want routes.ErrTwitchChannelNotFound", meta, err)
	}
	answer = slots["offline"]
	if meta, err := a.FetchStreamMetadata(context.Background(), "off"); meta != nil || err != nil {
		t.Errorf("offline channel = %+v, %v; want nil, nil", meta, err)
	}
	answer = slots["live"]
	if meta, err := a.FetchStreamMetadata(context.Background(), "on"); err != nil || meta == nil || !meta.IsLive || meta.StreamID != "77" {
		t.Errorf("live channel = %+v, %v; want its metadata", meta, err)
	}
}

// TestTwitchServiceAuthLossIsTheWorkersHook pins the production value of the
// Twitch service's credential-loss seam (twitch.Service.SetOnAuthLoss, G21):
// initServices hands it the SAME value it hands the worker's
// SetOnTwitchAuthLoss, and that value comes from twitchAuthLossHook — the
// goroutine-spawning, recover-guarded hop into NoteTwitchAuthLoss whose
// delivery contract services_twitch_auth_loss_test.go drives. initServices
// cannot be driven from a test, so the wiring is read from the syntax tree,
// the package's technique for undrivable sites.
//
// Mutants: the SetOnAuthLoss call deleted (a status batch's refused token
// marks nothing, and the platform reads healthy while every capture parks);
// it handed a func of its own, not the hook (a mark on the monitor's goroutine
// that bypasses the hook's asynchrony and its recover).
func TestTwitchServiceAuthLossIsTheWorkersHook(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "services.go", nil, 0)
	if err != nil {
		t.Fatalf("parse services.go: %v", err)
	}
	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "initServices" && fn.Body != nil {
			body = fn.Body
		}
	}
	if body == nil {
		t.Fatal("services.go has no initServices with a body — re-anchor this test rather than deleting it")
	}

	hooks := map[string]bool{} // identifiers assigned from twitchAuthLossHook(...)
	var workerArg, serviceArg string
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if len(n.Lhs) == 1 && len(n.Rhs) == 1 {
				if call, ok := n.Rhs[0].(*ast.CallExpr); ok {
					if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == "twitchAuthLossHook" {
						if id, ok := n.Lhs[0].(*ast.Ident); ok {
							hooks[id.Name] = true
						}
					}
				}
			}
		case *ast.CallExpr:
			arg := ""
			if len(n.Args) == 1 {
				if id, ok := n.Args[0].(*ast.Ident); ok {
					arg = id.Name
				}
			}
			switch {
			case selectorIs(n.Fun, "dlWorker", "SetOnTwitchAuthLoss"):
				workerArg = arg
			case selectorIs(n.Fun, "twService", "SetOnAuthLoss"):
				serviceArg = arg
			}
		}
		return true
	})

	if serviceArg == "" {
		t.Fatal("initServices never hands twService.SetOnAuthLoss a named hook — a stream check whose token " +
			"Twitch refused is answered signed out and marks nothing")
	}
	if !hooks[serviceArg] {
		t.Errorf("twService.SetOnAuthLoss gets %q, which is not assigned from twitchAuthLossHook", serviceArg)
	}
	if serviceArg != workerArg {
		t.Errorf("twService.SetOnAuthLoss gets %q and dlWorker.SetOnTwitchAuthLoss %q — one hook, one mark", serviceArg, workerArg)
	}
}

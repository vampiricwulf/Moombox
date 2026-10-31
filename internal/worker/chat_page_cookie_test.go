package worker

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// TestChatFallbackPageCarriesThePageCookies: both chat-start sites fetch a
// watch page of their own when the info carries no usable continuation, and
// that page gets the header every YouTube page is fetched with
// (youtube.Auth.PageCookieHeader) — SOCS=CAI for a jar that is not signed in,
// without which an EU address gets the consent interstitial and chat is
// "unavailable", and an English PREF.
//
// Mutant: either site snapshotting GetCookieHeader (its fetch carries the
// jar's PREF=hl=ja and no SOCS).
func TestChatFallbackPageCarriesThePageCookies(t *testing.T) {
	var sent []string
	orig := fetchWatchPageForChat
	fetchWatchPageForChat = func(_ context.Context, _ string, cookieHeader string) (*youtube.WatchPageResult, error) {
		sent = append(sent, cookieHeader)
		return nil, errors.New("the test stops here")
	}
	t.Cleanup(func() { fetchWatchPageForChat = orig })

	path := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(path, []byte("# Netscape HTTP Cookie File\n"+
		".youtube.com\tTRUE\t/\tTRUE\t0\tSID\tsid-value\n"+
		".youtube.com\tTRUE\t/\tTRUE\t0\tPREF\thl=ja\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	jar := cookies.NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	svc := youtube.NewService(jar, discardLogger{})

	o := &DownloadOrchestrator{db: openTestDB(t), logger: discardLogger{}}
	jobCtx := &JobContext{Job: &database.Job{ID: "yt_chatpage", VideoID: "vid"}, YT: svc}
	if dl := o.setupChatDownloader(context.Background(), jobCtx, &youtube.VideoInfo{}); dl != nil {
		t.Fatal("setupChatDownloader started a downloader without a continuation")
	}
	sp := &StreamProcessor{yt: svc, logger: discardLogger{}}
	if dl, _ := sp.tryStartEarlyChat(context.Background(), &database.Job{VideoID: "vid"}, &youtube.VideoInfo{}, nil); dl != nil {
		t.Fatal("tryStartEarlyChat started a downloader without a continuation")
	}

	const want = "PREF=hl=en&tz=UTC; SID=sid-value; SOCS=CAI"
	if len(sent) != 2 || sent[0] != want || sent[1] != want {
		t.Errorf("fallback pages sent Cookie %q, want %q from both sites", sent, want)
	}
}

// TestChatDownloadersGetThePageCookieGetter pins the other half of the
// wiring, which a unit test cannot drive: the long-lived chat downloader
// re-reads a stale continuation off a fresh watch page, hours into a
// capture, and only its PageCookieHeader getter (a METHOD VALUE, re-read per
// fetch like CookieHeader) gives that page the consent and language cookies.
// internal/chat's TestFreshContinuationSendsThePageCookie covers the
// downloader's side.
//
// Mutant: either site leaving PageCookieHeader unset, or setting it to
// GetCookieHeader.
func TestChatDownloadersGetThePageCookieGetter(t *testing.T) {
	for _, site := range []struct{ file, fn string }{
		{"orchestrator_chat.go", "setupChatDownloader"},
		{"stream_processor_youtube.go", "tryStartEarlyChat"},
	} {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, site.file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", site.file, err)
		}
		var body *ast.FuncDecl
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == site.fn {
				body = fn
			}
		}
		if body == nil {
			t.Fatalf("no function %s in %s", site.fn, site.file)
		}
		found := false
		ast.Inspect(body, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
				return true
			}
			lhs, ok := assign.Lhs[0].(*ast.SelectorExpr)
			if !ok || lhs.Sel.Name != "PageCookieHeader" {
				return true
			}
			rhs, ok := assign.Rhs[0].(*ast.SelectorExpr)
			if !ok || rhs.Sel.Name != "PageCookieHeader" {
				return true
			}
			if recv, ok := rhs.X.(*ast.SelectorExpr); ok && recv.Sel.Name == "Auth" {
				found = true
			}
			return true
		})
		if !found {
			t.Errorf("%s (%s) does not set opts.PageCookieHeader to the Auth's PageCookieHeader method value", site.fn, site.file)
		}
	}
}

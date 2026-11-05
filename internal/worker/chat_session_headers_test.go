package worker

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestChatDownloadersGetTheSessionHeadersGetter pins the worker's half of the
// chat identity headers, which a unit test cannot drive (the chat API's
// endpoints are constants): both chat-start sites hand the downloader the
// youtube Service's ChatSessionHeaders as a METHOD VALUE, so every poll reads
// the account's session at call time — never a map built from the page's
// ytcfg at setup, which a capture outlives across an account switch.
// internal/chat's TestFetchChatSendsTheSessionHeaders covers the downloader's
// side, internal/youtube's TestChatSessionHeadersFollowTheJarsAccount the
// getter's.
//
// Mutant: either site leaving opts.SessionHeaders unset, or setting it to a
// closure over a snapshot.
func TestChatDownloadersGetTheSessionHeadersGetter(t *testing.T) {
	for _, site := range []struct{ file, fn, recv string }{
		{"orchestrator_chat.go", "setupChatDownloader", "YT"},
		{"stream_processor_youtube.go", "tryStartEarlyChat", "yt"},
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
			if !ok || lhs.Sel.Name != "SessionHeaders" {
				return true
			}
			rhs, ok := assign.Rhs[0].(*ast.SelectorExpr)
			if !ok || rhs.Sel.Name != "ChatSessionHeaders" {
				return true
			}
			if recv, ok := rhs.X.(*ast.SelectorExpr); ok && recv.Sel.Name == site.recv {
				found = true
			}
			return true
		})
		if !found {
			t.Errorf("%s (%s) does not set opts.SessionHeaders to the youtube Service's ChatSessionHeaders method value", site.fn, site.file)
		}
	}
}

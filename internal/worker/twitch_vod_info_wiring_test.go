package worker

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestProcessJobWiresTheVodInfoReRead pins the production value of the
// TwitchVariantInfo.VodInfoFn seam. TestWireTwitchVodInfo tests the helper,
// but without processJob's call to it every VOD reached ExecuteTwitch with a
// nil VodInfoFn: twitchVodEnded answers "not ended" forever again (a VOD
// whose playlist never gains ENDLIST holds its slot) and refreshRecordedVodInfo
// never runs — G34/G58 reverted on the worker side with every other test
// green (the W18 review's mutant).
//
// Structural, because driving processJob into ExecuteTwitch needs a live
// twitch.Service: what is held here is that the function that calls
// ExecuteTwitch first calls wireTwitchVodInfo(variant, job, result,
// w.tw.GetVodInfo) on the same variant it hands ExecuteTwitch.
//
// Mutants: the wireTwitchVodInfo call removed; it handed a different
// getVodInfo; it placed after ExecuteTwitch.
func TestProcessJobWiresTheVodInfoReRead(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "worker.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		var exec *ast.CallExpr
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "ExecuteTwitch" && len(call.Args) >= 3 {
					exec = call
				}
			}
			return true
		})
		if exec == nil {
			continue
		}
		found++
		variant, _ := exec.Args[2].(*ast.Ident)
		if variant == nil {
			t.Fatalf("%s: ExecuteTwitch's variant argument is not an identifier", fset.Position(exec.Pos()))
		}
		wired := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call := identCall(n, "wireTwitchVodInfo")
			if call == nil || len(call.Args) != 4 || call.Pos() > exec.Pos() {
				return true
			}
			v, _ := call.Args[0].(*ast.Ident)
			get, _ := call.Args[3].(*ast.SelectorExpr)
			if v == nil || v.Name != variant.Name || get == nil || get.Sel.Name != "GetVodInfo" {
				return true
			}
			if recv, ok := get.X.(*ast.SelectorExpr); ok && recv.Sel.Name == "tw" {
				wired = true
			}
			return true
		})
		if !wired {
			t.Errorf("%s (%s): ExecuteTwitch is reached without wireTwitchVodInfo(%s, …, w.tw.GetVodInfo) ahead of it",
				fn.Name.Name, fset.Position(exec.Pos()), variant.Name)
		}
	}
	if found != 1 {
		t.Errorf("found %d functions in worker.go calling ExecuteTwitch, want 1 — the pin no longer sees the site it guards", found)
	}
}

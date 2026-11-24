package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// A branch on the vendor names the vendor it is about. "Codex, else Claude
// Code" read the other branch as one vendor, and the third made every such
// else a silent wrong answer: Antigravity would have been read as Claude
// Code — its Keychain, its probes, its identity file. So no code compares a
// vendor with Codex or Antigravity; it switches, and each case says what that
// vendor gets. A comparison with Claude Code stays: it selects what only
// Claude Code has (its probe, its session registry, its model limits), which
// every other vendor rightly goes without.
func TestNoVendorBranchAssumesTwo(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "testdata" || (strings.HasPrefix(name, ".") && path != root) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		inConfig := file.Name.Name == "config"
		ast.Inspect(file, func(n ast.Node) bool {
			be, ok := n.(*ast.BinaryExpr)
			if !ok || (be.Op != token.EQL && be.Op != token.NEQ) {
				return true
			}
			for _, side := range []ast.Expr{be.X, be.Y} {
				if name := vendorConst(side, inConfig); name == "Codex" || name == "Agy" {
					t.Errorf("%s: compares a vendor with %s — switch on the vendor and name each case", fset.Position(be.Pos()), name)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// vendorConst is the vendor constant e names — config.X outside the package,
// a bare X inside it — or "".
func vendorConst(e ast.Expr, inConfig bool) string {
	switch x := e.(type) {
	case *ast.SelectorExpr:
		if pkg, ok := x.X.(*ast.Ident); ok && pkg.Name == "config" {
			return x.Sel.Name
		}
	case *ast.Ident:
		if inConfig {
			return x.Name
		}
	}
	return ""
}

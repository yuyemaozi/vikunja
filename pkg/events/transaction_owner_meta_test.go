// Vikunja is a to-do list application to facilitate your life.
// Copyright 2018-present Vikunja and contributors. All rights reserved.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package events

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"strings"
	"testing"
)

// Each publishing transaction owner must bind before invoking business code.
// Helpers returning sessions are audited separately; read-only sessions are not
// required to allocate a transaction queue.
func TestRequestMetaTransactionOwnersBindBeforePublish(t *testing.T) {
	roots := []string{"../routes/api", "../routes/caldav", "../modules/auth/openid", "../modules/avatar", "../modules/migration", "../cmd", "../user", "../models"}
	for _, root := range roots {
		handle, err := os.OpenRoot(root)
		if err != nil {
			t.Fatal(err)
		}
		defer handle.Close()
		err = fs.WalkDir(handle.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			source, err := handle.ReadFile(path)
			if err != nil {
				return err
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, source, 0)
			if err != nil {
				return err
			}
			ast.Inspect(file, func(node ast.Node) bool {
				var body *ast.BlockStmt
				switch n := node.(type) {
				case *ast.FuncDecl:
					body = n.Body
				case *ast.FuncLit:
					body = n.Body
				default:
					return true
				}
				if body == nil {
					return true
				}
				text := string(source[fset.Position(body.Pos()).Offset:fset.Position(body.End()).Offset])
				ast.Inspect(body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "DispatchPending" || len(call.Args) != 2 {
						return true
					}
					session, ok := call.Args[1].(*ast.Ident)
					if !ok {
						return true
					}
					// A returned session is bound in its creator, not by read-only callers.
					if !strings.Contains(text, session.Name+" := db.NewSession()") && !strings.Contains(text, session.Name+" = db.NewSession()") {
						return true
					}
					ctx := string(source[fset.Position(call.Args[0].Pos()).Offset:fset.Position(call.Args[0].End()).Offset])
					want := "events.BindRequestMeta(" + session.Name + ", " + ctx + ")"
					if !strings.Contains(text, want) {
						t.Errorf("%s: transaction %s missing %s", fset.Position(call.Pos()), session.Name, want)
					}
					if !strings.Contains(text, "defer events.CleanupPending("+session.Name+")") {
						t.Errorf("%s: transaction %s missing deferred cleanup", fset.Position(call.Pos()), session.Name)
					}
					return true
				})
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

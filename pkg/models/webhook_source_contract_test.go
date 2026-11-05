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

package models

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Registration alone is insufficient: every catalogue source must actually use
// the synchronous sink, and no producer may silently ignore its returned error.
func TestOutboxSourceProducerCoverage(t *testing.T) {
	sources := map[string]bool{}
	for _, event := range webhookSourceEvents {
		sources[reflect.TypeOf(event).Elem().Name()] = false
	}
	files := []string{"tasks.go", "kanban_task_bucket.go", "task_assignees.go", "task_attachment.go", "task_comments.go", "task_relation.go", "project.go", "project_users.go", "project_team.go", "task_reminder.go", "task_overdue_reminder.go", "label_task.go", "task_position.go"}
	for _, name := range files {
		source, err := os.ReadFile(name)
		require.NoError(t, err)
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, source, 0)
		require.NoError(t, err)
		ast.Inspect(file, func(node ast.Node) bool {
			if statement, ok := node.(*ast.ExprStmt); ok {
				if call, ok := statement.X.(*ast.CallExpr); ok && webhookDispatchFunction(call) == "DispatchOnCommit" {
					t.Errorf("%s ignores transactional enqueue error", fset.Position(call.Pos()))
				}
			}
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			function := webhookDispatchFunction(call)
			if function == "" {
				return true
			}
			for _, arg := range call.Args {
				pointer, ok := arg.(*ast.UnaryExpr)
				if !ok {
					continue
				}
				literal, ok := pointer.X.(*ast.CompositeLit)
				if !ok {
					continue
				}
				event, ok := literal.Type.(*ast.Ident)
				if !ok {
					continue
				}
				if _, registered := sources[event.Name]; registered {
					assert.Equal(t, "DispatchOnCommit", function, "%s bypasses the transactional sink for %s", fset.Position(call.Pos()), event.Name)
					sources[event.Name] = true
				}
			}
			return true
		})
	}
	for name, seen := range sources {
		assert.True(t, seen, "registered source %s has no checked transactional producer", name)
	}
}

func webhookDispatchFunction(call *ast.CallExpr) string {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	receiver, ok := selector.X.(*ast.Ident)
	if !ok || receiver.Name != "events" {
		return ""
	}
	switch selector.Sel.Name {
	case "DispatchOnCommit", "Dispatch", "DispatchWithContext":
		return selector.Sel.Name
	default:
		return ""
	}
}

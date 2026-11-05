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

package migration

import (
	"strings"
	"testing"

	"code.vikunja.io/api/pkg/db"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"xorm.io/xorm"
	"xorm.io/xorm/names"
	"xorm.io/xorm/schemas"
)

// A renamed PostgreSQL table keeps schema-wide index names. Give destructive
// legacy-schema fixtures a separate schema instead of colliding with real tables.
func webhookMigrationTestEngine(t *testing.T) *xorm.Engine {
	t.Helper()
	parent, err := db.CreateTestEngine()
	require.NoError(t, err)
	if parent.Dialect().URI().DBType != schemas.POSTGRES && parent.Dialect().URI().DBType != schemas.SQLITE {
		return parent
	}
	dsn := parent.DataSourceName()
	schema := "webhook_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if parent.Dialect().URI().DBType == schemas.SQLITE {
		dsn = "file:" + schema + "?mode=memory&cache=shared"
	}
	engine, err := xorm.NewEngine(parent.DriverName(), dsn)
	require.NoError(t, err)
	engine.SetMapper(names.GonicMapper{})
	engine.SetMaxOpenConns(1)
	engine.SetMaxIdleConns(1)
	t.Cleanup(func() { require.NoError(t, engine.Close()) })
	if parent.Dialect().URI().DBType == schemas.POSTGRES {
		_, err = parent.Exec("CREATE SCHEMA " + schema)
		require.NoError(t, err)
		t.Cleanup(func() { _, err := parent.Exec("DROP SCHEMA " + schema + " CASCADE"); require.NoError(t, err) })
		_, err = engine.Exec("SET search_path TO " + schema)
		require.NoError(t, err)
		engine.SetSchema(schema)
	}
	return engine
}

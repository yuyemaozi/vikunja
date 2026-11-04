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
	"testing"
	"time"

	"code.vikunja.io/api/pkg/db"
	"code.vikunja.io/api/pkg/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateWebhookDeliveries20261103000100(t *testing.T) {
	x, err := db.CreateTestEngine()
	require.NoError(t, err)

	// Best effort: the table may not exist in a fresh in-memory engine.
	_ = x.DropTables(&webhookDeliveries20261103000100{})

	require.NoError(t, migrateWebhookDeliveries20261103000100(x))

	exists, err := x.IsTableExist("webhook_deliveries")
	require.NoError(t, err)
	assert.True(t, exists)

	record := &webhookDeliveries20261103000100{
		DeliveryID: "00000000-0000-0000-0000-000000000001",
		WebhookID:  1,
		EventName:  "task.updated",
		Payload: &models.WebhookPayload{
			EventName: "task.updated",
			Time:      time.Now(),
			Data:      map[string]interface{}{},
		},
		Status: "pending",
	}
	_, err = x.Insert(record)
	require.NoError(t, err)

	duplicate := *record
	duplicate.ID = 0
	_, err = x.Insert(&duplicate)
	require.Error(t, err, "delivery id must be unique")

	t.Cleanup(func() {
		_, err := x.Delete(&webhookDeliveries20261103000100{DeliveryID: record.DeliveryID})
		require.NoError(t, err)
	})
}

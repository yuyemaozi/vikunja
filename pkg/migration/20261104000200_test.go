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
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type legacyDelivery20261104000200 struct {
	ID           int64     `xorm:"bigint autoincr not null unique pk"`
	DeliveryID   string    `xorm:"varchar(36) not null unique"`
	WebhookID    int64     `xorm:"bigint not null"`
	EventName    string    `xorm:"varchar(255) not null"`
	Payload      string    `xorm:"JSON not null"`
	Status       string    `xorm:"varchar(16) not null"`
	AttemptCount int       `xorm:"int not null default 0"`
	CreatedAt    time.Time `xorm:"created not null"`
}

func (legacyDelivery20261104000200) TableName() string {
	return "webhook_deliveries"
}

func TestWebhookMigration002ResetsLegacyProcessing(t *testing.T) {
	x := webhookMigrationTestEngine(t)
	var err error

	const backup = "webhook_deliveries_backup_20261104000200"
	existed, err := x.IsTableExist("webhook_deliveries")
	require.NoError(t, err)
	sqlite := x.Dialect().URI().DBType == "sqlite3"
	if existed {
		if sqlite {
			require.NoError(t, x.DropTables(&legacyDelivery20261104000200{}))
		} else {
			_, renameErr := x.Exec("ALTER TABLE webhook_deliveries RENAME TO " + backup)
			require.NoError(t, renameErr)
		}
	}
	t.Cleanup(func() {
		require.NoError(t, x.DropTables(&legacyDelivery20261104000200{}))
		if existed && !sqlite {
			_, restoreErr := x.Exec("ALTER TABLE " + backup + " RENAME TO webhook_deliveries")
			require.NoError(t, restoreErr)
		}
	})
	require.NoError(t, x.Sync2(&legacyDelivery20261104000200{}))

	statuses := []string{"pending", "failed", "delivered", "cancelled", "processing"}
	for index, status := range statuses {
		_, err := x.Exec(
			"INSERT INTO webhook_deliveries (delivery_id, webhook_id, event_name, payload, status, attempt_count, created_at) "+
				"VALUES (?, 1, 'task.updated', '{}', ?, ?, CURRENT_TIMESTAMP)",
			"00000000-0000-0000-0000-00000000000"+string(rune('0'+index)),
			status,
			7+index,
		)
		require.NoError(t, err)
	}

	require.NoError(t, migrateWebhookDeliveriesLease20261104000200(x))

	expected := map[string]int64{"pending": 2, "failed": 1, "delivered": 1, "cancelled": 1}
	for status, want := range expected {
		count, err := x.Where("status = ?", status).Count(&legacyDelivery20261104000200{})
		require.NoError(t, err)
		assert.Equal(t, want, count, status)
	}
	processing, err := x.Where("status = ?", "processing").Count(&legacyDelivery20261104000200{})
	require.NoError(t, err)
	assert.Zero(t, processing)
	generation, err := x.Where("status = ?", "pending").Cols("retry_generation").Get(&legacyDelivery20261104000200{})
	require.NoError(t, err)
	assert.True(t, generation)
	rows, err := x.QueryString("SELECT status, attempt_count, cycle_attempt_count, retry_generation FROM webhook_deliveries ORDER BY delivery_id")
	require.NoError(t, err)
	require.Len(t, rows, len(statuses))
	for index, row := range rows {
		assert.Equal(t, strconv.Itoa(7+index), row["attempt_count"])
		assert.Equal(t, "0", row["cycle_attempt_count"])
		assert.Equal(t, "0", row["retry_generation"])
	}
}

func TestWebhookMigration002PreservesValidLeaseOnRerun(t *testing.T) {
	x := webhookMigrationTestEngine(t)
	require.NoError(t, x.Sync2(&legacyDelivery20261104000200{})) //nolint:forbidigo // Brand-new table in an isolated test database.
	require.NoError(t, migrateWebhookDeliveriesLease20261104000200(x))
	_, err := x.Exec("INSERT INTO webhook_deliveries (delivery_id,webhook_id,event_name,payload,status,created_at,lease_token,lease_until,cycle_attempt_count,retry_generation) VALUES (?,1,'task.updated','{}','processing',CURRENT_TIMESTAMP,?,?,5,3)", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "active-token", time.Now().Add(time.Minute))
	require.NoError(t, err)
	require.NoError(t, migrateWebhookDeliveriesLease20261104000200(x))
	rows, err := x.QueryString("SELECT status,cycle_attempt_count,lease_token,retry_generation FROM webhook_deliveries WHERE delivery_id=?", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "processing", rows[0]["status"])
	assert.Equal(t, "5", rows[0]["cycle_attempt_count"])
	assert.Equal(t, "active-token", rows[0]["lease_token"])
	assert.Equal(t, "3", rows[0]["retry_generation"])
}

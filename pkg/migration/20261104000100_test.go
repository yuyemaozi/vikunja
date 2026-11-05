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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type legacyWebhookDelivery20261104000100 struct {
	ID            int64     `xorm:"bigint autoincr not null unique pk"`
	DeliveryID    string    `xorm:"varchar(36) not null unique"`
	WebhookID     int64     `xorm:"bigint not null"`
	EventName     string    `xorm:"varchar(255) not null"`
	Payload       string    `xorm:"JSON not null"`
	Status        string    `xorm:"varchar(16) not null"`
	AttemptCount  int       `xorm:"int not null default 0"`
	LastErrorCode string    `xorm:"varchar(64) null"`
	CreatedAt     time.Time `xorm:"created not null"`
}

func (legacyWebhookDelivery20261104000100) TableName() string {
	return "webhook_deliveries"
}

func TestWebhookMigration001PreservesLegacyRows(t *testing.T) {
	x := webhookMigrationTestEngine(t)
	var err error

	const backup = "webhook_deliveries_backup_20261104000100"
	existed, err := x.IsTableExist("webhook_deliveries")
	require.NoError(t, err)
	sqlite := x.Dialect().URI().DBType == "sqlite3"
	if existed {
		if sqlite {
			require.NoError(t, x.DropTables(&legacyWebhookDelivery20261104000100{}))
		} else {
			_, renameErr := x.Exec("ALTER TABLE webhook_deliveries RENAME TO " + backup)
			require.NoError(t, renameErr)
		}
	}
	t.Cleanup(func() {
		require.NoError(t, x.DropTables(&legacyWebhookDelivery20261104000100{}))
		if existed && !sqlite {
			_, restoreErr := x.Exec("ALTER TABLE " + backup + " RENAME TO webhook_deliveries")
			require.NoError(t, restoreErr)
		}
	})
	require.NoError(t, x.Sync2(&legacyWebhookDelivery20261104000100{}))

	legacy := &legacyWebhookDelivery20261104000100{
		DeliveryID: "11111111-1111-1111-1111-111111111111",
		WebhookID:  1,
		EventName:  "task.updated",
		Payload:    "{}",
		Status:     "pending",
	}
	_, err = x.Insert(legacy)
	require.NoError(t, err)

	require.NoError(t, migrateWebhookDeliveriesStaging20261104000100(x))

	placeholder := "?"
	if x.Dialect().URI().DBType == "postgres" {
		placeholder = "$1"
	}
	var eventID *string
	err = x.DB().QueryRow(
		"SELECT event_id FROM webhook_deliveries WHERE delivery_id = "+placeholder,
		legacy.DeliveryID,
	).Scan(&eventID)
	require.NoError(t, err)
	assert.Nil(t, eventID)

	insertStaged := func(deliveryID string) error {
		_, err := x.Exec(
			"INSERT INTO webhook_deliveries "+
				"(delivery_id, webhook_id, event_name, payload, status, event_id, created_at) "+
				"VALUES (?, 2, 'task.updated', '{}', 'pending', "+
				"'22222222-2222-2222-2222-222222222222', CURRENT_TIMESTAMP)",
			deliveryID,
		)
		return err
	}
	require.NoError(t, insertStaged("33333333-3333-3333-3333-333333333333"))
	assert.Error(t, insertStaged("44444444-4444-4444-4444-444444444444"))

}

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
	"fmt"

	"src.techknowlogick.com/xormigrate"
	"xorm.io/xorm"
)

type webhookDeliveriesStaging20261104000100 struct {
	EventID       string `xorm:"varchar(36) null"`
	PayloadBody   []byte `xorm:"blob null"`
	PayloadSHA256 string `xorm:"varchar(64) null"`
}

func (webhookDeliveriesStaging20261104000100) TableName() string {
	return "webhook_deliveries"
}

func migrateWebhookDeliveriesStaging20261104000100(x *xorm.Engine) error {
	if err := partialSync(x, &webhookDeliveriesStaging20261104000100{}); err != nil {
		return err
	}

	const index = "CREATE UNIQUE INDEX IF NOT EXISTS idx_webhook_deliveries_event_webhook " +
		"ON webhook_deliveries (event_id, webhook_id)"
	switch x.Dialect().URI().DBType {
	case "postgres", "sqlite3":
		_, err := x.Exec(index)
		return err
	case "mysql":
		// MySQL has no CREATE INDEX IF NOT EXISTS; a duplicate name is expected on re-run.
		if _, err := x.Exec(
			"CREATE UNIQUE INDEX idx_webhook_deliveries_event_webhook " +
				"ON webhook_deliveries (event_id, webhook_id)",
		); err != nil {
			return nil
		}
		return nil
	default:
		return fmt.Errorf("unsupported database for webhook staging migration")
	}
}

func init() {
	migrations = append(migrations, &xormigrate.Migration{
		ID:          "20261104000100",
		Description: "Stage transactional webhook delivery snapshots",
		Migrate:     migrateWebhookDeliveriesStaging20261104000100,
		Rollback:    nil,
	})
}

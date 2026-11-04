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
	"time"

	"src.techknowlogick.com/xormigrate"
	"xorm.io/xorm"
)

type webhookDeliveriesLease20261104000200 struct {
	LeaseToken string     `xorm:"varchar(36) null"`
	LeaseUntil *time.Time `xorm:"timestamp null"`
	Generation int        `xorm:"int not null default 0"`
}

func (webhookDeliveriesLease20261104000200) TableName() string {
	return "webhook_deliveries"
}

func migrateWebhookDeliveriesLease20261104000200(x *xorm.Engine) error {
	return partialSync(x, &webhookDeliveriesLease20261104000200{})
}

func init() {
	migrations = append(migrations, &xormigrate.Migration{
		ID:          "20261104000200",
		Description: "Add delivery lease and generation columns",
		Migrate:     migrateWebhookDeliveriesLease20261104000200,
		Rollback:    nil,
	})
}

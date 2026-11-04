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

	"code.vikunja.io/api/pkg/models"

	"src.techknowlogick.com/xormigrate"
	"xorm.io/xorm"
)

type webhookDeliveries20261103000100 struct {
	ID            int64                  `xorm:"bigint autoincr not null unique pk"`
	DeliveryID    string                 `xorm:"varchar(36) not null unique"`
	WebhookID     int64                  `xorm:"bigint not null index"`
	EventName     string                 `xorm:"varchar(255) not null"`
	Payload       *models.WebhookPayload `xorm:"JSON not null"`
	Status        string                 `xorm:"varchar(16) not null index"`
	AttemptCount  int                    `xorm:"int not null default 0"`
	LastErrorCode string                 `xorm:"varchar(64) null"`
	CreatedAt     time.Time              `xorm:"created not null"`
	LastAttemptAt *time.Time             `xorm:"null"`
	DeliveredAt   *time.Time             `xorm:"null"`
	FailedAt      *time.Time             `xorm:"null"`
}

func (webhookDeliveries20261103000100) TableName() string {
	return "webhook_deliveries"
}

func migrateWebhookDeliveries20261103000100(x *xorm.Engine) error {
	return x.Sync2(&webhookDeliveries20261103000100{})
}

func init() {
	migrations = append(migrations, &xormigrate.Migration{
		ID:          "20261103000100",
		Description: "Create durable webhook delivery outbox",
		Migrate:     migrateWebhookDeliveries20261103000100,
		Rollback: func(x *xorm.Engine) error {
			return x.DropTables(&webhookDeliveries20261103000100{})
		},
	})
}

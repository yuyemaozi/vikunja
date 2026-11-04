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
	LeaseToken        string     `xorm:"varchar(36) null"`
	LeaseUntil        *time.Time `xorm:"timestamp null"`
	RetryGeneration   int        `xorm:"retry_generation int not null default 0"`
	CycleAttemptCount int        `xorm:"cycle_attempt_count int not null default 0"`
	NextAttemptAt     *time.Time `xorm:"next_attempt_at timestamp null"`
}

func (webhookDeliveriesLease20261104000200) TableName() string {
	return "webhook_deliveries"
}

func migrateWebhookDeliveriesLease20261104000200(x *xorm.Engine) error {
	if err := partialSync(x, &webhookDeliveriesLease20261104000200{}); err != nil {
		return err
	}
	// Legacy processing rows have no lease and cannot be fenced; make them claimable.
	_, err := x.Exec(
		"UPDATE webhook_deliveries SET status = 'pending', cycle_attempt_count = 0 " +
			"WHERE status = 'processing'",
	)
	return err
}

func init() {
	migrations = append(migrations, &xormigrate.Migration{
		ID:          "20261104000200",
		Description: "Add delivery lease and generation columns",
		Migrate:     migrateWebhookDeliveriesLease20261104000200,
		Rollback:    nil,
	})
}

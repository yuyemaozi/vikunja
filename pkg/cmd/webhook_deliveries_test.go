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

package cmd

import (
	"bytes"
	"os"
	"testing"
	"time"

	"code.vikunja.io/api/pkg/config"
	"code.vikunja.io/api/pkg/db"
	"code.vikunja.io/api/pkg/log"
	"code.vikunja.io/api/pkg/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	log.InitLogger()
	config.InitDefaultConfig()
	os.Exit(m.Run())
}

func TestWebhookDeliveriesListAndRetry(t *testing.T) {
	engine, err := db.CreateTestEngine()
	require.NoError(t, err)
	require.NoError(t, engine.Sync2(&models.WebhookDeliveryRecord{}))

	s := engine.NewSession()
	record, err := models.CreateWebhookDelivery(s, 1, &models.WebhookPayload{
		EventName: "task.updated",
		Time:      time.Now(),
		Data:      map[string]interface{}{},
	})
	require.NoError(t, err)
	require.NoError(t, s.Commit())
	_ = s.Close()
	t.Cleanup(func() {
		_, deleteErr := engine.Delete(&models.WebhookDeliveryRecord{DeliveryID: record.DeliveryID})
		require.NoError(t, deleteErr)
	})

	require.NoError(t, models.MarkWebhookDeliveryPoisoned(record.DeliveryID))

	s = engine.NewSession()
	records, err := models.ListWebhookDeliveries(s, nil)
	_ = s.Close()
	require.NoError(t, err)

	var buffer bytes.Buffer
	printWebhookDeliveries(&buffer, records)
	assert.Contains(t, buffer.String(), record.DeliveryID)
	assert.Contains(t, buffer.String(), "failed")
	assert.NotContains(t, buffer.String(), "payload")

	s = engine.NewSession()
	requeued, err := models.RetryWebhookDelivery(s, record.DeliveryID)
	_ = s.Close()
	require.NoError(t, err)
	assert.True(t, requeued)
}

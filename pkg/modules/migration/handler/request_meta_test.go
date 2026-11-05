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

package handler

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"code.vikunja.io/api/pkg/config"
	"code.vikunja.io/api/pkg/db"
	"code.vikunja.io/api/pkg/events"
	"code.vikunja.io/api/pkg/models"
	"code.vikunja.io/api/pkg/modules/migration"
	vikunjafile "code.vikunja.io/api/pkg/modules/migration/vikunja-file"
	"code.vikunja.io/api/pkg/notifications"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/stretchr/testify/require"
)

func TestFileMigrationRequestMetaReachesTransactionOutbox(t *testing.T) {
	clearMigrationStatus(t)
	notifications.Fake()
	t.Cleanup(notifications.Unfake)
	old := config.WebhooksEnabled.GetBool()
	config.WebhooksEnabled.Set(true)
	t.Cleanup(func() { config.WebhooksEnabled.Set(old) })
	require.NoError(t, models.RegisterWebhookOutbox())
	RegisterFileMigrator(func() migration.FileMigrator { return &vikunjafile.FileMigrator{} })
	u := getTestUser(t)
	s := db.NewSession()
	// Reserve the current sequence value so this remains valid after other fixture tests.
	probe := &models.Project{Title: "request-meta-sequence-probe", OwnerID: u.ID}
	_, err := s.Insert(probe)
	require.NoError(t, err)
	nextProjectID := probe.ID + 1
	hook := &models.Webhook{ProjectID: nextProjectID, CreatedByID: u.ID, TargetURL: "https://example.invalid/import", Events: []string{"task.created"}}
	_, err = s.Insert(hook)
	require.NoError(t, err)
	var baseline []models.WebhookDeliveryRecord
	require.NoError(t, s.Where("webhook_id = ?", hook.ID).Find(&baseline))
	previous := map[string]bool{}
	for _, row := range baseline {
		previous[row.DeliveryID] = true
	}
	require.NoError(t, s.Commit())
	require.NoError(t, s.Close())
	data, err := os.ReadFile("../vikunja-file/export.zip")
	require.NoError(t, err)
	status, err := migration.ClaimMigration(&vikunjafile.FileMigrator{}, u)
	require.NoError(t, err)
	storeTestUpload(t, status, data)
	payload, err := json.Marshal(&FileMigrationRequestedEvent{User: u, MigratorKind: "vikunja-file", MigrationStatusID: status.ID})
	require.NoError(t, err)
	msg := message.NewMessage("request-meta-import", payload)
	const requestID = "018f8f4c-2e18-7b61-9a50-9c4f5de4e10b"
	msg.Metadata.Set(events.MetadataKeyRequestID, requestID)
	require.NoError(t, (&FileMigrationListener{}).Handle(msg))
	read := db.NewSession()
	defer read.Close()
	var rows []models.WebhookDeliveryRecord
	require.NoError(t, read.Where("webhook_id = ?", hook.ID).Find(&rows))
	newRows := 0
	for _, row := range rows {
		if previous[row.DeliveryID] {
			continue
		}
		newRows++
		require.Equal(t, requestID, row.RequestID)
		require.NotEmpty(t, row.PayloadBody)
	}
	require.Positive(t, newRows)
}

func TestMigrationRequestMetaPreservesMessageContext(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	original := &events.RequestMeta{RequestID: "original", IP: "192.0.2.1", UserAgent: "synthetic-agent"}
	msg := message.NewMessage("metadata", nil)
	msg.SetContext(events.WithRequestMeta(base, original))
	const requestID = "018f8f4c-2e18-7b61-9a50-9c4f5de4e10b"
	msg.Metadata.Set(events.MetadataKeyRequestID, requestID)
	ctx := migrationRequestContext(msg)
	meta := events.RequestMetaFromContext(ctx)
	require.Equal(t, requestID, meta.RequestID)
	require.Equal(t, original.IP, meta.IP)
	require.Equal(t, original.UserAgent, meta.UserAgent)
	require.Equal(t, "original", original.RequestID)
	cancel()
	require.ErrorIs(t, ctx.Err(), context.Canceled)
}

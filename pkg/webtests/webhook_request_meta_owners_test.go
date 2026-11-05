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

package webtests

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"code.vikunja.io/api/pkg/config"
	"code.vikunja.io/api/pkg/db"
	"code.vikunja.io/api/pkg/models"
	"github.com/stretchr/testify/require"
)

// The outbox is read from the database before any asynchronous listener runs.
// A request binding only in the sender cannot make this test pass.
func TestRequestMetaAttachmentOwnersPersistHTTPUUID(t *testing.T) {
	for _, version := range []string{"v1", "v2"} {
		t.Run(version, func(t *testing.T) {
			e, err := setupTestEnv()
			require.NoError(t, err)
			config.WebhooksEnabled.Set(true)
			require.NoError(t, models.RegisterWebhookOutbox())
			s := db.NewSession()
			hook := &models.Webhook{ProjectID: 1, CreatedByID: 1, TargetURL: "https://example.invalid/hook", Events: []string{"task.attachment.created"}}
			_, err = s.Insert(hook)
			require.NoError(t, err)
			require.NoError(t, s.Commit())
			require.NoError(t, s.Close())
			body, ct := multipartFilesBody(t, map[string][]byte{"meta.txt": []byte("synthetic")})
			method := http.MethodPost
			if version == "v1" {
				method = http.MethodPut
			}
			req := httptest.NewRequest(method, "/api/"+version+"/tasks/1/attachments", body)
			req.Header.Set("Content-Type", ct)
			req.Header.Set("Authorization", "Bearer "+humaTokenFor(t, &testuser1))
			const requestID = "018f8f4c-2e18-7b61-9a50-9c4f5de4e10b"
			req.Header.Set("X-Request-ID", requestID)
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			if version == "v1" {
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			} else {
				require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
			}
			require.Equal(t, requestID, rec.Header().Get("X-Request-ID"))
			read := db.NewSession()
			defer read.Close()
			var rows []models.WebhookDeliveryRecord
			require.NoError(t, read.Where("webhook_id = ?", hook.ID).Find(&rows))
			require.Len(t, rows, 1)
			require.Equal(t, requestID, rows[0].RequestID)
			require.NotEmpty(t, rows[0].PayloadBody)
		})
	}
}

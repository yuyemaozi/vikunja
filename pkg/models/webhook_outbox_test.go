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

package models

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWebhookRegistryHasExactly19Sources(t *testing.T) {
	names := WebhookSourceEventNames()

	assert.Len(t, names, 19)
	seen := map[string]bool{}
	for _, name := range names {
		assert.False(t, seen[name], "duplicate source %s", name)
		seen[name] = true
	}
}

func TestRegisterWebhookOutboxIsIdempotent(t *testing.T) {
	assert.NoError(t, RegisterWebhookOutbox())
	assert.NoError(t, RegisterWebhookOutbox())
}

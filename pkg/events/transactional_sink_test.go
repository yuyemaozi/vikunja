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

package events

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type transactionalTestEvent struct{}

func (*transactionalTestEvent) Name() string { return "webhook.test.prepared" }

func TestTransactionalSinkRegistration(t *testing.T) {
	resetTransactionalSinks()
	t.Cleanup(resetTransactionalSinks)
	sink := func(any, PreparedEvent, RequestMeta) ([]Event, error) { return nil, nil }

	require.NoError(t, RegisterTransactionalSink([]string{(&transactionalTestEvent{}).Name()}, sink))
	assert.Error(t, RegisterTransactionalSink([]string{(&transactionalTestEvent{}).Name()}, sink))
}

func TestRequestMetaBoundBeforeSnapshot(t *testing.T) {
	key := new(int)
	const requestID = "018f8f4c-2e18-7b61-9a50-9c4f5de4e10b"

	BindRequestMeta(
		key,
		WithRequestMeta(context.Background(), &RequestMeta{RequestID: requestID}),
	)
	BindRequestMeta(
		key,
		WithRequestMeta(
			context.Background(),
			&RequestMeta{RequestID: "11111111-1111-1111-1111-111111111111"},
		),
	)

	assert.Equal(t, requestID, pendingQueue(key).requestID)
}

func TestNonWebhookEventsKeepOriginalBehavior(t *testing.T) {
	resetTransactionalSinks()
	t.Cleanup(resetTransactionalSinks)
	key := new(int)

	require.NoError(t, DispatchOnCommit(key, &transactionalTestEvent{}))
	assert.Len(t, pendingQueue(key).events, 1)
}

func TestTransactionalSinkRejectsAutocommit(t *testing.T) {
	resetTransactionalSinks()
	t.Cleanup(resetTransactionalSinks)
	sink := func(any, PreparedEvent, RequestMeta) ([]Event, error) { return nil, nil }
	require.NoError(t, RegisterTransactionalSink([]string{(&transactionalTestEvent{}).Name()}, sink))

	err := DispatchOnCommit(new(int), &transactionalTestEvent{})
	assert.ErrorIs(t, err, ErrWebhookTransactionRequired)
}

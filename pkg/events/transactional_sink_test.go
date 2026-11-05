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
	"errors"

	"testing"

	"code.vikunja.io/api/pkg/config"
	"github.com/google/uuid"
	_ "github.com/mattn/go-sqlite3"
	"xorm.io/xorm"

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
	previous := config.WebhooksEnabled.GetBool()
	config.WebhooksEnabled.Set(true)
	t.Cleanup(func() { config.WebhooksEnabled.Set(previous) })
	resetTransactionalSinks()
	t.Cleanup(resetTransactionalSinks)
	sink := func(any, PreparedEvent, RequestMeta) ([]Event, error) { return nil, nil }
	require.NoError(t, RegisterTransactionalSink([]string{(&transactionalTestEvent{}).Name()}, sink))

	err := DispatchOnCommit(new(int), &transactionalTestEvent{})
	assert.ErrorIs(t, err, ErrWebhookTransactionRequired)
}

func TestTransactionRequestIDGeneratedOnce(t *testing.T) {
	resetTransactionalSinks()
	t.Cleanup(resetTransactionalSinks)
	key := new(int)

	BindRequestMeta(key, context.Background())
	first := pendingQueue(key).requestID
	require.NotEmpty(t, first)

	require.NoError(t, DispatchOnCommit(key, &transactionalTestEvent{}))
	require.NoError(t, DispatchOnCommit(key, &transactionalTestEvent{}))
	assert.Equal(t, first, pendingQueue(key).requestID)
	CleanupPending(key)
}

func TestBoundRequestIDIsNotReplaced(t *testing.T) {
	key := new(int)
	defer CleanupPending(key)

	const first = "018f8f4c-2e18-7b61-9a50-9c4f5de4e10b"
	const second = "11111111-1111-1111-1111-111111111111"
	BindRequestMeta(key, WithRequestMeta(context.Background(), &RequestMeta{RequestID: first}))
	BindRequestMeta(key, WithRequestMeta(context.Background(), &RequestMeta{RequestID: second}))
	assert.Equal(t, first, pendingQueue(key).requestID)
}

func TestWebhookRegisteredSourceRejectsPublicDispatch(t *testing.T) {
	resetTransactionalSinks()
	t.Cleanup(resetTransactionalSinks)
	Fake()
	previous := config.WebhooksEnabled.GetBool()
	config.WebhooksEnabled.Set(true)
	t.Cleanup(func() { config.WebhooksEnabled.Set(previous) })
	require.NoError(t, RegisterTransactionalSink([]string{(&transactionalTestEvent{}).Name()}, func(any, PreparedEvent, RequestMeta) ([]Event, error) { return nil, nil }))
	require.ErrorIs(t, Dispatch(&transactionalTestEvent{}), ErrWebhookTransactionRequired)
	require.ErrorIs(t, DispatchWithContext(context.Background(), &transactionalTestEvent{}), ErrWebhookTransactionRequired)
	assert.Zero(t, CountDispatchedEvents((&transactionalTestEvent{}).Name()))
	config.WebhooksEnabled.Set(false)
	key := new(int)
	defer CleanupPending(key)
	require.NoError(t, DispatchOnCommit(key, &transactionalTestEvent{}))
	DispatchPending(context.Background(), key)
	require.NoError(t, Dispatch(&transactionalTestEvent{}))
	assert.Equal(t, 2, CountDispatchedEvents((&transactionalTestEvent{}).Name()))
}

type mutablePreparedTestEvent struct{ Value string }

func (*mutablePreparedTestEvent) Name() string { return "webhook.test.frozen" }

func TestTransactionalSinkFailureNeverEnqueues(t *testing.T) {
	resetTransactionalSinks()
	t.Cleanup(resetTransactionalSinks)
	Fake()
	previous := config.WebhooksEnabled.GetBool()
	config.WebhooksEnabled.Set(true)
	t.Cleanup(func() { config.WebhooksEnabled.Set(previous) })
	engine, err := xorm.NewEngine("sqlite3", ":memory:")
	require.NoError(t, err)
	defer engine.Close()
	session := engine.NewSession()
	defer session.Close()
	require.NoError(t, session.Begin())
	defer CleanupPending(session)
	const requestID = "018f8f4c-2e18-7b61-9a50-9c4f5de4e501"
	BindRequestMeta(session, WithRequestMeta(context.Background(), &RequestMeta{RequestID: requestID}))
	sentinel := errors.New("synthetic sink failure")
	event := &mutablePreparedTestEvent{Value: "before"}
	var frozen PreparedEvent
	require.NoError(t, RegisterTransactionalSink([]string{event.Name()}, func(key any, prepared PreparedEvent, meta RequestMeta) ([]Event, error) {
		assert.Same(t, session, key)
		assert.True(t, session.IsInTx())
		assert.Equal(t, requestID, meta.RequestID)
		_, parseErr := uuid.Parse(prepared.ID)
		assert.NoError(t, parseErr)
		assert.False(t, prepared.OccurredAt.IsZero())
		frozen = prepared
		return []Event{&testEvent{}}, sentinel
	}))
	require.ErrorIs(t, DispatchOnCommit(session, event), sentinel)
	event.Value = "after"
	assert.JSONEq(t, `{"Value":"before"}`, string(frozen.Payload))
	assert.Empty(t, pendingQueue(session).events)
	require.NoError(t, session.Rollback())
	CleanupPending(session)
	DispatchPending(context.Background(), session)
	assert.Zero(t, CountDispatchedEvents(event.Name()))
	assert.Zero(t, CountDispatchedEvents((&testEvent{}).Name()))
}

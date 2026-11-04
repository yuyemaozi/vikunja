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

package openid

import (
	"testing"
	"time"

	"code.vikunja.io/api/pkg/db"
	"code.vikunja.io/api/pkg/models"
	"code.vikunja.io/api/pkg/modules/keyvalue"
	"code.vikunja.io/api/pkg/user"

	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const identityTestPassword = "12345678"

func loadIdentityTestUser(t *testing.T, id int64) *user.User {
	t.Helper()
	s := db.NewSession()
	defer s.Close()
	u, err := user.GetUserByID(s, id)
	require.NoError(t, err)

	reset := func() {
		_ = keyvalue.Del(u.GetFailedPasswordAttemptsKey())
		_ = keyvalue.Del(u.GetFailedTOTPAttemptsKey())
	}
	reset()
	t.Cleanup(reset)
	return u
}

// enableTestTOTP gives userID its own secret. The fixture TOTP of user 10 and
// the secret of the linked-login test would collide on "passcode already used".
func enableTestTOTP(t *testing.T, userID int64) string {
	t.Helper()
	const secret = "JBSWY3DPEHPK3PXP"
	s := db.NewSession()
	defer s.Close()
	_, err := s.Insert(&user.TOTP{
		UserID:  userID,
		Secret:  secret,
		Enabled: true,
	})
	require.NoError(t, err)
	require.NoError(t, s.Commit())
	return secret
}

func failedAttempts(t *testing.T, key string) int64 {
	t.Helper()
	attempts, _, err := keyvalue.GetInt64(key)
	require.NoError(t, err)
	return attempts
}

func linkRequest(password string) *LinkIdentityRequest {
	return &LinkIdentityRequest{
		Provider: mockIDPProviderKey,
		Code:     "code",
		Password: password,
	}
}

func runLink(t *testing.T, u *user.User, req *LinkIdentityRequest) (*LinkedIdentity, error) {
	t.Helper()
	s := db.NewSession()
	defer s.Close()
	identity, err := LinkIdentity(t.Context(), s, u, req)
	if err == nil {
		require.NoError(t, s.Commit())
	}
	return identity, err
}

func runUnlink(t *testing.T, u *user.User, identityID int64, password, passcode string) error {
	t.Helper()
	s := db.NewSession()
	defer s.Close()
	err := UnlinkIdentity(t.Context(), s, u, identityID, password, passcode)
	if err == nil {
		require.NoError(t, s.Commit())
	}
	return err
}

func TestLinkIdentity(t *testing.T) {
	t.Run("links the identity", func(t *testing.T) {
		db.LoadAndAssertFixtures(t)
		idp := newMockIDP(t)
		idp.configure(t)
		idp.setClaims(map[string]any{"sub": "new-sub"})
		u := loadIdentityTestUser(t, 2)

		identity, err := runLink(t, u, linkRequest(identityTestPassword))
		require.NoError(t, err)

		assert.Equal(t, idp.URL, identity.Issuer)
		assert.Equal(t, mockIDPProviderKey, identity.ProviderKey)
		assert.Equal(t, "Provider One", identity.ProviderName)
		db.AssertExists(t, "user_identities", map[string]interface{}{
			"id":      identity.ID,
			"user_id": 2,
			"issuer":  idp.URL,
			"subject": "new-sub",
		}, false)
	})
	t.Run("wrong password is refused before the code is exchanged and counts toward the lockout", func(t *testing.T) {
		db.LoadAndAssertFixtures(t)
		idp := newMockIDP(t)
		idp.configure(t)
		idp.setClaims(map[string]any{"sub": "new-sub"})
		u := loadIdentityTestUser(t, 2)

		_, err := runLink(t, u, linkRequest("wrong"))
		require.Error(t, err)

		assert.True(t, user.IsErrWrongUsernameOrPassword(err), "unexpected error %v", err)
		assert.Equal(t, int32(0), idp.tokenRequests.Load(), "the single-use code must survive a typo")
		assert.Equal(t, int64(1), failedAttempts(t, u.GetFailedPasswordAttemptsKey()))
		db.AssertMissing(t, "user_identities", map[string]interface{}{
			"user_id": 2,
		})
	})
	t.Run("TOTP user", func(t *testing.T) {
		t.Run("missing passcode is refused", func(t *testing.T) {
			db.LoadAndAssertFixtures(t)
			idp := newMockIDP(t)
			idp.configure(t)
			u := loadIdentityTestUser(t, 3)
			enableTestTOTP(t, u.ID)

			_, err := runLink(t, u, linkRequest(identityTestPassword))
			require.Error(t, err)

			assert.True(t, user.IsErrInvalidTOTPPasscode(err), "unexpected error %v", err)
			assert.Equal(t, int32(0), idp.tokenRequests.Load())
		})
		t.Run("wrong passcode is refused and counts toward the lockout", func(t *testing.T) {
			db.LoadAndAssertFixtures(t)
			idp := newMockIDP(t)
			idp.configure(t)
			u := loadIdentityTestUser(t, 3)
			enableTestTOTP(t, u.ID)

			req := linkRequest(identityTestPassword)
			req.TOTPPasscode = "000000"
			_, err := runLink(t, u, req)
			require.Error(t, err)

			assert.True(t, user.IsErrInvalidTOTPPasscode(err), "unexpected error %v", err)
			assert.Equal(t, int32(0), idp.tokenRequests.Load())
			assert.Equal(t, int64(1), failedAttempts(t, u.GetFailedTOTPAttemptsKey()))
		})
		t.Run("valid passcode links", func(t *testing.T) {
			db.LoadAndAssertFixtures(t)
			idp := newMockIDP(t)
			idp.configure(t)
			idp.setClaims(map[string]any{"sub": "totp-sub"})
			u := loadIdentityTestUser(t, 3)
			secret := enableTestTOTP(t, u.ID)

			passcode, err := totp.GenerateCode(secret, time.Now())
			require.NoError(t, err)
			req := linkRequest(identityTestPassword)
			req.TOTPPasscode = passcode
			_, err = runLink(t, u, req)
			require.NoError(t, err)

			db.AssertExists(t, "user_identities", map[string]interface{}{
				"user_id": u.ID,
				"issuer":  idp.URL,
				"subject": "totp-sub",
			}, false)
		})
	})
	t.Run("refuses an identity linked to another user", func(t *testing.T) {
		db.LoadAndAssertFixtures(t)
		idp := newMockIDP(t)
		idp.configure(t)
		linkIdentity(t, 1, idp.URL, "taken-sub")
		idp.setClaims(map[string]any{"sub": "taken-sub"})
		u := loadIdentityTestUser(t, 2)

		_, err := runLink(t, u, linkRequest(identityTestPassword))
		require.Error(t, err)

		assert.True(t, models.IsErrUserIdentityLinkedToOtherUser(err), "unexpected error %v", err)
		db.AssertMissing(t, "user_identities", map[string]interface{}{
			"user_id": 2,
		})
	})
	t.Run("refuses the identity of an OIDC-created user", func(t *testing.T) {
		db.LoadAndAssertFixtures(t)
		idp := newMockIDP(t)
		idp.configure(t)
		s := db.NewSession()
		_, err := s.Insert(&user.User{
			Username: "oidcCreated",
			Email:    "oidc-created@example.com",
			Issuer:   idp.URL,
			Subject:  "oidc-created",
			Status:   user.StatusActive,
		})
		require.NoError(t, err)
		require.NoError(t, s.Commit())
		s.Close()
		idp.setClaims(map[string]any{"sub": "oidc-created"})
		u := loadIdentityTestUser(t, 2)

		_, err = runLink(t, u, linkRequest(identityTestPassword))
		require.Error(t, err)

		assert.True(t, models.IsErrUserIdentityLinkedToOtherUser(err), "unexpected error %v", err)
		db.AssertMissing(t, "user_identities", map[string]interface{}{
			"user_id": 2,
		})
	})
	t.Run("refuses a second identity for the same issuer", func(t *testing.T) {
		db.LoadAndAssertFixtures(t)
		idp := newMockIDP(t)
		idp.configure(t)
		linkIdentity(t, 2, idp.URL, "first-sub")
		idp.setClaims(map[string]any{"sub": "second-sub"})
		u := loadIdentityTestUser(t, 2)

		_, err := runLink(t, u, linkRequest(identityTestPassword))
		require.Error(t, err)

		assert.True(t, models.IsErrUserIdentityProviderAlreadyLinked(err), "unexpected error %v", err)
		db.AssertMissing(t, "user_identities", map[string]interface{}{
			"subject": "second-sub",
		})
	})
	t.Run("relinking the own identity returns it unchanged", func(t *testing.T) {
		db.LoadAndAssertFixtures(t)
		idp := newMockIDP(t)
		idp.configure(t)
		idp.setClaims(map[string]any{"sub": "own-sub"})
		u := loadIdentityTestUser(t, 2)

		first, err := runLink(t, u, linkRequest(identityTestPassword))
		require.NoError(t, err)

		second, err := runLink(t, u, linkRequest(identityTestPassword))
		require.NoError(t, err)

		assert.Equal(t, first.ID, second.ID)
		s := db.NewSession()
		defer s.Close()
		count, err := s.Where("user_id = ?", u.ID).Count(&models.UserIdentity{})
		require.NoError(t, err)
		assert.Equal(t, int64(1), count)
		assert.Equal(t, mockIDPProviderKey, second.ProviderKey)
	})
}

func TestUnlinkIdentity(t *testing.T) {
	t.Run("unlinks the own identity", func(t *testing.T) {
		db.LoadAndAssertFixtures(t)
		u := loadIdentityTestUser(t, 1)

		require.NoError(t, runUnlink(t, u, 1, identityTestPassword, ""))

		db.AssertMissing(t, "user_identities", map[string]interface{}{
			"id": 1,
		})
		db.AssertExists(t, "user_identities", map[string]interface{}{
			"id": 2,
		}, false)
	})
	t.Run("wrong password is refused and counts toward the lockout", func(t *testing.T) {
		db.LoadAndAssertFixtures(t)
		u := loadIdentityTestUser(t, 1)

		err := runUnlink(t, u, 1, "wrong", "")
		require.Error(t, err)

		assert.True(t, user.IsErrWrongUsernameOrPassword(err), "unexpected error %v", err)
		assert.Equal(t, int64(1), failedAttempts(t, u.GetFailedPasswordAttemptsKey()))
		db.AssertExists(t, "user_identities", map[string]interface{}{
			"id": 1,
		}, false)
	})
	t.Run("TOTP user", func(t *testing.T) {
		t.Run("missing passcode is refused", func(t *testing.T) {
			db.LoadAndAssertFixtures(t)
			u := loadIdentityTestUser(t, 6)
			enableTestTOTP(t, u.ID)

			err := runUnlink(t, u, 3, identityTestPassword, "")
			require.Error(t, err)

			assert.True(t, user.IsErrInvalidTOTPPasscode(err), "unexpected error %v", err)
			db.AssertExists(t, "user_identities", map[string]interface{}{
				"id": 3,
			}, false)
		})
		t.Run("wrong passcode is refused and counts toward the lockout", func(t *testing.T) {
			db.LoadAndAssertFixtures(t)
			u := loadIdentityTestUser(t, 6)
			enableTestTOTP(t, u.ID)

			err := runUnlink(t, u, 3, identityTestPassword, "000000")
			require.Error(t, err)

			assert.True(t, user.IsErrInvalidTOTPPasscode(err), "unexpected error %v", err)
			assert.Equal(t, int64(1), failedAttempts(t, u.GetFailedTOTPAttemptsKey()))
		})
		t.Run("valid passcode unlinks", func(t *testing.T) {
			db.LoadAndAssertFixtures(t)
			u := loadIdentityTestUser(t, 6)
			secret := enableTestTOTP(t, u.ID)

			passcode, err := totp.GenerateCode(secret, time.Now())
			require.NoError(t, err)
			require.NoError(t, runUnlink(t, u, 3, identityTestPassword, passcode))

			db.AssertMissing(t, "user_identities", map[string]interface{}{
				"id": 3,
			})
		})
	})
	t.Run("another user's identity does not exist", func(t *testing.T) {
		db.LoadAndAssertFixtures(t)
		u := loadIdentityTestUser(t, 6)

		err := runUnlink(t, u, 1, identityTestPassword, "")
		require.Error(t, err)

		assert.True(t, models.IsErrUserIdentityDoesNotExist(err), "unexpected error %v", err)
		db.AssertExists(t, "user_identities", map[string]interface{}{
			"id": 1,
		}, false)
	})
	t.Run("nonexistent identity", func(t *testing.T) {
		db.LoadAndAssertFixtures(t)
		u := loadIdentityTestUser(t, 1)

		err := runUnlink(t, u, 9999, identityTestPassword, "")
		require.Error(t, err)

		assert.True(t, models.IsErrUserIdentityDoesNotExist(err), "unexpected error %v", err)
	})
}

func TestListIdentities(t *testing.T) {
	t.Run("resolves the provider from the issuer", func(t *testing.T) {
		db.LoadAndAssertFixtures(t)
		idp := newMockIDP(t)
		idp.configure(t)
		linkIdentity(t, 2, idp.URL, "listed-sub")

		s := db.NewSession()
		defer s.Close()
		identities, err := ListIdentities(s, &user.User{ID: 2})
		require.NoError(t, err)

		require.Len(t, identities, 1)
		assert.Equal(t, idp.URL, identities[0].Issuer)
		assert.Equal(t, mockIDPProviderKey, identities[0].ProviderKey)
		assert.Equal(t, "Provider One", identities[0].ProviderName)
	})
	t.Run("leaves the provider empty when it is no longer configured", func(t *testing.T) {
		db.LoadAndAssertFixtures(t)
		idp := newMockIDP(t)
		idp.configure(t)

		s := db.NewSession()
		defer s.Close()
		identities, err := ListIdentities(s, &user.User{ID: 1})
		require.NoError(t, err)

		require.Len(t, identities, 2)
		assert.Equal(t, int64(1), identities[0].ID)
		assert.Equal(t, "https://some.service.com", identities[0].Issuer)
		assert.Empty(t, identities[0].ProviderKey)
		assert.Empty(t, identities[0].ProviderName)
		assert.Equal(t, "https://other.service.com", identities[1].Issuer)
	})
	t.Run("only lists the user's own identities", func(t *testing.T) {
		db.LoadAndAssertFixtures(t)

		s := db.NewSession()
		defer s.Close()
		identities, err := ListIdentities(s, &user.User{ID: 6})
		require.NoError(t, err)

		require.Len(t, identities, 1)
		assert.Equal(t, int64(3), identities[0].ID)
	})
}

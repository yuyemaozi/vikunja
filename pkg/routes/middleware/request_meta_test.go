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

package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"code.vikunja.io/api/pkg/events"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	echomiddleware "github.com/labstack/echo/v5/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequestMetaNormalizesUUID(t *testing.T) {
	const valid = "018f8f4c-2e18-7b61-9a50-9c4f5de4e10b"
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "missing", input: "", want: ""},
		{name: "invalid", input: "not-a-uuid", want: ""},
		{name: "valid uppercase", input: "018F8F4C-2E18-7B61-9A50-9C4F5DE4E10B", want: valid},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			e := echo.New()
			bound := ""
			e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
				return func(c *echo.Context) error {
					if value := c.Request().Header.Get(echo.HeaderXRequestID); value != "" {
						c.Response().Header().Set(echo.HeaderXRequestID, value)
					}
					return next(c)
				}
			})
			e.Use(RequestMeta())
			e.GET("/", func(c *echo.Context) error {
				meta := events.RequestMetaFromContext(c.Request().Context())
				require.NotNil(t, meta)
				bound = meta.RequestID
				return c.NoContent(http.StatusNoContent)
			})

			request := httptest.NewRequest(http.MethodGet, "/", nil)
			if testCase.input != "" {
				request.Header.Set(echo.HeaderXRequestID, testCase.input)
			}
			recorder := httptest.NewRecorder()
			e.ServeHTTP(recorder, request)

			header := recorder.Header().Get(echo.HeaderXRequestID)
			_, err := uuid.Parse(header)
			require.NoError(t, err)
			assert.Equal(t, header, bound)
			if testCase.want != "" {
				assert.Equal(t, testCase.want, header)
			}
		})
	}
}

func TestRequestMetaKeepsEchoRequestID(t *testing.T) {
	const supplied = "018f8f4c-2e18-7b61-9a50-9c4f5de4e10b"
	e := echo.New()
	e.Use(echomiddleware.RequestID())
	e.Use(RequestMeta())
	e.GET("/", func(c *echo.Context) error {
		meta := events.RequestMetaFromContext(c.Request().Context())
		require.NotNil(t, meta)
		return c.String(http.StatusOK, meta.RequestID)
	})

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set(echo.HeaderXRequestID, supplied)
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)

	assert.Equal(t, supplied, recorder.Body.String())
	assert.Equal(t, supplied, recorder.Header().Get(echo.HeaderXRequestID))
}

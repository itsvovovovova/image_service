package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestAuthentication(t *testing.T) {
	s := service{secret: []byte("test-secret-long-enough-for-hs256")}
	next := s.auth(func(w http.ResponseWriter, _ *http.Request, user string) {
		if user != "alice" {
			t.Errorf("unexpected subject: %s", user)
		}
		w.WriteHeader(204)
	})
	for _, tc := range []struct {
		name    string
		method  jwt.SigningMethod
		expires time.Time
		issuer  string
		want    int
	}{
		{"valid", jwt.SigningMethodHS256, time.Now().Add(time.Hour), "image-service", 204},
		{"expired", jwt.SigningMethodHS256, time.Now().Add(-time.Hour), "image-service", 401},
		{"wrong issuer", jwt.SigningMethodHS256, time.Now().Add(time.Hour), "other", 401},
		{"wrong algorithm", jwt.SigningMethodHS384, time.Now().Add(time.Hour), "image-service", 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := jwt.RegisteredClaims{Subject: "alice", Issuer: tc.issuer, ExpiresAt: jwt.NewNumericDate(tc.expires)}
			raw, err := jwt.NewWithClaims(tc.method, claims).SignedString(s.secret)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest("GET", "/status/example", nil)
			request.Header.Set("Authorization", "Bearer "+raw)
			response := httptest.NewRecorder()
			next(response, request)
			if response.Code != tc.want {
				t.Errorf("got %d, want %d", response.Code, tc.want)
			}
		})
	}
}

func TestUploadRejectsInvalidInputBeforeStorage(t *testing.T) {
	s := service{}
	for _, tc := range []struct {
		path, body string
		want       int
	}{
		{"/task?filter=unknown", "invalid", 400},
		{"/task?filter=Negative", "invalid", 415},
		{"/task?filter=Negative", strings.Repeat("x", maxImageBytes+1), 413},
	} {
		request := httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body))
		response := httptest.NewRecorder()
		s.upload(response, request, "alice")
		if response.Code != tc.want {
			t.Errorf("%s: got %d, want %d", tc.path, response.Code, tc.want)
		}
	}
}

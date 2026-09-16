package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStartDeviceAuthorization_PostsDeviceNameWithoutAuthorization(t *testing.T) {
	var gotAuth, gotName, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		var body struct {
			DeviceName string `json:"device_name"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		gotName = body.DeviceName
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"device_code":"dc","user_code":"WDJB-MJHT","verification_uri":"https://x/cli/auth","verification_uri_complete":"https://x/cli/auth?code=WDJB-MJHT","expires_in":600,"interval":5}`))
	}))
	defer srv.Close()

	got, err := New(srv.URL, "").StartDeviceAuthorization(context.Background(), "alex-macbook")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if gotPath != "/api/cli/device" || gotName != "alex-macbook" {
		t.Fatalf("path=%q name=%q", gotPath, gotName)
	}
	if gotAuth != "" {
		t.Fatalf("an unauthenticated client must not send Authorization, got %q", gotAuth)
	}
	want := DeviceAuthorization{DeviceCode: "dc", UserCode: "WDJB-MJHT", VerificationURI: "https://x/cli/auth",
		VerificationURIComplete: "https://x/cli/auth?code=WDJB-MJHT", ExpiresIn: 600, Interval: 5}
	if got != want {
		t.Fatalf("got %+v", got)
	}
}

func TestStartDeviceAuthorization_Errors(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
		check  func(error) bool
	}{
		"throttled":       {429, `{"error":"too many login attempts — wait a few minutes and try again"}`, func(err error) bool { var e *APIError; return errors.As(err, &e) && e.StatusCode == 429 }},
		"missing codes":   {201, `{"device_code":"","user_code":""}`, func(err error) bool { return errors.Is(err, ErrMalformedDeviceResponse) }},
		"not json":        {201, `<html>`, func(err error) bool { return errors.Is(err, ErrMalformedDeviceResponse) }},
		"server exploded": {500, `oops`, func(err error) bool { var e *APIError; return errors.As(err, &e) && e.StatusCode == 500 }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			_, err := New(srv.URL, "").StartDeviceAuthorization(context.Background(), "x")
			if err == nil || !tc.check(err) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestPollDeviceToken_Statuses(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   DeviceToken
	}{
		{200, `{"status":"authorization_pending"}`, DeviceToken{Status: DeviceStatusPending}},
		{200, `{"status":"authorized","token":"pgrun_abc","user":{"email":"a@b.c"},"account":{"id":"d-1","slug":"d-1","name":"Default"}}`,
			DeviceToken{Status: DeviceStatusAuthorized, Token: "pgrun_abc", User: DeviceUser{Email: "a@b.c"}, Account: DeviceAccount{ID: "d-1", Slug: "d-1", Name: "Default"}}},
		{403, `{"status":"access_denied"}`, DeviceToken{Status: DeviceStatusDenied}},
		{404, `{"status":"invalid_device_code"}`, DeviceToken{Status: DeviceStatusInvalid}},
		{410, `{"status":"expired"}`, DeviceToken{Status: DeviceStatusExpired}},
		{410, `{"status":"consumed"}`, DeviceToken{Status: DeviceStatusConsumed}},
		{429, `{"status":"slow_down"}`, DeviceToken{Status: DeviceStatusSlowDown}},
	}
	for _, tc := range cases {
		t.Run(tc.body, func(t *testing.T) {
			var gotCode, gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				var body struct {
					DeviceCode string `json:"device_code"`
				}
				json.NewDecoder(r.Body).Decode(&body)
				gotCode = body.DeviceCode
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			got, err := New(srv.URL, "").PollDeviceToken(context.Background(), "dc-123")
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if gotPath != "/api/cli/device/token" || gotCode != "dc-123" {
				t.Fatalf("path=%q code=%q", gotPath, gotCode)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestPollDeviceToken_MalformedAndServerErrors(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
		check  func(error) bool
	}{
		"unknown status":      {200, `{"status":"party"}`, func(err error) bool { return errors.Is(err, ErrMalformedDeviceResponse) }},
		"authorized no token": {200, `{"status":"authorized"}`, func(err error) bool { return errors.Is(err, ErrMalformedDeviceResponse) }},
		"not json":            {200, `nope`, func(err error) bool { return errors.Is(err, ErrMalformedDeviceResponse) }},
		"5xx is an APIError":  {502, `bad gateway`, func(err error) bool { var e *APIError; return errors.As(err, &e) && e.StatusCode == 502 }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			_, err := New(srv.URL, "").PollDeviceToken(context.Background(), "dc")
			if err == nil || !tc.check(err) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestAuthenticatedClientStillSendsBearer(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"projects":[]}`))
	}))
	defer srv.Close()
	if _, _, err := New(srv.URL, "tok").ListProjects(context.Background()); err != nil {
		t.Fatalf("err = %v", err)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
}

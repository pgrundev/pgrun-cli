package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// Device login (`pgrun auth login`, docs/specs/cli-browser-auth.md in the
// Rails repo): the CLI starts a request, the person approves it in a
// browser, the CLI polls until the server hands over a freshly minted token
// exactly once. Both calls run on a Client with no token.

// DeviceAuthorization is the start response. DeviceCode is the secret the
// CLI keeps; UserCode is what the person sees.
type DeviceAuthorization struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type DeviceUser struct {
	Email string `json:"email"`
}

type DeviceAccount struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// DeviceToken is one poll answer. Token is set only when Status is
// DeviceStatusAuthorized — and only on that one response.
type DeviceToken struct {
	Status  string        `json:"status"`
	Token   string        `json:"token"`
	User    DeviceUser    `json:"user"`
	Account DeviceAccount `json:"account"`
}

const (
	DeviceStatusPending    = "authorization_pending"
	DeviceStatusAuthorized = "authorized"
	DeviceStatusDenied     = "access_denied"
	DeviceStatusExpired    = "expired"
	DeviceStatusConsumed   = "consumed"
	DeviceStatusInvalid    = "invalid_device_code"
	DeviceStatusSlowDown   = "slow_down"
)

// ErrMalformedDeviceResponse: the server answered, but not in the contract's shape.
var ErrMalformedDeviceResponse = errors.New("malformed response from the login API")

// StartDeviceAuthorization creates a pending login request named after deviceName.
func (c *Client) StartDeviceAuthorization(ctx context.Context, deviceName string) (DeviceAuthorization, error) {
	raw, err := c.do(ctx, http.MethodPost, "/api/cli/device", map[string]string{"device_name": deviceName}, http.StatusCreated)
	if err != nil {
		return DeviceAuthorization{}, err
	}
	var auth DeviceAuthorization
	if json.Unmarshal(raw, &auth) != nil || auth.DeviceCode == "" || auth.UserCode == "" || auth.VerificationURI == "" {
		return DeviceAuthorization{}, ErrMalformedDeviceResponse
	}
	return auth, nil
}

// pollStatuses are the HTTP statuses whose body carries a device status.
var pollStatuses = map[int]bool{
	http.StatusOK: true, http.StatusForbidden: true, http.StatusNotFound: true,
	http.StatusGone: true, http.StatusTooManyRequests: true,
}

var knownDeviceStatuses = map[string]bool{
	DeviceStatusPending: true, DeviceStatusAuthorized: true, DeviceStatusDenied: true, DeviceStatusExpired: true,
	DeviceStatusConsumed: true, DeviceStatusInvalid: true, DeviceStatusSlowDown: true,
}

// PollDeviceToken asks once whether the request has been approved.
func (c *Client) PollDeviceToken(ctx context.Context, deviceCode string) (DeviceToken, error) {
	status, raw, err := c.send(ctx, http.MethodPost, "/api/cli/device/token", map[string]string{"device_code": deviceCode})
	if err != nil {
		return DeviceToken{}, err
	}
	if !pollStatuses[status] {
		return DeviceToken{}, newAPIError(status, raw)
	}
	var tok DeviceToken
	if json.Unmarshal(raw, &tok) != nil || !knownDeviceStatuses[tok.Status] {
		return DeviceToken{}, ErrMalformedDeviceResponse
	}
	if tok.Status == DeviceStatusAuthorized && tok.Token == "" {
		return DeviceToken{}, ErrMalformedDeviceResponse
	}
	return tok, nil
}

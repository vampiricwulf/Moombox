package errors

import (
	"errors"
	"fmt"
	"testing"
)

func TestMoomboxError_Error(t *testing.T) {
	tests := []struct {
		name     string
		err      *MoomboxError
		expected string
	}{
		{
			name:     "without cause",
			err:      &MoomboxError{Code: "TEST_CODE", Message: "something failed"},
			expected: "TEST_CODE: something failed",
		},
		{
			name:     "with cause",
			err:      &MoomboxError{Code: "TEST_CODE", Message: "something failed", Cause: fmt.Errorf("underlying")},
			expected: "TEST_CODE: something failed: underlying",
		},
		{
			name:     "empty code and message",
			err:      &MoomboxError{},
			expected: ": ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.err.Error()
			if got != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, got)
			}
		})
	}
}

func TestMoomboxError_Unwrap(t *testing.T) {
	tests := []struct {
		name      string
		err       *MoomboxError
		wantCause error
	}{
		{
			name:      "nil cause",
			err:       &MoomboxError{Code: "X", Message: "x"},
			wantCause: nil,
		},
		{
			name:      "with cause",
			err:       &MoomboxError{Code: "X", Message: "x", Cause: fmt.Errorf("root")},
			wantCause: fmt.Errorf("root"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.err.Unwrap()
			if tt.wantCause == nil && got != nil {
				t.Errorf("expected nil cause, got %v", got)
			}
			if tt.wantCause != nil && got == nil {
				t.Errorf("expected cause %v, got nil", tt.wantCause)
			}
			if tt.wantCause != nil && got != nil && got.Error() != tt.wantCause.Error() {
				t.Errorf("expected cause %q, got %q", tt.wantCause.Error(), got.Error())
			}
		})
	}
}

func TestMoomboxError_Is(t *testing.T) {
	tests := []struct {
		name   string
		err    *MoomboxError
		target error
		want   bool
	}{
		{
			name:   "same code matches",
			err:    &MoomboxError{Code: "SAME"},
			target: &MoomboxError{Code: "SAME"},
			want:   true,
		},
		{
			name:   "different code does not match",
			err:    &MoomboxError{Code: "A"},
			target: &MoomboxError{Code: "B"},
			want:   false,
		},
		{
			name:   "non-MoomboxError target",
			err:    &MoomboxError{Code: "X"},
			target: fmt.Errorf("random error"),
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.err.Is(tt.target)
			if got != tt.want {
				t.Errorf("expected %v, got %v", tt.want, got)
			}
		})
	}
}

func TestNewYouTubeError(t *testing.T) {
	tests := []struct {
		name       string
		code       string
		message    string
		cause      error
		wantCode   string
		wantMsg    string
		wantCause  bool
	}{
		{
			name:     "without cause",
			code:     ErrLoginRequired,
			message:  "login needed",
			cause:    nil,
			wantCode: ErrLoginRequired,
			wantMsg:  "login needed",
		},
		{
			name:      "with cause",
			code:      ErrMembersOnly,
			message:   "members only stream",
			cause:     fmt.Errorf("access denied"),
			wantCode:  ErrMembersOnly,
			wantMsg:   "members only stream",
			wantCause: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := NewYouTubeError(tt.code, tt.message, tt.cause)
			if err.Code != tt.wantCode {
				t.Errorf("expected code %q, got %q", tt.wantCode, err.Code)
			}
			if err.Message != tt.wantMsg {
				t.Errorf("expected message %q, got %q", tt.wantMsg, err.Message)
			}
			if tt.wantCause && err.Cause == nil {
				t.Errorf("expected cause to be set")
			}
			if !tt.wantCause && err.Cause != nil {
				t.Errorf("expected nil cause, got %v", err.Cause)
			}
		})
	}
}

func TestNewDownloadError(t *testing.T) {
	tests := []struct {
		name       string
		code       string
		message    string
		httpStatus int
		cause      error
	}{
		{
			name:       "segment failed with 404",
			code:       ErrSegmentFailed,
			message:    "segment download failed",
			httpStatus: 404,
			cause:      nil,
		},
		{
			name:       "manifest failed with 503",
			code:       ErrManifestFailed,
			message:    "manifest unavailable",
			httpStatus: 503,
			cause:      fmt.Errorf("server error"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := NewDownloadError(tt.code, tt.message, tt.httpStatus, tt.cause)
			if err.Code != tt.code {
				t.Errorf("expected code %q, got %q", tt.code, err.Code)
			}
			if err.HTTPStatus != tt.httpStatus {
				t.Errorf("expected HTTP status %d, got %d", tt.httpStatus, err.HTTPStatus)
			}
			if err.Message != tt.message {
				t.Errorf("expected message %q, got %q", tt.message, err.Message)
			}
		})
	}
}

func TestNewNetworkError(t *testing.T) {
	err := NewNetworkError("connection refused", 0, "http://example.com", fmt.Errorf("dial tcp"))
	if err.Code != "NETWORK_ERROR" {
		t.Errorf("expected code NETWORK_ERROR, got %q", err.Code)
	}
	if err.URL != "http://example.com" {
		t.Errorf("expected URL http://example.com, got %q", err.URL)
	}
	if err.HTTPStatus != 0 {
		t.Errorf("expected HTTP status 0, got %d", err.HTTPStatus)
	}
	if err.Cause == nil {
		t.Errorf("expected cause to be set")
	}
}

func TestNewConfigError(t *testing.T) {
	err := NewConfigError("bad config value", nil)
	if err.Code != "CONFIG_ERROR" {
		t.Errorf("expected code CONFIG_ERROR, got %q", err.Code)
	}
	if !err.Expected {
		t.Errorf("expected ConfigError to be marked as Expected")
	}
	if err.Message != "bad config value" {
		t.Errorf("expected message %q, got %q", "bad config value", err.Message)
	}
}

func TestNewMuxingError(t *testing.T) {
	tests := []struct {
		name     string
		message  string
		exitCode int
		cause    error
	}{
		{
			name:     "ffmpeg exit code 1",
			message:  "ffmpeg failed",
			exitCode: 1,
			cause:    nil,
		},
		{
			name:     "ffmpeg not found",
			message:  "ffmpeg not found on PATH",
			exitCode: 127,
			cause:    fmt.Errorf("exec: not found"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := NewMuxingError(tt.message, tt.exitCode, tt.cause)
			if err.Code != "MUXING_ERROR" {
				t.Errorf("expected code MUXING_ERROR, got %q", err.Code)
			}
			if err.ExitCode != tt.exitCode {
				t.Errorf("expected exit code %d, got %d", tt.exitCode, err.ExitCode)
			}
		})
	}
}

func TestNewVideoPlayabilityError(t *testing.T) {
	err := NewVideoPlayabilityError("video is private", "PRIVATE", "This video is private.")
	if err.Code != "PLAYABILITY_PRIVATE" {
		t.Errorf("expected code PLAYABILITY_PRIVATE, got %q", err.Code)
	}
	if !err.Expected {
		t.Errorf("expected VideoPlayabilityError to be marked as Expected")
	}
	if err.PlayabilityStatus != "PRIVATE" {
		t.Errorf("expected playability status PRIVATE, got %q", err.PlayabilityStatus)
	}
	if err.Reason != "This video is private." {
		t.Errorf("expected reason %q, got %q", "This video is private.", err.Reason)
	}
	if err.Context["playabilityStatus"] != "PRIVATE" {
		t.Errorf("expected context playabilityStatus=PRIVATE, got %v", err.Context["playabilityStatus"])
	}
	if err.Context["reason"] != "This video is private." {
		t.Errorf("expected context reason=%q, got %v", "This video is private.", err.Context["reason"])
	}
}

func TestNewAuthError(t *testing.T) {
	tests := []struct {
		name     string
		code     string
		message  string
		cause    error
	}{
		{
			name:    "cookies expired",
			code:    ErrCookiesExpired,
			message: "cookies have expired",
			cause:   nil,
		},
		{
			name:    "token expired with cause",
			code:    ErrTokenExpired,
			message: "auth token expired",
			cause:   fmt.Errorf("401 unauthorized"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := NewAuthError(tt.code, tt.message, tt.cause)
			if err.Code != tt.code {
				t.Errorf("expected code %q, got %q", tt.code, err.Code)
			}
			if !err.Expected {
				t.Errorf("expected AuthError to be marked as Expected")
			}
		})
	}
}

func TestIsExpected(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "MoomboxError expected=true",
			err:  &MoomboxError{Code: "X", Expected: true},
			want: true,
		},
		{
			name: "MoomboxError expected=false",
			err:  &MoomboxError{Code: "X", Expected: false},
			want: false,
		},
		{
			name: "YouTubeError expected=false by default",
			err:  NewYouTubeError("X", "msg", nil),
			want: false,
		},
		{
			name: "ConfigError expected=true",
			err:  NewConfigError("bad", nil),
			want: true,
		},
		{
			name: "AuthError expected=true",
			err:  NewAuthError(ErrCookiesExpired, "expired", nil),
			want: true,
		},
		{
			name: "VideoPlayabilityError expected=true",
			err:  NewVideoPlayabilityError("private", "PRIVATE", "reason"),
			want: true,
		},
		{
			name: "DownloadError expected=false by default",
			err:  NewDownloadError(ErrSegmentFailed, "fail", 500, nil),
			want: false,
		},
		{
			name: "NetworkError expected=false by default",
			err:  NewNetworkError("fail", 500, "http://x", nil),
			want: false,
		},
		{
			name: "MuxingError expected=false by default",
			err:  NewMuxingError("fail", 1, nil),
			want: false,
		},
		{
			name: "plain error returns false",
			err:  fmt.Errorf("just an error"),
			want: false,
		},
		{
			name: "nil error interface returns false",
			err:  fmt.Errorf("not a moombox error at all"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsExpected(tt.err)
			if got != tt.want {
				t.Errorf("expected %v, got %v", tt.want, got)
			}
		})
	}
}

func TestIsLoginRequired(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "YouTubeError with LOGIN_REQUIRED",
			err:  NewYouTubeError(ErrLoginRequired, "need login", nil),
			want: true,
		},
		{
			name: "YouTubeError with different code",
			err:  NewYouTubeError(ErrMembersOnly, "members", nil),
			want: false,
		},
		{
			name: "AuthError with LOGIN_REQUIRED",
			err:  NewAuthError(ErrLoginRequired, "login required", nil),
			want: true,
		},
		{
			name: "AuthError with COOKIES_EXPIRED",
			err:  NewAuthError(ErrCookiesExpired, "expired", nil),
			want: true,
		},
		{
			name: "AuthError with TOKEN_EXPIRED",
			err:  NewAuthError(ErrTokenExpired, "token expired", nil),
			want: false,
		},
		{
			name: "MoomboxError with LOGIN_REQUIRED (not YouTubeError/AuthError)",
			err:  &MoomboxError{Code: ErrLoginRequired},
			want: false,
		},
		{
			name: "plain error",
			err:  fmt.Errorf("login_required"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsLoginRequired(tt.err)
			if got != tt.want {
				t.Errorf("expected %v, got %v", tt.want, got)
			}
		})
	}
}

func TestErrorsIs_Chain(t *testing.T) {
	// Test that errors.Is works through the MoomboxError.Is method
	sentinel := &MoomboxError{Code: "SENTINEL"}
	wrapped := &MoomboxError{Code: "SENTINEL", Message: "wrapped instance"}

	if !errors.Is(wrapped, sentinel) {
		t.Errorf("expected errors.Is to match by code")
	}

	different := &MoomboxError{Code: "OTHER"}
	if errors.Is(different, sentinel) {
		t.Errorf("expected errors.Is to NOT match different codes")
	}
}

func TestErrorsAs_Chain(t *testing.T) {
	// Test that errors.As can extract YouTubeError from error interface
	ytErr := NewYouTubeError(ErrLoginRequired, "login needed", fmt.Errorf("no cookies"))

	var target *YouTubeError
	if !errors.As(ytErr, &target) {
		t.Errorf("expected errors.As to find YouTubeError")
	}
	if target.Code != ErrLoginRequired {
		t.Errorf("expected code %q, got %q", ErrLoginRequired, target.Code)
	}

	// Test that errors.As can extract DownloadError
	dlErr := NewDownloadError(ErrSegmentFailed, "seg fail", 404, nil)
	var dlTarget *DownloadError
	if !errors.As(dlErr, &dlTarget) {
		t.Errorf("expected errors.As to find DownloadError")
	}
	if dlTarget.HTTPStatus != 404 {
		t.Errorf("expected HTTPStatus 404, got %d", dlTarget.HTTPStatus)
	}

	// Test that errors.As does NOT find a VideoPlayabilityError when it is a YouTubeError
	var vpTarget *VideoPlayabilityError
	if errors.As(ytErr, &vpTarget) {
		t.Errorf("expected errors.As to NOT find VideoPlayabilityError in YouTubeError")
	}
}

func TestUnwrapChain(t *testing.T) {
	rootCause := fmt.Errorf("disk I/O error")
	dlErr := NewDownloadError(ErrSegmentFailed, "segment failed", 500, rootCause)

	unwrapped := dlErr.Unwrap()
	if unwrapped == nil {
		t.Errorf("expected non-nil unwrapped error")
	}
	if unwrapped.Error() != "disk I/O error" {
		t.Errorf("expected root cause 'disk I/O error', got %q", unwrapped.Error())
	}

	// errors.Is should traverse the unwrap chain for the cause
	if !errors.Is(dlErr, rootCause) {
		t.Errorf("expected errors.Is to find root cause via unwrap chain")
	}
}

func TestVideoPlayabilityError_ErrorString(t *testing.T) {
	err := NewVideoPlayabilityError("video unavailable", "UNPLAYABLE", "content warning")
	expected := "PLAYABILITY_UNPLAYABLE: video unavailable"
	got := err.Error()
	if got != expected {
		t.Errorf("expected %q, got %q", expected, got)
	}
}

func TestNetworkError_ErrorString(t *testing.T) {
	err := NewNetworkError("timeout", 408, "http://api.example.com/data", fmt.Errorf("context deadline exceeded"))
	expected := "NETWORK_ERROR: timeout: context deadline exceeded"
	got := err.Error()
	if got != expected {
		t.Errorf("expected %q, got %q", expected, got)
	}
}

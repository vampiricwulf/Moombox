// Package errors provides typed error types for Moombox.
package errors

import "fmt"

// MoomboxError is the base error type for all Moombox errors.
type MoomboxError struct {
	Code     string
	Message  string
	Expected bool // User-facing (true) vs developer error (false)
	Context  map[string]interface{}
	Cause    error
}

func (e *MoomboxError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *MoomboxError) Unwrap() error { return e.Cause }

// Is checks whether the target matches this error's code.
func (e *MoomboxError) Is(target error) bool {
	if t, ok := target.(*MoomboxError); ok {
		return e.Code == t.Code
	}
	return false
}

// YouTubeError represents a YouTube API error.
type YouTubeError struct {
	MoomboxError
}

// NewYouTubeError creates a new YouTube error.
func NewYouTubeError(code, message string, cause error) *YouTubeError {
	return &YouTubeError{MoomboxError{
		Code:    code,
		Message: message,
		Cause:   cause,
	}}
}

// DownloadError represents a download failure.
type DownloadError struct {
	MoomboxError
	HTTPStatus int
}

// NewDownloadError creates a new download error.
func NewDownloadError(code, message string, httpStatus int, cause error) *DownloadError {
	return &DownloadError{
		MoomboxError: MoomboxError{
			Code:    code,
			Message: message,
			Cause:   cause,
		},
		HTTPStatus: httpStatus,
	}
}

// NetworkError represents a network-level failure.
type NetworkError struct {
	MoomboxError
	HTTPStatus int
	URL        string
}

// NewNetworkError creates a new network error.
func NewNetworkError(message string, httpStatus int, url string, cause error) *NetworkError {
	return &NetworkError{
		MoomboxError: MoomboxError{
			Code:    "NETWORK_ERROR",
			Message: message,
			Cause:   cause,
		},
		HTTPStatus: httpStatus,
		URL:        url,
	}
}

// ConfigError represents a configuration error.
type ConfigError struct {
	MoomboxError
}

// NewConfigError creates a new config error.
func NewConfigError(message string, cause error) *ConfigError {
	return &ConfigError{MoomboxError{
		Code:     "CONFIG_ERROR",
		Message:  message,
		Expected: true,
		Cause:    cause,
	}}
}

// MuxingError represents an FFmpeg muxing error.
type MuxingError struct {
	MoomboxError
	ExitCode int
}

// NewMuxingError creates a new muxing error.
func NewMuxingError(message string, exitCode int, cause error) *MuxingError {
	return &MuxingError{
		MoomboxError: MoomboxError{
			Code:    "MUXING_ERROR",
			Message: message,
			Cause:   cause,
		},
		ExitCode: exitCode,
	}
}

// VideoPlayabilityError represents video playability errors (member-only, unavailable, etc.).
// Marked as expected (user-facing) errors.
type VideoPlayabilityError struct {
	MoomboxError
	PlayabilityStatus string
	Reason            string
}

// NewVideoPlayabilityError creates a new video playability error.
func NewVideoPlayabilityError(message, playabilityStatus, reason string) *VideoPlayabilityError {
	return &VideoPlayabilityError{
		MoomboxError: MoomboxError{
			Code:     "PLAYABILITY_" + playabilityStatus,
			Message:  message,
			Expected: true,
			Context:  map[string]interface{}{"playabilityStatus": playabilityStatus, "reason": reason},
		},
		PlayabilityStatus: playabilityStatus,
		Reason:            reason,
	}
}

// AuthError represents an authentication error.
type AuthError struct {
	MoomboxError
}

// NewAuthError creates a new auth error.
func NewAuthError(code, message string, cause error) *AuthError {
	return &AuthError{MoomboxError{
		Code:     code,
		Message:  message,
		Expected: true,
		Cause:    cause,
	}}
}

// Common error codes.
const (
	// YouTube errors
	ErrLoginRequired   = "LOGIN_REQUIRED"
	ErrUnplayable      = "UNPLAYABLE"
	ErrLiveNotStarted  = "LIVE_NOT_STARTED"
	ErrNotAStream      = "NOT_A_STREAM"
	ErrMembersOnly     = "MEMBERS_ONLY"
	ErrAgeRestricted   = "AGE_RESTRICTED"
	ErrPrivate         = "PRIVATE"
	ErrCopyright       = "COPYRIGHT"
	ErrGeoRestricted   = "GEO_RESTRICTED"
	ErrStreamEnded     = "STREAM_ENDED"

	// Download errors
	ErrSegmentFailed   = "SEGMENT_FAILED"
	ErrManifestFailed  = "MANIFEST_FAILED"
	ErrFormatNotFound  = "FORMAT_NOT_FOUND"
	ErrResumeCorrupted = "RESUME_CORRUPTED"
	ErrDiskFull        = "DISK_FULL"
	ErrTimeout         = "TIMEOUT"

	// Network errors
	ErrHTTPFailed     = "HTTP_FAILED"
	ErrDNSFailed      = "DNS_FAILED"
	ErrConnectionReset = "CONNECTION_RESET"
	ErrTLSFailed      = "TLS_FAILED"

	// Muxing errors
	ErrFFmpegNotFound = "FFMPEG_NOT_FOUND"
	ErrFFmpegFailed   = "FFMPEG_FAILED"
	ErrInvalidInput   = "INVALID_INPUT"

	// Auth errors
	ErrCookiesExpired  = "COOKIES_EXPIRED"
	ErrCookiesInvalid  = "COOKIES_INVALID"
	ErrTokenExpired    = "TOKEN_EXPIRED"
)

// IsExpected returns true if the error is user-facing and expected.
// Traverses the error chain so it works with wrapped errors (fmt.Errorf %w).
// All Moombox error types satisfy this via the embedded MoomboxError.
func IsExpected(err error) bool {
	for err != nil {
		if e, ok := err.(interface{ isExpected() bool }); ok {
			return e.isExpected()
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			break
		}
		err = u.Unwrap()
	}
	return false
}

func (e *MoomboxError) isExpected() bool { return e.Expected }

// IsLoginRequired returns true if the error indicates login is required.
// Traverses the error chain so it works with wrapped errors (fmt.Errorf %w).
func IsLoginRequired(err error) bool {
	for err != nil {
		if e, ok := err.(*YouTubeError); ok {
			return e.Code == ErrLoginRequired
		}
		if e, ok := err.(*AuthError); ok {
			return e.Code == ErrLoginRequired || e.Code == ErrCookiesExpired
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			break
		}
		err = u.Unwrap()
	}
	return false
}

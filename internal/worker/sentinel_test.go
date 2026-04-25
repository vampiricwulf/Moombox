package worker

import (
	"errors"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// TestCheckPlayabilitySentinels locks down the producer side of the
// sentinel migration: each PlayabilityError category maps to exactly
// one of (ErrCookiesRequired, ErrNonActionable, nil), and the display
// string still contains the user-visible reason. Replaces the old
// string-matching helpers with errors.Is contract tests.
func TestCheckPlayabilitySentinels(t *testing.T) {
	sp := &StreamProcessor{}

	tests := []struct {
		name         string
		playability  youtube.PlayabilityError
		reason       string
		wantSentinel error
		wantMsgFrag  string
	}{
		{
			name:         "MembersOnly → ErrCookiesRequired",
			playability:  youtube.PlayabilityMembersOnly,
			reason:       "Members only — join the channel",
			wantSentinel: ErrCookiesRequired,
			wantMsgFrag:  "Member-only",
		},
		{
			name:         "LoginRequired → ErrCookiesRequired",
			playability:  youtube.PlayabilityLoginRequired,
			reason:       "Sign in to verify your age",
			wantSentinel: ErrCookiesRequired,
			wantMsgFrag:  "Login required",
		},
		{
			name:         "AgeRestricted → ErrNonActionable",
			playability:  youtube.PlayabilityAgeRestricted,
			reason:       "This content is age-restricted",
			wantSentinel: ErrNonActionable,
			wantMsgFrag:  "Age restricted",
		},
		{
			name:         "Unknown playability error → no sentinel",
			playability:  "SomeNewYouTubeCategory",
			reason:       "Unsupported in this region",
			wantSentinel: nil,
			wantMsgFrag:  "Unsupported",
		},
		{
			name:         "Empty reason → falls back to Unknown error",
			playability:  youtube.PlayabilityLoginRequired,
			reason:       "",
			wantSentinel: ErrCookiesRequired,
			wantMsgFrag:  "Unknown error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := &youtube.VideoInfo{
				PlayabilityError:  tt.playability,
				PlayabilityReason: tt.reason,
			}
			msg, sentinel := sp.checkPlayability(info)

			if sentinel != tt.wantSentinel {
				t.Errorf("sentinel: want %v, got %v", tt.wantSentinel, sentinel)
			}
			if !strings.Contains(msg, tt.wantMsgFrag) {
				t.Errorf("display msg %q should contain %q", msg, tt.wantMsgFrag)
			}
		})
	}
}

func TestCheckPlayabilityNoErrorWhenPlayable(t *testing.T) {
	sp := &StreamProcessor{}

	tests := []*youtube.VideoInfo{
		{PlayabilityError: ""},                      // not set
		{PlayabilityError: youtube.PlayabilityOK},   // explicit OK
	}
	for _, info := range tests {
		msg, sentinel := sp.checkPlayability(info)
		if msg != "" {
			t.Errorf("playable info should return empty msg, got %q", msg)
		}
		if sentinel != nil {
			t.Errorf("playable info should return nil sentinel, got %v", sentinel)
		}
	}
}

// TestStreamProcessResultAsError covers the result→error conversion
// that worker.go's main loop uses to feed setJobError. The sentinel
// must round-trip via errors.Is so the downstream "transition to
// StatusCookies" / "suppress notification" branches fire.
func TestStreamProcessResultAsError(t *testing.T) {
	t.Run("nil when no error", func(t *testing.T) {
		r := &StreamProcessResult{}
		if got := r.AsError(); got != nil {
			t.Errorf("empty Error: want nil, got %v", got)
		}
	})

	t.Run("plain error (no sentinel)", func(t *testing.T) {
		r := &StreamProcessResult{Error: "twitch channel is offline"}
		err := r.AsError()
		if err == nil {
			t.Fatal("AsError: want non-nil")
		}
		if err.Error() != "twitch channel is offline" {
			t.Errorf("display: want %q, got %q", "twitch channel is offline", err.Error())
		}
		if errors.Is(err, ErrCookiesRequired) {
			t.Error("plain error should NOT match ErrCookiesRequired")
		}
		if errors.Is(err, ErrNonActionable) {
			t.Error("plain error should NOT match ErrNonActionable")
		}
	})

	t.Run("wraps ErrCookiesRequired", func(t *testing.T) {
		r := &StreamProcessResult{
			Error:       "Login required: Members only",
			ErrSentinel: ErrCookiesRequired,
		}
		err := r.AsError()
		if !errors.Is(err, ErrCookiesRequired) {
			t.Error("errors.Is(err, ErrCookiesRequired): want true")
		}
		if !strings.Contains(err.Error(), "Login required: Members only") {
			t.Errorf("display should preserve the user-visible message, got %q", err.Error())
		}
	})

	t.Run("wraps ErrNonActionable", func(t *testing.T) {
		r := &StreamProcessResult{
			Error:       "Age restricted: 18+",
			ErrSentinel: ErrNonActionable,
		}
		err := r.AsError()
		if !errors.Is(err, ErrNonActionable) {
			t.Error("errors.Is(err, ErrNonActionable): want true")
		}
		if errors.Is(err, ErrCookiesRequired) {
			t.Error("ErrNonActionable should NOT match ErrCookiesRequired")
		}
	})
}

// TestSentinelDistinct documents that the two main category sentinels
// don't accidentally compare equal — a single error wrapping one
// shouldn't satisfy errors.Is on the other.
func TestSentinelDistinct(t *testing.T) {
	if errors.Is(ErrCookiesRequired, ErrNonActionable) {
		t.Error("ErrCookiesRequired should not match ErrNonActionable")
	}
	if errors.Is(ErrNonActionable, ErrCookiesRequired) {
		t.Error("ErrNonActionable should not match ErrCookiesRequired")
	}
	if errors.Is(ErrCookiesRequired, ErrCancelled) {
		t.Error("ErrCookiesRequired should not match ErrCancelled")
	}
}

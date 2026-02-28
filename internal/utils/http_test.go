package utils

import (
	"context"
	"errors"
	"testing"
)

func TestRetry(t *testing.T) {
	attempts := 0
	err := Retry(context.Background(), RetryConfig{
		MaxRetries:    3,
		InitialDelay:  0, // no delay for tests
		MaxDelay:      0,
		BackoffFactor: 1,
	}, func() error {
		attempts++
		if attempts < 3 {
			return errors.New("not yet")
		}
		return nil
	})

	if err != nil {
		t.Errorf("expected success, got %v", err)
	}
	if attempts != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts)
	}
}

func TestRetryExhausted(t *testing.T) {
	err := Retry(context.Background(), RetryConfig{
		MaxRetries:    2,
		InitialDelay:  0,
		MaxDelay:      0,
		BackoffFactor: 1,
	}, func() error {
		return errors.New("always fails")
	})

	if err == nil {
		t.Error("expected error")
	}
}

func TestRetryCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := Retry(ctx, DefaultRetryConfig(), func() error {
		return errors.New("fail")
	})

	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

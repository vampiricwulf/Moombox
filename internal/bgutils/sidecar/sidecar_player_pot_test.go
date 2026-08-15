package sidecar

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"
)

// TestGeneratePlayerPoTokenParams exercises the wire-level contract of
// GeneratePlayerPoToken without a real Node subprocess or network: a
// synthetic stdin/stdout pipe pair stands in for the sidecar process, so the
// test captures exactly what JSON-RPC params GeneratePlayerPoToken sends.
//
// Two properties are under test (moombox review-hardening Finding 1):
//   - binding and challenge (when supplied) ride along in params.
//   - freshMinter is never set. Unlike GenerateGvsPoToken (which sets
//     freshMinter:true, forcing a fresh multi-second BotGuard run every
//     call), player calls happen on every probe/refresh — every live job
//     re-fetches every few minutes, plus monitor polls — so forcing a
//     fresh mint per call would be a severe cost regression.
func TestGeneratePlayerPoTokenParams(t *testing.T) {
	s := New(Config{Logger: &testLogger{t: t}})

	reqR, reqW := io.Pipe()
	respR, respW := io.Pipe()
	s.stdin = reqW
	s.stdout = respR
	s.healthy.Store(true)
	// This test never calls Start(), so s.readyCh is nil. readPump's EOF
	// path closes it via readyOnce on the very first EOF it observes;
	// pre-firing readyOnce with a no-op here means that close never
	// happens, sidestepping a "close of nil channel" panic that has
	// nothing to do with what this test is verifying.
	s.readyOnce.Do(func() {})

	type capturedReq struct {
		Method string
		Params map[string]any
	}
	reqCh := make(chan capturedReq, 4)

	// Fake sidecar process: decode each JSON-RPC request line, capture it,
	// and reply with a canned poToken response carrying the same id.
	fakeDone := make(chan struct{})
	go func() {
		defer close(fakeDone)
		scanner := bufio.NewScanner(reqR)
		for scanner.Scan() {
			var raw struct {
				ID     uint64         `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &raw); err != nil {
				continue
			}
			reqCh <- capturedReq{Method: raw.Method, Params: raw.Params}
			resp := fmt.Sprintf(`{"id":%d,"result":{"poToken":"tok-for-%d"}}`, raw.ID, raw.ID)
			if _, err := respW.Write([]byte(resp + "\n")); err != nil {
				return
			}
		}
	}()

	s.pumpsDone.Add(1)
	go s.readPump()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Case 1: challenge supplied.
	token, err := s.GeneratePlayerPoToken(ctx, "dQw4w9WgXcQ", `{"program":"p"}`)
	if err != nil {
		t.Fatalf("GeneratePlayerPoToken with challenge: %v", err)
	}
	if token == "" {
		t.Fatal("expected non-empty token")
	}
	select {
	case r := <-reqCh:
		if r.Method != "generatePoToken" {
			t.Errorf("method = %q, want generatePoToken", r.Method)
		}
		if r.Params["binding"] != "dQw4w9WgXcQ" {
			t.Errorf("binding = %v, want dQw4w9WgXcQ", r.Params["binding"])
		}
		if r.Params["challenge"] != `{"program":"p"}` {
			t.Errorf("challenge = %v, want the supplied challenge JSON", r.Params["challenge"])
		}
		if v, present := r.Params["freshMinter"]; present {
			t.Errorf("freshMinter must be absent (player tokens must not force a fresh mint per call), got %v", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for captured request (challenge case)")
	}

	// Case 2: no challenge available — the challenge key must be omitted
	// entirely, not sent as "". server.js treats a missing/invalid challenge
	// as "fall back to /att/get"; an empty string would still be a present
	// key that JSON.parse("") rejects, degrading to the same fallback via a
	// logged warning instead of cleanly.
	if _, err := s.GeneratePlayerPoToken(ctx, "dQw4w9WgXcQ", ""); err != nil {
		t.Fatalf("GeneratePlayerPoToken without challenge: %v", err)
	}
	select {
	case r := <-reqCh:
		if v, present := r.Params["challenge"]; present {
			t.Errorf("challenge key should be absent when no challenge is supplied, got %v", v)
		}
		if v, present := r.Params["freshMinter"]; present {
			t.Errorf("freshMinter must be absent, got %v", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for captured request (no-challenge case)")
	}

	// Shut down cleanly: mark stopping so readPump's EOF branch doesn't log
	// (which would race t.Logf against test completion), then close both
	// pipe halves and wait for both goroutines to actually exit.
	s.stopping.Store(true)
	_ = reqW.Close()
	_ = respW.Close()
	s.pumpsDone.Wait()
	<-fakeDone
}

// TestGenerateGvsPoTokenParams is the counterpart to
// TestGeneratePlayerPoTokenParams: GVS mints MUST set freshMinter:true. The
// provider relies on this to honour an explicit bypass_cache request — before
// review-hardening, PotProvider.generateAndMint ignored bypassCache on the
// sidecar path entirely and answered "generate fresh" from a minter that
// could be hours old, so this contract is now load-bearing in two places.
func TestGenerateGvsPoTokenParams(t *testing.T) {
	s := New(Config{Logger: &testLogger{t: t}})

	reqR, reqW := io.Pipe()
	respR, respW := io.Pipe()
	s.stdin = reqW
	s.stdout = respR
	s.healthy.Store(true)
	s.readyOnce.Do(func() {})

	type capturedReq struct {
		Method string
		Params map[string]any
	}
	reqCh := make(chan capturedReq, 4)

	fakeDone := make(chan struct{})
	go func() {
		defer close(fakeDone)
		scanner := bufio.NewScanner(reqR)
		for scanner.Scan() {
			var raw struct {
				ID     uint64         `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &raw); err != nil {
				continue
			}
			reqCh <- capturedReq{Method: raw.Method, Params: raw.Params}
			resp := fmt.Sprintf(
				`{"id":%d,"result":{"poToken":"tok","minterSource":"challenge","minterFresh":true}}`, raw.ID)
			if _, err := respW.Write([]byte(resp + "\n")); err != nil {
				return
			}
		}
	}()

	s.pumpsDone.Add(1)
	go s.readPump()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := s.GenerateGvsPoToken(ctx, "dQw4w9WgXcQ", `{"program":"p"}`)
	if err != nil {
		t.Fatalf("GenerateGvsPoToken: %v", err)
	}
	if res.PoToken != "tok" || res.MinterSource != "challenge" || !res.MinterFresh {
		t.Errorf("provenance not decoded from the wire: %+v", res)
	}
	select {
	case r := <-reqCh:
		if r.Params["freshMinter"] != true {
			t.Errorf("freshMinter = %v, want true (GVS mints must force a fresh minter)", r.Params["freshMinter"])
		}
		if r.Params["challenge"] != `{"program":"p"}` {
			t.Errorf("challenge = %v, want the supplied challenge JSON", r.Params["challenge"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for captured request")
	}

	_ = reqW.Close()
	<-fakeDone
}

package cipher

import (
	"context"
	"fmt"
)

// DecryptSignature decrypts the signature and n-parameter for a stream URL.
func (s *Solver) DecryptSignature(ctx context.Context, req SignatureRequest) (*SignatureResponse, error) {
	if req.PlayerURL == "" {
		return nil, fmt.Errorf("player URL is required")
	}

	solvers, err := s.GetSolvers(ctx, req.PlayerURL)
	if err != nil {
		return nil, fmt.Errorf("get solvers: %w", err)
	}

	resp := &SignatureResponse{}

	// Decrypt signature
	if req.EncryptedSignature != "" && solvers.Sig != nil {
		decrypted, err := solvers.DecryptSig(req.EncryptedSignature)
		if err != nil {
			return nil, fmt.Errorf("decrypt signature: %w", err)
		}
		resp.DecryptedSignature = decrypted
	}

	// Decrypt n-parameter
	if req.NParam != "" && solvers.N != nil {
		decrypted, err := solvers.DecryptN(req.NParam)
		if err != nil {
			return nil, fmt.Errorf("decrypt n-parameter: %w", err)
		}
		resp.DecryptedNSig = decrypted
	}

	return resp, nil
}

// GetSts returns the signatureTimestamp for a player URL.
func (s *Solver) GetSts(ctx context.Context, playerURL string) (string, error) {
	playerURL = OverridePlayerURL(playerURL)
	if s.stsCache == nil {
		s.stsCache = NewStsCache()
	}
	return s.stsCache.GetSts(ctx, s.playerCache, playerURL)
}

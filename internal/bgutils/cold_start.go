package bgutils

import (
	"encoding/base64"
	"encoding/binary"
	"math/rand"
	"time"
)

// GenerateColdStartToken generates a fallback PO token without BotGuard.
// This works while StreamProtectionStatus=2 (no BotGuard required).
// Algorithm: build a packet with random keys, timestamp, and XOR-encoded identifier.
func GenerateColdStartToken(identifier string, clientState int) (string, error) {
	encodedID := []byte(identifier)
	if len(encodedID) > 118 {
		return "", &BGError{Code: ErrBadConfig, Message: "identifier too long (max 118 bytes)"}
	}

	if clientState == 0 {
		clientState = 1
	}

	timestamp := time.Now().Unix()

	// Random XOR keys
	key0 := byte(rand.Intn(256))
	key1 := byte(rand.Intn(256))

	// Build header: [key0, key1, 0, clientState, timestamp (4 bytes big-endian)]
	header := make([]byte, 8)
	header[0] = key0
	header[1] = key1
	header[2] = 0
	header[3] = byte(clientState)
	binary.BigEndian.PutUint32(header[4:8], uint32(timestamp))

	// Build payload: header + encodedID
	payload := append(header, encodedID...)

	// Build packet: [34, payloadLength, ...payload]
	packet := make([]byte, 2+len(payload))
	packet[0] = 34
	packet[1] = byte(len(payload))
	copy(packet[2:], payload)

	// XOR payload bytes (starting after the random keys) with the keys
	payloadStart := 2 // skip packet header bytes
	keyLen := 2       // number of random key bytes
	for i := payloadStart + keyLen; i < len(packet); i++ {
		packet[i] ^= packet[payloadStart+(i-payloadStart)%keyLen]
	}

	// TS u8ToBase64(packet, true) keeps = padding — use URLEncoding to match
	return base64.URLEncoding.EncodeToString(packet), nil
}

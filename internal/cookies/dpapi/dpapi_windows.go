//go:build windows

package dpapi

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

var (
	crypt32                = syscall.NewLazyDLL("crypt32.dll")
	procCryptUnprotectData = crypt32.NewProc("CryptUnprotectData")
	kernel32DPAPI          = syscall.NewLazyDLL("kernel32.dll")
	procLocalFreeDPAPI     = kernel32DPAPI.NewProc("LocalFree")
)

// dataBlob mirrors the Windows DATA_BLOB struct (cbData uint32 +
// pbData *BYTE). Used both as input and output to CryptUnprotectData.
type dataBlob struct {
	cbData uint32
	pbData *byte
}

func newDataBlob(d []byte) *dataBlob {
	if len(d) == 0 {
		return &dataBlob{}
	}
	return &dataBlob{
		pbData: &d[0],
		cbData: uint32(len(d)),
	}
}

// toByteArray copies the blob's contents into a Go []byte so the caller
// owns the memory after we LocalFree the OS-allocated buffer.
func (b *dataBlob) toByteArray() []byte {
	if b.cbData == 0 {
		return nil
	}
	d := make([]byte, b.cbData)
	src := unsafe.Slice(b.pbData, b.cbData)
	copy(d, src)
	return d
}

// cryptUnprotectData wraps the Windows CryptUnprotectData API for the
// CURRENT_USER scope (the only scope Chrome uses). The OS allocates the
// output buffer; we copy out and LocalFree it. No description / entropy
// / prompt-flags are passed because Chrome's master-key blob is plain
// CURRENT_USER-DPAPI with no extra entropy.
func cryptUnprotectData(in []byte) ([]byte, error) {
	inBlob := newDataBlob(in)
	var outBlob dataBlob

	r, _, callErr := procCryptUnprotectData.Call(
		uintptr(unsafe.Pointer(inBlob)),
		0, // pszDataDescr
		0, // pOptionalEntropy
		0, // pvReserved
		0, // pPromptStruct
		0, // dwFlags
		uintptr(unsafe.Pointer(&outBlob)),
	)
	if r == 0 {
		return nil, fmt.Errorf("CryptUnprotectData: %w", callErr)
	}
	defer procLocalFreeDPAPI.Call(uintptr(unsafe.Pointer(outBlob.pbData)))

	return outBlob.toByteArray(), nil
}

// loadChromeMasterKey reads the encrypted master key from a Chrome
// profile's "Local State" file and DPAPI-decrypts it.
//
// Layout:
//
//	{profilePath}\..\Local State    JSON, contains os_crypt.encrypted_key
//	                                (base64 of "DPAPI" || dpapi_blob)
//
// profilePath should point at the profile dir (e.g. "Default" or
// "Profile 1") — the Local State file lives one level up at the
// User Data root.
//
// Returns the 32-byte AES-GCM master key suitable for decryptV10Cookie.
func loadChromeMasterKey(profilePath string) ([]byte, error) {
	localStatePath := filepath.Join(filepath.Dir(profilePath), "Local State")
	data, err := os.ReadFile(localStatePath)
	if err != nil {
		return nil, fmt.Errorf("read Local State at %q: %w", localStatePath, err)
	}

	var ls struct {
		OsCrypt struct {
			EncryptedKey string `json:"encrypted_key"`
		} `json:"os_crypt"`
	}
	if err := json.Unmarshal(data, &ls); err != nil {
		return nil, fmt.Errorf("parse Local State JSON: %w", err)
	}
	if ls.OsCrypt.EncryptedKey == "" {
		return nil, fmt.Errorf("local State has no os_crypt.encrypted_key (browser may be too old or first-run)")
	}

	raw, err := base64.StdEncoding.DecodeString(ls.OsCrypt.EncryptedKey)
	if err != nil {
		return nil, fmt.Errorf("base64-decode encrypted_key: %w", err)
	}
	const dpapiPrefix = "DPAPI"
	if len(raw) < len(dpapiPrefix) || string(raw[:len(dpapiPrefix)]) != dpapiPrefix {
		return nil, fmt.Errorf("encrypted_key missing DPAPI prefix")
	}
	return cryptUnprotectData(raw[len(dpapiPrefix):])
}

// ReadChromeCookiesStats opens the SQLite Cookies file under profilePath,
// decrypts each row's encrypted_value with the master key from Local
// State, and returns the result. originFilter (if non-empty) restricts
// the result to cookies whose host_key contains the substring
// (case-insensitive — typical use: "youtube.com" or "twitch.tv").
//
// The function fails-soft per row: a row whose encrypted_value can't
// be decrypted (legacy pre-v10 format, corrupted blob, master-key
// mismatch from a different profile) is skipped rather than failing
// the whole pass. The returned ChromeReadStats accounts for every skip
// BY REASON, which is the difference between "nothing came out" and a
// diagnosis — App-Bound encryption, a foreign master key and a failed
// meta.version probe all look identical without it.
//
// Concurrency: Chrome holds an exclusive write lock on Cookies while
// running. The SQLite open uses ?mode=ro and a 2 s busy_timeout so a
// brief lock-conflict yields a clean error instead of a hang. Callers
// that hit "database is locked" should retry after the user closes
// Chrome, or copy Cookies to a tempdir first (Chrome doesn't lock the
// copy).
func ReadChromeCookiesStats(profilePath, originFilter string) ([]ChromeCookie, ChromeReadStats, error) {
	var stats ChromeReadStats

	masterKey, err := loadChromeMasterKey(profilePath)
	if err != nil {
		return nil, stats, fmt.Errorf("load master key: %w", err)
	}

	cookiesPath := filepath.Join(profilePath, "Cookies")
	if _, statErr := os.Stat(cookiesPath); statErr != nil {
		// Chrome >= 96-ish moved cookies into a Network/ subdir.
		alt := filepath.Join(profilePath, "Network", "Cookies")
		if _, altErr := os.Stat(alt); altErr == nil {
			cookiesPath = alt
		} else {
			return nil, stats, fmt.Errorf("cookies file not found at %q (and not at Network/Cookies)", cookiesPath)
		}
	}

	dsn := "file:" + cookiesPath + "?mode=ro&_pragma=busy_timeout(2000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, stats, fmt.Errorf("open SQLite at %q: %w", cookiesPath, err)
	}
	defer db.Close()

	// Chrome ~130 (Cookies meta.version >= 24) prepends a 32-byte SHA-256 of
	// the cookie's domain to the plaintext inside every encrypted value.
	// Probe the schema before decrypting anything so the strip is gated on
	// this profile's actual version rather than assumed either way.
	metaVersion, metaKnown := readChromeMetaVersion(db)
	stats.MetaVersion = metaVersion
	if !metaKnown {
		stats.MetaProbeFailed = 1
	}
	hashPrefix := chromeUsesHashPrefix(metaVersion)

	rows, err := db.Query(`
		SELECT host_key, name, encrypted_value, path,
		       expires_utc, is_secure, is_httponly, samesite
		FROM cookies
	`)
	if err != nil {
		return nil, stats, fmt.Errorf("query cookies: %w", err)
	}
	defer rows.Close()

	filter := strings.ToLower(originFilter)
	var result []ChromeCookie
	for rows.Next() {
		var (
			host     string
			name     string
			encVal   []byte
			path     string
			expUTC   int64
			secure   int
			httpOnly int
			samesite int
		)
		if err := rows.Scan(&host, &name, &encVal, &path, &expUTC, &secure, &httpOnly, &samesite); err != nil {
			// In scope by definition: without a host there is no way to
			// know whether the filter would have excluded it.
			stats.Rows++
			stats.ScanFailed++
			continue
		}
		if filter != "" && !strings.Contains(strings.ToLower(host), filter) {
			continue
		}
		// Counted AFTER the filter so Rows means "rows this pass actually
		// considered". Counting the whole table instead would make
		// Summary()'s "N of M could not be decrypted" ratio understate
		// itself for any caller that passes an origin filter.
		stats.Rows++
		value, decryptErr := decryptV10Cookie(masterKey, encVal, hashPrefix)
		if decryptErr != nil {
			// Skip the row but don't kill the whole extraction —
			// legacy pre-v10 rows (rare today), master-key
			// mismatches, App-Bound v20 values and non-UTF-8
			// plaintexts all show up here, and they are counted by
			// reason so the caller can say WHICH one happened.
			stats.recordDecryptFailure(decryptErr)
			continue
		}
		stats.Decrypted++
		result = append(result, ChromeCookie{
			Host:     host,
			Name:     name,
			Value:    value,
			Path:     path,
			Expires:  chromeEpochToUnix(expUTC),
			Secure:   secure != 0,
			HttpOnly: httpOnly != 0,
			SameSite: chromeSameSiteString(samesite),
		})
	}
	if err := rows.Err(); err != nil {
		return result, stats, fmt.Errorf("rows iteration: %w", err)
	}
	return result, stats, nil
}

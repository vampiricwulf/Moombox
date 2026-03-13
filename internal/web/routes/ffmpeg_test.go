package routes

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestParseFFmpegVersion(t *testing.T) {
	tests := []struct {
		name  string
		input string
		major int
		minor int
		ok    bool
	}{
		{"standard_7.1", "ffmpeg version 7.1 Copyright (c) 2000-2024", 7, 1, true},
		{"standard_4.4", "ffmpeg version 4.4.2 Copyright (c) 2000-2022", 4, 4, true},
		{"old_3.4", "ffmpeg version 3.4 Copyright (c) 2000-2017", 3, 4, true},
		{"major_only", "ffmpeg version 6.0", 6, 0, true},
		{"git_build", "ffmpeg version N-12345-g1234567 Copyright", 0, 0, false},
		{"empty", "", 0, 0, false},
		{"no_version_keyword", "not ffmpeg output at all", 0, 0, false},
		{"partial_match", "ffmpeg version x.y", 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			major, minor, ok := parseFFmpegVersion(tt.input)
			if major != tt.major || minor != tt.minor || ok != tt.ok {
				t.Errorf("parseFFmpegVersion(%q) = (%d, %d, %v), want (%d, %d, %v)",
					tt.input, major, minor, ok, tt.major, tt.minor, tt.ok)
			}
		})
	}
}

func TestFFmpegVersionWarning(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantMsg bool
	}{
		{"modern_7.1", "ffmpeg version 7.1 Copyright", false},
		{"v4_boundary", "ffmpeg version 4.0 Copyright", false},
		{"old_3.4", "ffmpeg version 3.4 Copyright", true},
		{"very_old_2.8", "ffmpeg version 2.8 Copyright", true},
		{"git_build", "ffmpeg version N-12345-g1234567", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ffmpegVersionWarning(tt.input)
			if tt.wantMsg && got == "" {
				t.Error("expected warning, got empty string")
			}
			if !tt.wantMsg && got != "" {
				t.Errorf("expected no warning, got %q", got)
			}
		})
	}
}

func TestTruncateOutput(t *testing.T) {
	tests := []struct {
		name      string
		input     []byte
		maxBytes  int
		wantTrunc bool
		contains  string
	}{
		{"short", []byte("hello"), 10, false, "hello"},
		{"exact", []byte("hello"), 5, false, "hello"},
		{"truncated", []byte("hello world"), 5, true, "world"},
		{"empty", []byte{}, 10, false, ""},
		{"single_byte", []byte("x"), 1, false, "x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateOutput(tt.input, tt.maxBytes)
			if tt.wantTrunc && !strings.HasPrefix(got, "[...truncated...]") {
				t.Errorf("expected truncation marker, got %q", got)
			}
			if !tt.wantTrunc && strings.HasPrefix(got, "[...truncated...]") {
				t.Errorf("unexpected truncation marker in %q", got)
			}
			if !strings.Contains(got, tt.contains) {
				t.Errorf("expected to contain %q, got %q", tt.contains, got)
			}
		})
	}

	// UTF-8 multi-byte: ensure truncation doesn't produce invalid UTF-8.
	// "héllo" in UTF-8: h(0x68) é(0xC3 0xA9) l(0x6C) l(0x6C) o(0x6F) = 6 bytes
	t.Run("utf8_multibyte", func(t *testing.T) {
		input := []byte("héllo")
		// maxBytes=4 starts at byte 2 (0xA9, a continuation byte).
		// The fix should skip past it to produce valid UTF-8.
		got := truncateOutput(input, 4)
		if !strings.Contains(got, "llo") {
			t.Errorf("expected truncated output to contain 'llo', got %q", got)
		}
		// Verify no orphan continuation byte at the start (after marker)
		after := strings.TrimPrefix(got, "[...truncated...]\n")
		if len(after) > 0 && after[0]&0xC0 == 0x80 {
			t.Errorf("truncated output starts with UTF-8 continuation byte: %q", got)
		}
	})
}

func TestExtractRegValue(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			"reg_sz",
			"HKEY_LOCAL_MACHINE\\...\r\n    Path    REG_SZ    C:\\Windows;C:\\Go\\bin\r\n\r\n",
			"C:\\Windows;C:\\Go\\bin",
		},
		{
			"reg_expand_sz",
			"HKEY_LOCAL_MACHINE\\...\r\n    Path    REG_EXPAND_SZ    C:\\Windows;C:\\Go\\bin\r\n\r\n",
			"C:\\Windows;C:\\Go\\bin",
		},
		{"empty", "", ""},
		{"no_path_line", "HKEY_LOCAL_MACHINE\\...\r\n    Other    REG_SZ    value\r\n", ""},
		{
			"mixed_case_path",
			"    path    REG_SZ    C:\\Test\r\n",
			"C:\\Test",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractRegValue(tt.input)
			if got != tt.want {
				t.Errorf("extractRegValue() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExpandWindowsEnv(t *testing.T) {
	t.Run("no_vars", func(t *testing.T) {
		got := expandWindowsEnv("C:\\Windows")
		if got != "C:\\Windows" {
			t.Errorf("expandWindowsEnv() = %q, want %q", got, "C:\\Windows")
		}
	})

	t.Run("single_var", func(t *testing.T) {
		t.Setenv("MOOMBOX_TEST_A", "C:\\Test")
		got := expandWindowsEnv("%MOOMBOX_TEST_A%\\bin")
		want := "C:\\Test\\bin"
		if got != want {
			t.Errorf("expandWindowsEnv() = %q, want %q", got, want)
		}
	})

	t.Run("unknown_var_kept", func(t *testing.T) {
		got := expandWindowsEnv("%MOOMBOX_NONEXISTENT_VAR%\\bin")
		want := "%MOOMBOX_NONEXISTENT_VAR%\\bin"
		if got != want {
			t.Errorf("expandWindowsEnv() = %q, want %q", got, want)
		}
	})

	t.Run("multiple_vars", func(t *testing.T) {
		t.Setenv("MOOMBOX_TEST_X", "hello")
		t.Setenv("MOOMBOX_TEST_Y", "world")
		got := expandWindowsEnv("%MOOMBOX_TEST_X%\\%MOOMBOX_TEST_Y%")
		want := "hello\\world"
		if got != want {
			t.Errorf("expandWindowsEnv() = %q, want %q", got, want)
		}
	})
}

func TestGenerateInstallScript(t *testing.T) {
	resultPath := `C:\Temp\moombox-ffmpeg-result-abc123.json`

	tests := []struct {
		name     string
		method   string
		wantErr  bool
		contains []string // substrings the script must contain
	}{
		{
			"choco",
			MethodChoco,
			false,
			[]string{"choco install " + chocoFFmpegPkg, "$ErrorActionPreference", resultPath, "ConvertTo-Json"},
		},
		{
			"choco_install",
			MethodChocoInstall,
			false,
			[]string{
				"chocolatey.org/install.ps1",
				"choco install " + chocoFFmpegPkg,
				resultPath,
			},
		},
		{
			"winget",
			MethodWinget,
			false,
			[]string{"winget install " + wingetFFmpegPkg, "--accept-package-agreements", resultPath},
		},
		{
			"unknown_method",
			"apt-get",
			true,
			nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			script, err := generateInstallScript(tt.method, resultPath)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			for _, sub := range tt.contains {
				if !strings.Contains(script, sub) {
					t.Errorf("script missing expected substring %q\nscript:\n%s", sub, script)
				}
			}
		})
	}

	// Verify single-quote escaping in result path
	t.Run("single_quote_in_path", func(t *testing.T) {
		pathWithQuote := `C:\Users\O'Brien\AppData\Local\Temp\result.json`
		script, err := generateInstallScript(MethodChoco, pathWithQuote)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// PowerShell escapes single quotes by doubling them
		if !strings.Contains(script, "O''Brien") {
			t.Errorf("expected escaped single quote (O''Brien) in script, got:\n%s", script)
		}
		if strings.Contains(script, "O'B") && !strings.Contains(script, "O''B") {
			t.Errorf("unescaped single quote found in script:\n%s", script)
		}
	})
}

func TestGenerateToken(t *testing.T) {
	token, err := generateToken()
	if err != nil {
		t.Fatalf("generateToken() error: %v", err)
	}
	// 16 bytes → 32 hex characters
	if len(token) != 32 {
		t.Errorf("token length = %d, want 32", len(token))
	}
	// Must be valid hex
	for _, c := range token {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("token contains non-hex character %q: %s", c, token)
			break
		}
	}
	// Two tokens must differ
	token2, err := generateToken()
	if err != nil {
		t.Fatalf("generateToken() error: %v", err)
	}
	if token == token2 {
		t.Error("two consecutive tokens are identical")
	}
}

func TestCleanExpiredPending(t *testing.T) {
	// Reset global state
	pendingInstallsMu.Lock()
	orig := pendingInstalls
	pendingInstalls = make(map[string]*pendingInstall)
	pendingInstallsMu.Unlock()
	defer func() {
		pendingInstallsMu.Lock()
		pendingInstalls = orig
		pendingInstallsMu.Unlock()
	}()

	// Add an expired and a fresh entry
	pendingInstallsMu.Lock()
	pendingInstalls["expired-token"] = &pendingInstall{
		method:    MethodChoco,
		createdAt: time.Now().Add(-10 * time.Minute), // well past TTL
	}
	pendingInstalls["fresh-token"] = &pendingInstall{
		method:    MethodWinget,
		createdAt: time.Now(),
	}
	cleanExpiredPending()
	_, expiredExists := pendingInstalls["expired-token"]
	_, freshExists := pendingInstalls["fresh-token"]
	pendingInstallsMu.Unlock()

	if expiredExists {
		t.Error("expired pending install was not cleaned")
	}
	if !freshExists {
		t.Error("fresh pending install was incorrectly removed")
	}
}

func TestIsRebootRequired(t *testing.T) {
	t.Run("nil_error", func(t *testing.T) {
		if isRebootRequired(nil) {
			t.Error("isRebootRequired(nil) = true, want false")
		}
	})

	t.Run("non_exit_error", func(t *testing.T) {
		if isRebootRequired(os.ErrNotExist) {
			t.Error("isRebootRequired(os.ErrNotExist) = true, want false")
		}
	})

	// Exit code 3010 test requires running a real process.
	// Only works on Windows where we can specify arbitrary exit codes.
	if runtime.GOOS == "windows" {
		t.Run("exit_3010", func(t *testing.T) {
			err := exec.Command("cmd", "/c", "exit 3010").Run()
			if !isRebootRequired(err) {
				t.Error("expected isRebootRequired to return true for exit code 3010")
			}
		})

		t.Run("exit_1_not_reboot", func(t *testing.T) {
			err := exec.Command("cmd", "/c", "exit 1").Run()
			if isRebootRequired(err) {
				t.Error("expected isRebootRequired to return false for exit code 1")
			}
		})
	}
}

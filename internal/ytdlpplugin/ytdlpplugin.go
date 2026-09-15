// Package ytdlpplugin computes and writes the yt-dlp PO-token provider plugin
// Moombox installs for yt-dlp.
//
// It lives outside internal/web/routes because two very different surfaces ask
// the same questions about that one file on disk — GET /api/ytdlp-plugin/status
// for the dashboard's Integrations card, and the TUI's R Y overlay — and the
// terminal must not have to import the HTTP layer (and, transitively, the
// BotGuard embed blobs) to read a struct. Its own imports are stdlib only.
package ytdlpplugin

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
)

var pluginURLRe = regexp.MustCompile(`(?:DEFAULT_BASE_URL|_MOOMBOX_URL)\s*=\s*["'](https?)://[^:]+:(\d+)["']`)

// ParseInstalled extracts the scheme and port from a plugin file's DEFAULT_BASE_URL line.
func ParseInstalled(content string) (scheme string, port int) {
	m := pluginURLRe.FindStringSubmatch(content)
	if m == nil {
		return "", 0
	}
	fmt.Sscanf(m[2], "%d", &port)
	return m[1], port
}

// Info is what GET /api/ytdlp-plugin/status reports and what the TUI's R Y
// overlay shows. The JSON tags are the wire contract: settings.js's
// loadYtdlpPluginStatus reads all seven keys, and they were an inline
// map[string]any until the TUI needed the same answer.
//
// InstalledPort is a POINTER, without omitempty, so the wire still says
// "installedPort":null when no plugin file parsed — the shape settings.js has
// always been handed, and the convention CLAUDE.md sets for optional numerics.
// Nothing dereferences it blindly: the route marshals it and the TUI's row is
// gated on the pointer being non-nil.
type Info struct {
	Installed     bool   `json:"installed"`
	PluginDir     string `json:"pluginDir"`
	CurrentPort   int    `json:"currentPort"`
	HTTPSEnabled  bool   `json:"httpsEnabled"`
	InstalledPort *int   `json:"installedPort"`
	PortMismatch  bool   `json:"portMismatch"`
	ExtractedPath string `json:"extractedPath"`
}

// Status reads the installed plugin file, if any, and reports it against the
// port and scheme this process is actually serving on.
//
// Shared by the GET route and the TUI's R Y overlay rather than reimplemented
// for the terminal: "installed" and "mismatched" are file-parsing verdicts,
// and two copies of the parse are two answers to the same question.
//
// The error return is for a future read failure that is worth reporting; an
// undeterminable plugin directory is NOT one — it is reported as an empty
// PluginDir with Installed false, exactly as the route always has, because
// that is the state of a platform where yt-dlp has no standard plugin dir
// rather than a failure of this call.
func Status(port int, httpsEnabled bool) (Info, error) {
	pluginDir := Dir()
	info := Info{
		PluginDir:    pluginDir,
		CurrentPort:  port,
		HTTPSEnabled: httpsEnabled,
	}

	expectedScheme := "http"
	if httpsEnabled {
		expectedScheme = "https"
	}

	if pluginDir != "" {
		pluginPath := filepath.Join(pluginDir, "moombox", "yt_dlp_plugins", "extractor", "getpot_moombox.py")
		if data, err := os.ReadFile(pluginPath); err == nil {
			info.Installed = true
			if scheme, p := ParseInstalled(string(data)); p > 0 {
				parsed := p
				info.InstalledPort = &parsed
				info.PortMismatch = p != port || scheme != expectedScheme
			}
		}
		// extractedPath: the plugin source dir for --plugin-dirs usage.
		// In Go we generate inline, so point to the installed location.
		info.ExtractedPath = filepath.Join(pluginDir, "moombox")
	}

	return info, nil
}

// Install writes the yt-dlp PO token provider plugin to the standard yt-dlp
// plugin directory. Called from the TUI setup wizard, the R Y overlay's I key
// and the web install route.
func Install(port int, httpsEnabled bool) error {
	pluginDir := Dir()
	if pluginDir == "" {
		return fmt.Errorf("cannot determine yt-dlp plugin directory")
	}
	targetDir := filepath.Join(pluginDir, "moombox", "yt_dlp_plugins", "extractor")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return fmt.Errorf("create plugin directory: %w", err)
	}
	pluginPath := filepath.Join(targetDir, "getpot_moombox.py")
	return os.WriteFile(pluginPath, []byte(Generate(port, httpsEnabled)), 0o644)
}

// Dir is the standard yt-dlp plugin directory for this platform, or "" where
// there is none. Callers report "" as "not installed", not as an error.
func Dir() string {
	// Use the standard yt-dlp plugin directory for the platform
	// (matches TypeScript: APPDATA/yt-dlp/plugins on Windows, ~/.config/yt-dlp/plugins elsewhere)
	switch runtime.GOOS {
	case "windows":
		if appdata := os.Getenv("APPDATA"); appdata != "" {
			return filepath.Join(appdata, "yt-dlp", "plugins")
		}
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "Library", "Application Support", "yt-dlp", "plugins")
		}
	default:
		if xdgConfig := os.Getenv("XDG_CONFIG_HOME"); xdgConfig != "" {
			return filepath.Join(xdgConfig, "yt-dlp", "plugins")
		}
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, ".config", "yt-dlp", "plugins")
		}
	}
	return ""
}

// Generate renders the plugin's Python source for a given port and scheme.
func Generate(port int, httpsEnabled bool) string {
	scheme := "http"
	if httpsEnabled {
		scheme = "https"
	}
	baseURL := fmt.Sprintf("%s://127.0.0.1:%d", scheme, port)

	// When HTTPS is enabled, the plugin uses Python's stdlib urllib.request
	// directly with a custom SSL context for localhost calls. yt-dlp's request
	// handler pipeline does not support ssl_context as an extension, so we
	// bypass it entirely for these simple localhost HTTP calls.
	sslImports := ""
	sslSetup := ""
	sslHelper := ""
	if httpsEnabled {
		sslImports = "\nimport ssl\nimport urllib.request"
		sslSetup = `

def _make_localhost_ssl_context():
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    return ctx

_LOCALHOST_SSL_CTX = _make_localhost_ssl_context()
`
		// Helper that uses stdlib urllib for HTTPS localhost requests,
		// since yt-dlp's networking stack doesn't support custom SSL contexts.
		sslHelper = `
    def _localhost_request(self, url, data=None, headers=None, timeout=10):
        """Make a direct HTTP(S) request to localhost, bypassing yt-dlp's networking."""
        req = urllib.request.Request(url, data=data, headers=headers or {})
        return urllib.request.urlopen(req, timeout=timeout, context=_LOCALHOST_SSL_CTX)
`
	}

	// When HTTPS is disabled, _localhost_request uses yt-dlp's standard
	// _request_webpage. When enabled, it uses the stdlib override above.
	if !httpsEnabled {
		sslHelper = `
    def _localhost_request(self, url, data=None, headers=None, timeout=10):
        """Make a request to localhost via yt-dlp's networking."""
        return self._request_webpage(
            Request(url, data=data, headers=headers or {},
                    extensions={'timeout': timeout}, proxies={'all': None}),
            note=False,
        )
`
	}

	return fmt.Sprintf(`"""
yt-dlp PO Token provider plugin for Moombox.

Uses Moombox's built-in /get_pot HTTP endpoint to generate PO tokens.
Moombox handles BotGuard internally, so no challenge extraction is needed.

Install: copy to yt-dlp's plugin directory, or use --plugin-dirs to point at this folder.
"""
from __future__ import annotations

import functools
import json
import time%s

from yt_dlp.extractor.youtube.pot.provider import (
    PoTokenContext,
    PoTokenProvider,
    PoTokenProviderError,
    PoTokenProviderRejectedRequest,
    PoTokenRequest,
    PoTokenResponse,
    register_preference,
    register_provider,
)
from yt_dlp.extractor.youtube.pot.utils import WEBPO_CLIENTS, get_webpo_content_binding
from yt_dlp.networking.common import Request
from yt_dlp.networking.exceptions import TransportError
%s

@register_provider
class MoomboxPTP(PoTokenProvider):
    PROVIDER_NAME = 'moombox'
    BUG_REPORT_LOCATION = 'https://github.com/vampiricwulf/Moombox/issues'
    _SUPPORTED_CLIENTS = WEBPO_CLIENTS
    _SUPPORTED_CONTEXTS = (PoTokenContext.GVS, PoTokenContext.PLAYER, PoTokenContext.SUBS)
    _PING_TIMEOUT = 5.0
    _GETPOT_TIMEOUT = 20.0
    DEFAULT_BASE_URL = '%s'

    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self._last_server_check = 0
        self._server_available = True

    @functools.cached_property
    def _base_url(self):
        base_url = self._configuration_arg('base_url', default=[None])[0]
        if base_url:
            return base_url
        self.logger.debug(f'No base_url provided, defaulting to {self.DEFAULT_BASE_URL}')
        return self.DEFAULT_BASE_URL
%s
    def _check_server_availability(self):
        if self._last_server_check + 60 > time.time():
            return self._server_available

        self._server_available = False
        try:
            self.logger.trace(f'Checking Moombox server at {self._base_url}/ping')
            self._localhost_request(f'{self._base_url}/ping', timeout=self._PING_TIMEOUT)
        except TransportError as e:
            self.logger.warning(
                f'Moombox server not reachable at {self._base_url}/ping '
                f'(caused by {e.__class__.__name__}). '
                f'Make sure Moombox is running.',
                once=True,
            )
            raise PoTokenProviderRejectedRequest(
                f'Moombox server not reachable at {self._base_url}'
            ) from e
        except Exception as e:
            self.logger.warning(
                f'Error reaching Moombox /ping (caused by {e!r})',
                once=True,
            )
            raise PoTokenProviderRejectedRequest(
                f'Error reaching Moombox: {e!r}'
            ) from e
        else:
            self._server_available = True
            return True
        finally:
            self._last_server_check = time.time()

    def is_available(self):
        return self._server_available or self._last_server_check + 60 < int(time.time())

    def _real_request_pot(self, request: PoTokenRequest) -> PoTokenResponse:
        if not self._check_server_availability():
            raise PoTokenProviderRejectedRequest('Moombox server is not available')

        self.logger.trace('Generating PO token via Moombox')

        try:
            response = self._localhost_request(
                f'{self._base_url}/get_pot',
                data=json.dumps({
                    'bypass_cache': request.bypass_cache,
                    'content_binding': get_webpo_content_binding(request)[0],
                }).encode(),
                headers={'Content-Type': 'application/json'},
                timeout=self._GETPOT_TIMEOUT,
            )
        except Exception as e:
            raise PoTokenProviderError(
                f'Error reaching Moombox POST /get_pot (caused by {e!r})'
            ) from e

        try:
            response_json = json.load(response)
        except Exception as e:
            raise PoTokenProviderError(
                f'Error parsing Moombox response JSON (caused by {e!r})'
            ) from e

        if error_msg := response_json.get('error'):
            raise PoTokenProviderError(error_msg)

        if 'poToken' not in response_json:
            raise PoTokenProviderError(
                f'Moombox did not respond with a poToken. Response: {response_json}'
            )

        po_token = response_json['poToken']
        self.logger.trace(f'Got PO token from Moombox: {po_token[:30]}...')
        return PoTokenResponse(po_token=po_token)


@register_preference(MoomboxPTP)
def moombox_getpot_preference(provider, request):
    return 150


__all__ = [MoomboxPTP.__name__, moombox_getpot_preference.__name__]
`, sslImports, sslSetup, baseURL, sslHelper)
}

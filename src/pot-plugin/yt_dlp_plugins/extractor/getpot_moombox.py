"""
yt-dlp PO Token provider plugin for Moombox.

Uses Moombox's built-in /get_pot HTTP endpoint to generate PO tokens.
Moombox handles BotGuard internally, so no challenge extraction is needed.

Install: copy to yt-dlp's plugin directory, or use --plugin-dirs to point at this folder.
"""
from __future__ import annotations

import functools
import json
import time

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


@register_provider
class MoomboxPTP(PoTokenProvider):
    PROVIDER_NAME = 'moombox'
    BUG_REPORT_LOCATION = 'https://github.com/Wulf/Moombox/issues'
    _SUPPORTED_CLIENTS = WEBPO_CLIENTS
    _SUPPORTED_CONTEXTS = (PoTokenContext.GVS, PoTokenContext.PLAYER, PoTokenContext.SUBS)
    _PING_TIMEOUT = 5.0
    _GETPOT_TIMEOUT = 20.0
    DEFAULT_BASE_URL = 'http://127.0.0.1:774'

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

    def _check_server_availability(self):
        if self._last_server_check + 60 > time.time():
            return self._server_available

        self._server_available = False
        try:
            self.logger.trace(f'Checking Moombox server at {self._base_url}/ping')
            self._request_webpage(
                Request(
                    f'{self._base_url}/ping',
                    extensions={'timeout': self._PING_TIMEOUT},
                    proxies={'all': None},
                ),
                note=False,
            )
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
            response = self._request_webpage(
                request=Request(
                    f'{self._base_url}/get_pot',
                    data=json.dumps({
                        'bypass_cache': request.bypass_cache,
                        'content_binding': get_webpo_content_binding(request)[0],
                    }).encode(),
                    headers={'Content-Type': 'application/json'},
                    extensions={'timeout': self._GETPOT_TIMEOUT},
                    proxies={'all': None},
                ),
                note=f'Requesting {request.context.value} PO token from Moombox '
                     f'for {request.internal_client_name} client',
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

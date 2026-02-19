import BotGuardClient from './botGuardClient.js';
import WebPoMinter from './webPoMinter.js';
import { buildURL, u8ToBase64, getHeaders, BGError } from '../utils/index.js';
import type { PoTokenArgs, PoTokenResult, WebPoSignalOutput } from '../utils/index.js';

/**
 * Generates a Proof of Origin Token.
 * @param args - The arguments for generating the token.
 */
export async function generate(args: PoTokenArgs): Promise<PoTokenResult> {
  const { program, bgConfig, globalName } = args;
  const { identifier } = bgConfig;

  const botguard = await BotGuardClient.create({ program, globalName, globalObj: bgConfig.globalObj });

  const webPoSignalOutput: WebPoSignalOutput = [];
  const botguardResponse = await botguard.snapshot({ webPoSignalOutput });

  const payload = [ bgConfig.requestKey, botguardResponse ];

  const integrityTokenResponse = await bgConfig.fetch(buildURL('GenerateIT', bgConfig.useYouTubeAPI), {
    method: 'POST',
    headers: getHeaders(),
    body: JSON.stringify(payload)
  });

  if (!integrityTokenResponse.ok) {
    throw new Error(`GenerateIT request failed: HTTP ${integrityTokenResponse.status}`);
  }

  const integrityTokenJson = await integrityTokenResponse.json() as [string, number, number, string];

  const [ integrityToken, estimatedTtlSecs, mintRefreshThreshold, websafeFallbackToken ] = integrityTokenJson;

  const integrityTokenData = {
    integrityToken,
    estimatedTtlSecs,
    mintRefreshThreshold,
    websafeFallbackToken
  };

  const webPoMinter = await WebPoMinter.create(integrityTokenData, webPoSignalOutput);

  const poToken = await webPoMinter.mintAsWebsafeString(identifier);

  return { poToken, integrityTokenData };
}

/**
 * Creates a cold start token. This can be used while `sps` (StreamProtectionStatus) is 2, but will not work once it changes to 3.
 * @param identifier - Visitor ID or Data Sync ID.
 * @param clientState - The client state.
 */
export function generateColdStartToken(identifier: string, clientState?: number): string {
  const encodedIdentifier = new TextEncoder().encode(identifier);

  if (encodedIdentifier.length > 118)
    throw new BGError('BAD_INPUT', 'Content binding is too long.', { identifierLength: encodedIdentifier.length });

  const timestamp = Math.floor(Date.now() / 1000);
  const randomKeys = [ Math.floor(Math.random() * 256), Math.floor(Math.random() * 256) ];

  // NOTE: The "0" value before the client state is supposed to be someVal & 0xFF.
  // It is always 0 though, so I didn't bother investigating further.
  const header = randomKeys.concat(
    [
      0, (clientState ?? 1)
    ],
    [
      (timestamp >> 24) & 0xFF,
      (timestamp >> 16) & 0xFF,
      (timestamp >> 8) & 0xFF,
      timestamp & 0xFF
    ]
  );

  const packet = new Uint8Array(2 + header.length + encodedIdentifier.length);

  packet[0] = 34;
  packet[1] = header.length + encodedIdentifier.length;

  packet.set(header, 2);
  packet.set(encodedIdentifier, 2 + header.length);

  const payload = packet.subarray(2);

  const keyLength = randomKeys.length;

  for (let i = keyLength; i < payload.length; i++) {
    payload[i] ^= payload[i % keyLength];
  }

  return u8ToBase64(packet, true);
}


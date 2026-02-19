/**
 * Self-Signed Certificate Generator
 *
 * Generates a self-signed X.509 certificate for HTTPS using pure Node.js crypto.
 * No external dependencies required.
 */

import crypto from "crypto";
import fs from "fs-extra";
import path from "path";

const CERT_FILENAME = "moombox-cert.pem";
const KEY_FILENAME = "moombox-key.pem";

// =============================================================================
// ASN.1 DER Encoding Helpers
// =============================================================================

function encodeLength(length: number): Buffer {
  if (length < 0x80) return Buffer.from([length]);
  if (length < 0x100) return Buffer.from([0x81, length]);
  return Buffer.from([0x82, (length >> 8) & 0xff, length & 0xff]);
}

function encodeTLV(tag: number, value: Buffer): Buffer {
  const len = encodeLength(value.length);
  return Buffer.concat([Buffer.from([tag]), len, value]);
}

function encodeSequence(...items: Buffer[]): Buffer {
  return encodeTLV(0x30, Buffer.concat(items));
}

function encodeSet(...items: Buffer[]): Buffer {
  return encodeTLV(0x31, Buffer.concat(items));
}

function encodeInteger(value: Buffer | number): Buffer {
  let buf: Buffer;
  if (typeof value === "number") {
    // Encode small integers
    if (value < 0x80) {
      buf = Buffer.from([value]);
    } else if (value < 0x8000) {
      buf = Buffer.from([(value >> 8) & 0xff, value & 0xff]);
    } else {
      buf = Buffer.alloc(4);
      buf.writeUInt32BE(value);
      // Strip leading zeros but keep sign bit
      while (buf.length > 1 && buf[0] === 0 && buf[1]! < 0x80) buf = buf.subarray(1);
    }
  } else {
    buf = value;
    // Ensure positive integer (prepend 0x00 if high bit set)
    if (buf[0]! >= 0x80) {
      buf = Buffer.concat([Buffer.from([0x00]), buf]);
    }
  }
  return encodeTLV(0x02, buf);
}

function encodeBitString(value: Buffer): Buffer {
  // Bit string: first byte is number of unused bits (0)
  return encodeTLV(0x03, Buffer.concat([Buffer.from([0x00]), value]));
}

function encodeOctetString(value: Buffer): Buffer {
  return encodeTLV(0x04, value);
}

function encodeOID(oid: string): Buffer {
  const parts = oid.split(".").map(Number);
  const bytes: number[] = [parts[0]! * 40 + parts[1]!];
  for (let i = 2; i < parts.length; i++) {
    let val = parts[i]!;
    if (val < 128) {
      bytes.push(val);
    } else {
      const encoded: number[] = [];
      encoded.push(val & 0x7f);
      val >>= 7;
      while (val > 0) {
        encoded.push(0x80 | (val & 0x7f));
        val >>= 7;
      }
      encoded.reverse();
      bytes.push(...encoded);
    }
  }
  return encodeTLV(0x06, Buffer.from(bytes));
}

function encodeUTF8String(str: string): Buffer {
  return encodeTLV(0x0c, Buffer.from(str, "utf-8"));
}

function encodeUTCTime(date: Date): Buffer {
  const y = date.getUTCFullYear() % 100;
  const s = [
    y.toString().padStart(2, "0"),
    (date.getUTCMonth() + 1).toString().padStart(2, "0"),
    date.getUTCDate().toString().padStart(2, "0"),
    date.getUTCHours().toString().padStart(2, "0"),
    date.getUTCMinutes().toString().padStart(2, "0"),
    date.getUTCSeconds().toString().padStart(2, "0"),
    "Z",
  ].join("");
  return encodeTLV(0x17, Buffer.from(s, "ascii"));
}

function encodeGeneralizedTime(date: Date): Buffer {
  const s = [
    date.getUTCFullYear().toString().padStart(4, "0"),
    (date.getUTCMonth() + 1).toString().padStart(2, "0"),
    date.getUTCDate().toString().padStart(2, "0"),
    date.getUTCHours().toString().padStart(2, "0"),
    date.getUTCMinutes().toString().padStart(2, "0"),
    date.getUTCSeconds().toString().padStart(2, "0"),
    "Z",
  ].join("");
  return encodeTLV(0x18, Buffer.from(s, "ascii"));
}

function encodeExplicit(tag: number, value: Buffer): Buffer {
  return encodeTLV(0xa0 | tag, value);
}

function encodeBooleanTrue(): Buffer {
  return encodeTLV(0x01, Buffer.from([0xff]));
}

// =============================================================================
// Certificate Builder
// =============================================================================

// OIDs
const OID_RSA_ENCRYPTION = "1.2.840.113549.1.1.1";
const OID_SHA256_WITH_RSA = "1.2.840.113549.1.1.11";
const OID_COMMON_NAME = "2.5.4.3";
const OID_SUBJECT_ALT_NAME = "2.5.29.17";
const OID_BASIC_CONSTRAINTS = "2.5.29.19";

/**
 * Build a self-signed X.509 v3 certificate in DER format.
 */
function buildCertificate(
  publicKeyDer: Buffer,
  privateKey: crypto.KeyObject,
): Buffer {
  const now = new Date();
  const notAfter = new Date(now);
  notAfter.setUTCFullYear(notAfter.getUTCFullYear() + 10);

  // Serial number — random 16 bytes
  const serial = crypto.randomBytes(16);
  serial[0] = serial[0]! & 0x7f; // Ensure positive

  // Signature algorithm
  const sigAlgo = encodeSequence(encodeOID(OID_SHA256_WITH_RSA), Buffer.from([0x05, 0x00]));

  // Issuer / Subject: CN=Moombox
  const cn = encodeSet(encodeSequence(encodeOID(OID_COMMON_NAME), encodeUTF8String("Moombox")));
  const issuer = encodeSequence(cn);
  const subject = issuer;

  // Validity
  // Use UTCTime for dates before 2050, GeneralizedTime otherwise
  const encodeTime = (d: Date) => d.getUTCFullYear() < 2050 ? encodeUTCTime(d) : encodeGeneralizedTime(d);
  const validity = encodeSequence(encodeTime(now), encodeTime(notAfter));

  // Subject Public Key Info — wrap the raw public key DER
  const spki = publicKeyDer;

  // Extensions (v3)
  // Basic Constraints: CA=FALSE
  const basicConstraints = encodeSequence(
    encodeOID(OID_BASIC_CONSTRAINTS),
    encodeOctetString(encodeSequence()), // empty sequence = CA:FALSE
  );

  // Subject Alternative Name: DNS:localhost, IP:127.0.0.1, IP:::1
  const sanDns = encodeTLV(0x82, Buffer.from("localhost", "ascii")); // context [2] = dNSName
  const sanIpv4 = encodeTLV(0x87, Buffer.from([127, 0, 0, 1])); // context [7] = iPAddress
  const ipv6Loopback = Buffer.alloc(16, 0);
  ipv6Loopback[15] = 1;
  const sanIpv6 = encodeTLV(0x87, ipv6Loopback); // context [7] = iPAddress
  const sanExtension = encodeSequence(
    encodeOID(OID_SUBJECT_ALT_NAME),
    encodeBooleanTrue(), // critical
    encodeOctetString(encodeSequence(sanDns, sanIpv4, sanIpv6)),
  );

  const extensions = encodeExplicit(3, encodeSequence(basicConstraints, sanExtension));

  // TBS Certificate
  const tbsCert = encodeSequence(
    encodeExplicit(0, encodeInteger(2)), // version v3
    encodeInteger(serial),
    sigAlgo,
    issuer,
    validity,
    subject,
    spki,
    extensions,
  );

  // Sign the TBS certificate
  const signer = crypto.createSign("SHA256");
  signer.update(tbsCert);
  const signature = signer.sign(privateKey);

  // Full certificate
  return encodeSequence(tbsCert, sigAlgo, encodeBitString(signature));
}

// =============================================================================
// Public API
// =============================================================================

export interface CertificateResult {
  key: string;
  cert: string;
}

/**
 * Ensure a self-signed TLS certificate exists in the given directory.
 * If certificates already exist, they are loaded and returned.
 * Otherwise, new ones are generated and saved.
 */
export function ensureCertificate(certDir: string): CertificateResult {
  const certPath = path.join(certDir, CERT_FILENAME);
  const keyPath = path.join(certDir, KEY_FILENAME);

  // Return existing certs if both files exist
  if (fs.existsSync(certPath) && fs.existsSync(keyPath)) {
    return {
      key: fs.readFileSync(keyPath, "utf-8"),
      cert: fs.readFileSync(certPath, "utf-8"),
    };
  }

  // Generate new RSA 2048 key pair
  const { publicKey, privateKey } = crypto.generateKeyPairSync("rsa", {
    modulusLength: 2048,
  });

  // Get public key in SPKI DER format (for embedding in certificate)
  const publicKeyDer = publicKey.export({ type: "spki", format: "der" });

  // Build the self-signed certificate
  const certDer = buildCertificate(publicKeyDer, privateKey);

  // Convert to PEM
  const certPem = [
    "-----BEGIN CERTIFICATE-----",
    certDer.toString("base64").match(/.{1,64}/g)!.join("\n"),
    "-----END CERTIFICATE-----",
    "",
  ].join("\n");

  const keyPem = privateKey.export({ type: "pkcs8", format: "pem" }) as string;

  // Write files
  fs.ensureDirSync(certDir);
  fs.writeFileSync(keyPath, keyPem, { mode: 0o600 });
  fs.writeFileSync(certPath, certPem, { mode: 0o600 });

  return { key: keyPem, cert: certPem };
}

/**
 * IP Address Validation Utilities
 *
 * Provides functions for validating and categorizing IP addresses.
 */

/**
 * Check if an IP address is a private/LAN address
 *
 * @param ip - IP address to check (IPv4 or IPv6)
 * @returns True if the IP is in a private range
 */
export function isPrivateIP(ip: string): boolean {
  return (
    ip === "127.0.0.1" ||
    ip === "::1" ||
    ip === "::ffff:127.0.0.1" ||
    ip.startsWith("192.168.") ||
    ip.startsWith("::ffff:192.168.") ||
    ip.startsWith("10.") ||
    ip.startsWith("::ffff:10.") ||
    /^(::ffff:)?172\.(1[6-9]|2\d|3[01])\./.test(ip)
  );
}

/**
 * Check if an IP address is a loopback address
 *
 * @param ip - IP address to check (IPv4 or IPv6)
 * @returns True if the IP is localhost/loopback
 */
export function isLoopback(ip: string): boolean {
  return ip === "127.0.0.1" || ip === "::1" || ip === "::ffff:127.0.0.1";
}

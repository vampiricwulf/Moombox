/**
 * Shared validation utilities for route handlers
 */

/**
 * Validate and parse offset-based pagination parameters
 *
 * @param offset - Offset (string from query param)
 * @param limit - Items per page (string from query param)
 * @returns Validated pagination values
 * @throws Error if parameters are invalid
 */
export function validateOffsetPagination(
  offset?: string,
  limit?: string
): { offset: number; limit: number | undefined } {
  const parsedOffset = offset ? parseInt(offset, 10) : 0;
  const parsedLimit = limit ? parseInt(limit, 10) : undefined;

  if (isNaN(parsedOffset) || parsedOffset < 0) {
    throw new Error(`Invalid offset parameter (must be >= 0): ${offset}`);
  }

  if (parsedLimit !== undefined) {
    if (isNaN(parsedLimit) || parsedLimit < 1 || parsedLimit > 1000) {
      throw new Error(`Invalid limit parameter (must be 1-1000): ${limit}`);
    }
  }

  return { offset: parsedOffset, limit: parsedLimit };
}

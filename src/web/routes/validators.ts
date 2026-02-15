/**
 * Shared validation utilities for route handlers
 */

import { LIMITS } from "../../constants/limits.js";

/**
 * Validate and parse page-based pagination parameters
 *
 * @param page - Page number (string from query param)
 * @param limit - Items per page (string from query param)
 * @returns Validated pagination values
 * @throws Error if parameters are invalid
 */
export function validatePagination(
  page?: string,
  limit?: string
): { page: number; limit: number } {
  const parsedPage = page ? parseInt(page, 10) : 1;
  const parsedLimit = limit ? parseInt(limit, 10) : LIMITS.DEFAULT_ITEMS_PER_PAGE;

  if (
    parsedPage < 1 ||
    parsedLimit < 1 ||
    parsedLimit > LIMITS.MAX_ITEMS_PER_PAGE
  ) {
    throw new Error(
      `Invalid pagination parameters: page=${parsedPage}, limit=${parsedLimit} (max ${LIMITS.MAX_ITEMS_PER_PAGE})`
    );
  }

  return { page: parsedPage, limit: parsedLimit };
}

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

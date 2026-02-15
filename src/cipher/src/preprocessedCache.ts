import { LRUCache } from 'lru-cache';
import { env } from '../../core/env.js';

// The key is the hash of the player URL, and the value is the preprocessed script content.
export const preprocessedCache = new LRUCache<string, string>({ max: env.PREPROCESSED_CACHE_SIZE });
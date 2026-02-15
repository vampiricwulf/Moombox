import { LRUCache } from 'lru-cache';
import { env } from '../../core/env.js';

// key = hash of player URL
export const stsCache = new LRUCache<string, string>({ max: env.STS_CACHE_SIZE });
import { LRUCache } from 'lru-cache';

// key = hash of player URL
const cacheSizeEnv = process.env.STS_CACHE_SIZE;
const maxCacheSize = cacheSizeEnv ? parseInt(cacheSizeEnv, 10) : 150;
export const stsCache = new LRUCache<string, string>({ max: maxCacheSize });
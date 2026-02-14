import { LRUCache } from 'lru-cache';

// The key is the hash of the player URL, and the value is the preprocessed script content.
const cacheSizeEnv = process.env.PREPROCESSED_CACHE_SIZE;
const maxCacheSize = cacheSizeEnv ? parseInt(cacheSizeEnv, 10) : 150;
export const preprocessedCache = new LRUCache<string, string>({ max: maxCacheSize });
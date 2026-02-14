import { LRUCache } from 'lru-cache';
import type { Solvers } from "./types.js";

// key = hash of the player url
const cacheSizeEnv = process.env.SOLVER_CACHE_SIZE;
const maxCacheSize = cacheSizeEnv ? parseInt(cacheSizeEnv, 10) : 50;
export const solverCache = new LRUCache<string, Solvers>({ max: maxCacheSize });
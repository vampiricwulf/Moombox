import { LRUCache } from 'lru-cache';
import type { Solvers } from "./types.js";
import { env } from '../../core/env.js';

// key = hash of the player url
export const solverCache = new LRUCache<string, Solvers>({ max: env.SOLVER_CACHE_SIZE });
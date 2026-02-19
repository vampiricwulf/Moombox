import { preprocessPlayer } from "../../ejs/yt/solver/solvers.js";

// Simple inline execution instead of worker pool
// This avoids the complexity of TypeScript workers and tsx loader issues
export async function execInPool(data: string): Promise<string> {
  return preprocessPlayer(data);
}

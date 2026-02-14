import { parentPort } from "worker_threads";
import { preprocessPlayer } from "../ejs/yt/solver/solvers.js";

if (parentPort) {
  parentPort.on("message", async (data: string) => {
    try {
      const output = preprocessPlayer(data);
      parentPort!.postMessage({ type: "success", data: output });
    } catch (error) {
      parentPort!.postMessage({
        type: "error",
        data: {
          message: error instanceof Error ? error.message : String(error),
          stack: error instanceof Error ? error.stack : undefined,
        },
      });
    }
  });

  // Signal ready
  parentPort.postMessage({ type: "ready" });
}

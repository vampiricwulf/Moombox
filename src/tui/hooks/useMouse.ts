import { useEffect, useRef } from "react";
import { mouseDataBus } from "../stdinFilter.js";

export interface MouseEvent {
  type: "click" | "scroll" | "move";
  button: "left" | "right" | "middle" | "scrollUp" | "scrollDown" | "none";
  x: number;
  y: number;
  shift: boolean;
  meta: boolean;
  ctrl: boolean;
}

interface UseMouseOptions {
  onMouseEvent?: (event: MouseEvent) => void;
}

export function useMouse(options: UseMouseOptions = {}): void {
  // Store callback in a ref so the effect doesn't re-run when it changes.
  const onMouseEventRef = useRef(options.onMouseEvent);
  onMouseEventRef.current = options.onMouseEvent;

  useEffect(() => {
    // Listen on the mouseDataBus (populated by stdinFilter) instead of raw stdin.
    // This avoids Ink seeing mouse escape sequences as typed characters.
    const handleData = (data: Buffer) => {
      const str = data.toString();

      // SGR mouse event format: \x1b[<Cb;Cx;CyM or \x1b[<Cb;Cx;Cym
      const sgrMatch = str.match(/\x1b\[<(\d+);(\d+);(\d+)([Mm])/);
      if (sgrMatch) {
        const [, buttonCode, col, row, pressRelease] = sgrMatch;
        const code = parseInt(buttonCode, 10);
        const x = parseInt(col, 10) - 1; // Convert to 0-indexed
        const y = parseInt(row, 10) - 1;
        const isPress = pressRelease === "M";

        // Decode button and modifiers
        const shift = (code & 4) !== 0;
        const meta = (code & 8) !== 0;
        const ctrl = (code & 16) !== 0;
        const buttonBits = code & 3;
        const isScroll = (code & 64) !== 0;
        const isMove = (code & 32) !== 0;

        let button: MouseEvent["button"] = "none";
        let type: MouseEvent["type"] = "click";

        if (isScroll) {
          type = "scroll";
          button = buttonBits === 0 ? "scrollUp" : "scrollDown";
        } else if (isMove) {
          type = "move";
        } else if (isPress) {
          type = "click";
          switch (buttonBits) {
            case 0:
              button = "left";
              break;
            case 1:
              button = "middle";
              break;
            case 2:
              button = "right";
              break;
          }
        }

        // Only emit events we care about (not releases or moves without button)
        if (type === "click" || type === "scroll") {
          const event: MouseEvent = { type, button, x, y, shift, meta, ctrl };
          onMouseEventRef.current?.(event);
        }
      }
    };

    mouseDataBus.on("data", handleData);

    return () => {
      mouseDataBus.off("data", handleData);
    };
  }, []);
}

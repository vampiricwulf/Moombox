import { useState, useEffect } from "react";

export function useProgressTick(active: boolean): number {
  const [tick, setTick] = useState(0);
  useEffect(() => {
    if (!active) return;
    const timer = setInterval(() => setTick(t => t + 1), 100);
    return () => clearInterval(timer);
  }, [active]);
  return tick;
}

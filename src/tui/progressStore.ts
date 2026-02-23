export interface ProgressData {
  progress: string;
  percent: number;
  speed: string;
  eta: string;
  lastVideoSeq?: number;
  lastAudioSeq?: number;
  totalVideoSeq?: number;
  totalAudioSeq?: number;
  totalChatMessages?: number;
  chatStatus?: string;
}

// Module-level — no React involvement, zero allocations on read
const store = new Map<string, ProgressData>();

export function setProgress(jobId: string, data: ProgressData): void {
  store.set(jobId, data);
}

export function getProgress(jobId: string): ProgressData | undefined {
  return store.get(jobId);
}

export function deleteProgress(jobId: string): void {
  store.delete(jobId);
}

export function clearProgress(): void {
  store.clear();
}

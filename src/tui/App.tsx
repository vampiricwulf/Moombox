import React, { useState, useEffect, useCallback, useMemo, useRef } from "react";
import { Box, Text, useInput, useStdout, useApp } from "ink";
import path from "path";
import { spawn } from "child_process";
import { ConfigManager } from "../core/config.js";
import { Database, Job } from "../core/database.js";
import { Logger } from "../core/logger.js";
import { TaskList } from "./components/TaskList.js";
import { LogViewer } from "./components/LogViewer.js";
import { StatusBar } from "./components/StatusBar.js";
import { JobDetails } from "./components/JobDetails.js";
import { HelpOverlay } from "./components/HelpOverlay.js";
import { SetupWizard } from "./components/SetupWizard.js";
import { AddVideoDialog } from "./components/AddVideoDialog.js";
import { TrimDialog } from "./components/TrimDialog.js";
import { SettingsPanel } from "./components/SettingsPanel.js";
import { useMouse, MouseEvent } from "./hooks/useMouse.js";
import { FeedMonitor } from "../core/monitor.js";
import { DecapiMonitor } from "../core/decapiMonitor.js";
import { NotificationManager, NotificationType } from "../core/notifications.js";
import { readClipboard } from "./clipboard.js";
import { extractVideoId } from "../utils/youtube.js";
import { getErrorMessage } from "../types/errors.js";

type FocusPanel = "tasks" | "details" | "logs";
const FOCUS_ORDER: FocusPanel[] = ["tasks", "details", "logs"];

type StatusFilter = "all" | "active" | "errors" | "finished";
const FILTER_ORDER: StatusFilter[] = ["all", "active", "errors", "finished"];
const FILTER_LABELS: Record<StatusFilter, string> = {
  all: "All",
  active: "Active",
  errors: "Errors",
  finished: "Finished",
};

interface AppProps {
  db: Database;
  logger: Logger;
}

export function App({ db, logger }: AppProps): React.ReactElement {
  const { stdout } = useStdout();
  const [jobs, setJobs] = useState<Job[]>([]);
  const [logs, setLogs] = useState<string[]>([]);
  const [focusedPanel, setFocusedPanel] = useState<FocusPanel>("tasks");
  const [selectedJobIndex, setSelectedJobIndex] = useState(0);
  const [taskScrollOffset, setTaskScrollOffset] = useState(0);
  const [detailScrollOffset, setDetailScrollOffset] = useState(0);
  const [logScrollOffset, setLogScrollOffset] = useState(0);
  const [addDialogOpen, setAddDialogOpen] = useState(false);
  const [addMessage, setAddMessage] = useState<{ text: string; color: string } | null>(null);
  const [deleteConfirmJobId, setDeleteConfirmJobId] = useState<string | null>(null);
  const [trimDialogOpen, setTrimDialogOpen] = useState(false);
  const [trimDialogJob, setTrimDialogJob] = useState<Job | null>(null);
  const [statusFilter, setStatusFilter] = useState<StatusFilter>("all");
  const [helpMode, setHelpMode] = useState(false);
  const [helpScrollOffset, setHelpScrollOffset] = useState(0);
  const [logAutoScroll, setLogAutoScroll] = useState(true);
  const [archiveExpanded, setArchiveExpanded] = useState(false);
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [setupMode, setSetupMode] = useState(() => !ConfigManager.getInstance().hasConfig());
  const [, setTick] = useState(0);

  // 1-second timer for countdown display updates
  useEffect(() => {
    const timer = setInterval(() => setTick((t) => t + 1), 1000);
    return () => clearInterval(timer);
  }, []);

  // Get terminal dimensions
  const rows = stdout?.rows || 24;
  const cols = stdout?.columns || 80;

  // Layout: top half = tasks + details side by side, bottom = logs
  const statusBarHeight = 1;
  const availableHeight = rows - statusBarHeight;

  // Top panel gets more space when tasks or details focused, less when logs focused
  const topHeight = focusedPanel === "logs"
    ? Math.floor(availableHeight * 0.25)
    : Math.floor(availableHeight * 0.7);
  const logHeight = availableHeight - topHeight;

  // Split width for task list and details panel
  const taskWidth = Math.floor(cols / 2);
  const detailWidth = cols - taskWidth;

  const addBarHeight = addMessage ? 1 : 0;
  const taskHeight = topHeight - addBarHeight;

  // Memoize sorted jobs (unfiltered + filtered)
  const allSortedJobs = useMemo(() => getSortedJobs(jobs), [jobs]);

  const statusCounts = useMemo(() => {
    const counts: Record<string, number> = {};
    for (const job of allSortedJobs) {
      counts[job.status] = (counts[job.status] || 0) + 1;
    }
    return counts;
  }, [allSortedJobs]);

  const sortedJobs = useMemo(() => {
    if (statusFilter === "all") return allSortedJobs;
    return allSortedJobs.filter((job) => {
      switch (statusFilter) {
        case "active":
          return ["Live", "Downloading", "Muxing", "Upcoming"].includes(job.status);
        case "errors":
          return ["Error", "Cancelled", "COOKIES?"].includes(job.status);
        case "finished":
          return job.status === "Finished";
        default:
          return true;
      }
    });
  }, [allSortedJobs, statusFilter]);

  // Compute archived jobs (finished jobs older than hideAgeDays)
  const archivedJobs = useMemo(() => {
    const config = ConfigManager.getInstance().get();
    const hideAgeDays = config.tasklist?.hide_finished_age_days || 30;
    const now = Date.now();

    return jobs
      .filter((job) => {
        if (job.status !== "Finished") return false;
        const diffDays = Math.ceil(
          (now - new Date(job.updatedAt).getTime()) / (1000 * 60 * 60 * 24),
        );
        return diffDays > hideAgeDays;
      })
      .sort((a, b) => new Date(b.updatedAt).getTime() - new Date(a.updatedAt).getTime());
  }, [jobs]);

  // Virtual list: [sortedJobs..., divider?, archivedJobs...]
  // Only show archive section when filter includes Finished jobs
  const showArchive = archivedJobs.length > 0 && (statusFilter === "all" || statusFilter === "finished");
  const hasDivider = showArchive;
  const dividerIndex = sortedJobs.length;
  const virtualListLength = sortedJobs.length
    + (hasDivider ? 1 : 0)
    + (hasDivider && archiveExpanded ? archivedJobs.length : 0);

  // Resolve a virtual index to a job (or null for divider)
  const getJobAtVirtualIndex = useCallback((idx: number): Job | null => {
    if (idx < sortedJobs.length) return sortedJobs[idx];
    if (hasDivider && idx === dividerIndex) return null; // divider
    if (hasDivider && archiveExpanded && idx > dividerIndex) {
      const archiveIdx = idx - dividerIndex - 1;
      return archivedJobs[archiveIdx] || null;
    }
    return null;
  }, [sortedJobs, archivedJobs, hasDivider, dividerIndex, archiveExpanded]);

  // Clamp selectedJobIndex when jobs or virtual list change
  useEffect(() => {
    if (virtualListLength > 0) {
      setSelectedJobIndex((prev) => Math.min(prev, virtualListLength - 1));
      setTaskScrollOffset((prev) => Math.min(prev, Math.max(0, virtualListLength - (taskHeight - 3))));
    } else {
      setSelectedJobIndex(0);
      setTaskScrollOffset(0);
    }
  }, [virtualListLength, taskHeight]);

  // When archive collapses, clamp index to at most the divider row
  useEffect(() => {
    if (!archiveExpanded && hasDivider) {
      setSelectedJobIndex((prev) => Math.min(prev, dividerIndex));
    }
  }, [archiveExpanded, hasDivider, dividerIndex]);

  // Reset detail scroll when selected job changes
  useEffect(() => {
    setDetailScrollOffset(0);
  }, [selectedJobIndex]);

  // Ref for selectedJobIndex to avoid stale closures
  const selectedJobIndexRef = useRef(selectedJobIndex);
  selectedJobIndexRef.current = selectedJobIndex;

  // Refs so mouse handler can read these without stale closures
  const addDialogOpenRef = useRef(addDialogOpen);
  addDialogOpenRef.current = addDialogOpen;
  const settingsOpenRef = useRef(settingsOpen);
  settingsOpenRef.current = settingsOpen;

  // Handle mouse events
  const handleMouseEvent = useCallback(
    (event: MouseEvent) => {
      // Ignore mouse events when dialog/settings is open
      if (addDialogOpenRef.current || settingsOpenRef.current) {
        return;
      }

      if (event.type === "click" && event.button === "left") {
        // Determine which panel was clicked
        let targetPanel: FocusPanel;
        if (event.y < topHeight) {
          targetPanel = event.x < taskWidth ? "tasks" : "details";
        } else {
          targetPanel = "logs";
        }

        // When clicking away from logs, re-enable auto-scroll
        if (focusedPanel === "logs" && targetPanel !== "logs") {
          setLogScrollOffset(Number.MAX_SAFE_INTEGER);
          setLogAutoScroll(true);
        }

        setFocusedPanel(targetPanel);

        if (targetPanel === "tasks" && event.y < taskHeight) {
          const taskIndex = event.y - 2 + taskScrollOffset;
          if (taskIndex >= 0 && taskIndex < virtualListLength) {
            setSelectedJobIndex(taskIndex);
            // Click on archive divider toggles it
            if (hasDivider && taskIndex === dividerIndex) {
              setArchiveExpanded((prev) => !prev);
            }
          }
        }
      } else if (event.type === "scroll") {
        if (focusedPanel === "tasks") {
          const maxScroll = Math.max(0, virtualListLength - (taskHeight - 3));
          if (event.button === "scrollUp") {
            setTaskScrollOffset((prev) => Math.max(0, prev - 3));
            setSelectedJobIndex((prev) => Math.max(0, prev - 3));
          } else if (event.button === "scrollDown") {
            setTaskScrollOffset((prev) => Math.min(maxScroll, prev + 3));
            setSelectedJobIndex((prev) => Math.min(virtualListLength - 1, prev + 3));
          }
        } else if (focusedPanel === "details") {
          if (event.button === "scrollUp") {
            setDetailScrollOffset((prev) => Math.max(0, prev - 3));
          } else if (event.button === "scrollDown") {
            setDetailScrollOffset((prev) => prev + 3);
          }
        } else {
          const maxScroll = Math.max(0, logs.length - (logHeight - 3));
          if (event.button === "scrollUp") {
            setLogScrollOffset((prev) => Math.max(0, Math.min(prev, maxScroll) - 3));
            setLogAutoScroll(false);
          } else if (event.button === "scrollDown") {
            setLogScrollOffset((prev) => {
              const next = Math.min(maxScroll, prev + 3);
              if (next >= maxScroll) setLogAutoScroll(true);
              return next;
            });
          }
        }
      }
    },
    [sortedJobs, virtualListLength, logs.length, topHeight, taskWidth, taskHeight, logHeight, taskScrollOffset, focusedPanel],
  );

  useMouse({ onMouseEvent: handleMouseEvent });

  // Set terminal window title
  useEffect(() => {
    if (!stdout) return;
    const activeCount = allSortedJobs.filter(
      (j) => j.status === "Downloading" || j.status === "Live" || j.status === "Muxing",
    ).length;
    const upcomingCount = allSortedJobs.filter((j) => j.status === "Upcoming").length;
    const parts = ["Moombox"];
    if (activeCount > 0) parts.push(`${activeCount} active`);
    if (upcomingCount > 0) parts.push(`${upcomingCount} upcoming`);
    const title = parts.join(" \u2014 ");
    stdout.write(`\x1b]0;${title}\x07`);
  }, [stdout, allSortedJobs]);

  // Load jobs
  useEffect(() => {
    const loadJobs = async () => {
      const allJobs = await db.getJobs();
      setJobs(allJobs);
    };

    loadJobs();

    const unsubUpdate = db.onJobUpdate((job: Job) => {
      setJobs((prev: Job[]) => {
        const idx = prev.findIndex((j: Job) => j.id === job.id);
        if (idx !== -1) {
          const newJobs = [...prev];
          newJobs[idx] = job;
          return newJobs;
        }
        return prev;
      });
    });

    const unsubChange = db.onJobsChange((newJobs: Job[]) => {
      setJobs(newJobs);
    });

    return () => {
      unsubUpdate();
      unsubChange();
    };
  }, [db]);

  // Ref for logAutoScroll to avoid stale closures
  const logAutoScrollRef = useRef(logAutoScroll);
  logAutoScrollRef.current = logAutoScroll;

  // Subscribe to logs (batched — flush every 100ms to avoid excessive re-renders)
  const logBufferRef = useRef<string[]>([]);
  const flushTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => {
    const flush = () => {
      flushTimerRef.current = null;
      if (logBufferRef.current.length === 0) return;
      const batch = logBufferRef.current;
      logBufferRef.current = [];
      setLogs((prev: string[]) => {
        const combined = [...prev, ...batch];
        return combined.length > 1000 ? combined.slice(-1000) : combined;
      });
      if (logAutoScrollRef.current) {
        setLogScrollOffset(Number.MAX_SAFE_INTEGER);
      }
    };

    const unsubscribe = logger.subscribe((msg: string) => {
      logBufferRef.current.push(msg);
      if (!flushTimerRef.current) {
        flushTimerRef.current = setTimeout(flush, 100);
      }
    });

    return () => {
      unsubscribe();
      if (flushTimerRef.current) {
        clearTimeout(flushTimerRef.current);
        flushTimerRef.current = null;
      }
    };
  }, [logger]);

  const { exit } = useApp();

  // Clear add message after timeout
  useEffect(() => {
    if (!addMessage) return;
    const timer = setTimeout(() => setAddMessage(null), 3000);
    return () => clearTimeout(timer);
  }, [addMessage]);

  // Clear delete confirmation after timeout
  useEffect(() => {
    if (!deleteConfirmJobId) return;
    const timer = setTimeout(() => {
      setDeleteConfirmJobId(null);
      setAddMessage({ text: "Delete cancelled", color: "gray" });
    }, 3000);
    return () => clearTimeout(timer);
  }, [deleteConfirmJobId]);

  // Keyboard input
  useInput((input, key) => {
    if (setupMode || addDialogOpen || settingsOpen) return;

    // Toggle help
    if (input === "?") {
      setHelpMode((prev) => !prev);
      setHelpScrollOffset(0);
      setDeleteConfirmJobId(null);
      return;
    }

    // Open settings
    if (input === "`") {
      setSettingsOpen(true);
      return;
    }

    // Help mode: swallow all keys except Esc and arrows
    if (helpMode) {
      if (key.escape) {
        setHelpMode(false);
      } else if (key.upArrow) {
        setHelpScrollOffset((prev) => Math.max(0, prev - 1));
      } else if (key.downArrow) {
        setHelpScrollOffset((prev) => prev + 1);
      }
      return;
    }

    // Clear delete confirmation on any non-D key
    if (deleteConfirmJobId && input !== "d" && input !== "D") {
      setDeleteConfirmJobId(null);
    }

    // Quit
    if (input === "q" || input === "Q" || (key.ctrl && input === "c")) {
      exit();
      return;
    }

    // Tab cycles focus: tasks → details → logs → tasks
    if (key.tab) {
      setFocusedPanel((prev) => {
        const idx = FOCUS_ORDER.indexOf(prev);
        const next = FOCUS_ORDER[(idx + 1) % FOCUS_ORDER.length];
        if (prev === "logs") {
          setLogScrollOffset(Number.MAX_SAFE_INTEGER);
          setLogAutoScroll(true);
        }
        return next;
      });
      return;
    }

    // Open web dashboard in browser
    if (input === "w" || input === "W") {
      const port = process.env.MOOMBOX_PORT || String(ConfigManager.getInstance().get().port || 774);
      const url = `https://localhost:${port}`;
      const [prog, args] = process.platform === "win32"
        ? ["explorer.exe", [url]]
        : [process.platform === "darwin" ? "open" : "xdg-open", [url]];
      spawn(prog, args, { stdio: "ignore", detached: true }).unref();
      setAddMessage({ text: `Opening: ${url}`, color: "green" });
      return;
    }

    // Filter (only from tasks panel)
    if ((input === "f" || input === "F") && focusedPanel === "tasks") {
      setStatusFilter((prev) => {
        const idx = FILTER_ORDER.indexOf(prev);
        return FILTER_ORDER[(idx + 1) % FILTER_ORDER.length];
      });
      setSelectedJobIndex(0);
      setTaskScrollOffset(0);
      return;
    }

    // Add video (only from tasks panel)
    if ((input === "a" || input === "A") && focusedPanel === "tasks") {
      setAddDialogOpen(true);
      setAddMessage(null);
      return;
    }

    // Panel-specific controls
    if (focusedPanel === "tasks") {
      handleTaskInput(input, key);
    } else if (focusedPanel === "details") {
      handleDetailInput(key);
    } else {
      handleLogInput(key);
    }
  });

  const handleTaskInput = useCallback(
    (input: string, key: { upArrow?: boolean; downArrow?: boolean; return?: boolean }) => {
      if (key.upArrow) {
        setSelectedJobIndex((prev: number) => Math.max(0, prev - 1));
        if (selectedJobIndexRef.current <= taskScrollOffset) {
          setTaskScrollOffset((prev: number) => Math.max(0, prev - 1));
        }
      } else if (key.downArrow) {
        setSelectedJobIndex((prev: number) => Math.min(virtualListLength - 1, prev + 1));
        const visibleItems = taskHeight - 3;
        if (selectedJobIndexRef.current >= taskScrollOffset + visibleItems - 1) {
          const maxScroll = Math.max(0, virtualListLength - visibleItems);
          setTaskScrollOffset((prev: number) => Math.min(maxScroll, prev + 1));
        }
      } else if (key.return) {
        // Enter on the divider row toggles archive expansion
        if (hasDivider && selectedJobIndexRef.current === dividerIndex) {
          setArchiveExpanded((prev) => !prev);
        }
      } else if (input === "c" || input === "C") {
        const job = getJobAtVirtualIndex(selectedJobIndexRef.current);
        if (job && job.status !== "Finished" && job.status !== "Cancelled" && job.status !== "Error") {
          db.updateJob(job.id, { status: "Cancelled" }).catch((e: unknown) => logger.error(`[TUI] Failed to cancel job: ${getErrorMessage(e)}`));
          NotificationManager.getInstance().send(
            "Job Cancelled",
            `Cancelled: ${job.title}`,
            NotificationType.CANCELLED,
            [
              { name: "Channel", value: job.channelName, inline: true },
              { name: "Video ID", value: job.videoId, inline: true },
            ],
            {
              url: `https://www.youtube.com/watch?v=${job.videoId}`,
              thumbnail: job.thumbnailUrl,
              event: "cancelled",
            },
          );
        }
      } else if (input === "r" || input === "R") {
        const job = getJobAtVirtualIndex(selectedJobIndexRef.current);
        if (job && (job.status === "Error" || job.status === "Cancelled")) {
          db.updateJob(job.id, { status: "Upcoming", error: undefined }).catch((e: unknown) => logger.error(`[TUI] Failed to retry job: ${getErrorMessage(e)}`));
        }
      } else if (input === "d" || input === "D") {
        const job = getJobAtVirtualIndex(selectedJobIndexRef.current);
        if (job) {
          if (deleteConfirmJobId === job.id) {
            db.deleteJob(job.id).catch((e: unknown) => logger.error(`[TUI] Failed to delete job: ${getErrorMessage(e)}`));
            setSelectedJobIndex((prev: number) => Math.max(0, prev - 1));
            setDeleteConfirmJobId(null);
            setAddMessage({ text: `Deleted: ${job.title}`, color: "gray" });
          } else {
            setDeleteConfirmJobId(job.id);
            setAddMessage({ text: `Press D again to delete "${job.title}"`, color: "yellow" });
          }
        }
      } else if (input === "o" || input === "O") {
        const job = getJobAtVirtualIndex(selectedJobIndexRef.current);
        if (job && job.status === "Finished" && job.filename) {
          const config = ConfigManager.getInstance().get();
          const outputDir = job.outputDirectory || config.downloader?.output_directory || "./output";
          const filePath = path.resolve(path.join(outputDir, job.filename));
          const folderPath = path.dirname(filePath);
          const [prog, args] = process.platform === "win32"
            ? ["explorer.exe", [folderPath]]
            : [process.platform === "darwin" ? "open" : "xdg-open", [folderPath]];
          spawn(prog, args, { stdio: "ignore", detached: true }).unref();
          setAddMessage({ text: `Opening: ${folderPath}`, color: "green" });
        } else {
          setAddMessage({ text: "Can only open folder for finished jobs", color: "yellow" });
        }
      } else if (input === "t" || input === "T") {
        const job = getJobAtVirtualIndex(selectedJobIndexRef.current);
        if (job && job.status === "Finished" && job.filename) {
          setTrimDialogOpen(true);
          setTrimDialogJob(job);
        } else {
          setAddMessage({
            text: "Trim only available for finished jobs with files",
            color: "yellow",
          });
        }
      }
    },
    [sortedJobs, virtualListLength, hasDivider, dividerIndex, getJobAtVirtualIndex, taskScrollOffset, taskHeight, db, deleteConfirmJobId],
  );

  const handleDetailInput = useCallback(
    (key: { upArrow?: boolean; downArrow?: boolean }) => {
      if (key.upArrow) {
        setDetailScrollOffset((prev: number) => Math.max(0, prev - 1));
      } else if (key.downArrow) {
        setDetailScrollOffset((prev: number) => prev + 1);
      }
    },
    [],
  );

  const handleLogInput = useCallback(
    (key: { upArrow?: boolean; downArrow?: boolean; pageUp?: boolean; pageDown?: boolean }) => {
      const maxScroll = Math.max(0, logs.length - (logHeight - 3));

      if (key.upArrow) {
        setLogScrollOffset((prev: number) => Math.max(0, Math.min(prev, maxScroll) - 1));
        setLogAutoScroll(false);
      } else if (key.downArrow) {
        setLogScrollOffset((prev: number) => {
          const next = Math.min(maxScroll, prev + 1);
          if (next >= maxScroll) setLogAutoScroll(true);
          return next;
        });
      } else if (key.pageUp) {
        setLogScrollOffset((prev: number) => Math.max(0, Math.min(prev, maxScroll) - (logHeight - 3)));
        setLogAutoScroll(false);
      } else if (key.pageDown) {
        setLogScrollOffset((prev: number) => {
          const next = Math.min(maxScroll, prev + (logHeight - 3));
          if (next >= maxScroll) setLogAutoScroll(true);
          return next;
        });
      }
    },
    [logs.length, logHeight],
  );

  const selectedJob = getJobAtVirtualIndex(selectedJobIndex) ?? undefined;

  if (setupMode) {
    return <SetupWizard width={cols} height={rows} onComplete={() => setSetupMode(false)} />;
  }

  if (addDialogOpen) {
    return (
      <AddVideoDialog
        width={cols}
        height={rows}
        onComplete={(message) => {
          setAddDialogOpen(false);
          setAddMessage(message);
        }}
        onCancel={() => {
          setAddDialogOpen(false);
          setAddMessage(null);
        }}
      />
    );
  }

  if (trimDialogOpen && trimDialogJob) {
    return (
      <TrimDialog
        job={trimDialogJob}
        width={cols}
        height={rows}
        onComplete={(message) => {
          setTrimDialogOpen(false);
          setTrimDialogJob(null);
          setAddMessage(message);
        }}
        onCancel={() => {
          setTrimDialogOpen(false);
          setTrimDialogJob(null);
        }}
      />
    );
  }

  if (settingsOpen) {
    return (
      <SettingsPanel
        width={cols}
        height={rows}
        onClose={async (needsRestart) => {
          setSettingsOpen(false);
          if (needsRestart) {
            try {
              const { restart } = await import("../index.js");
              await restart();
            } catch (e: unknown) {
              setAddMessage({ text: `Restart failed: ${getErrorMessage(e)}`, color: "red" });
            }
          }
        }}
      />
    );
  }

  if (helpMode) {
    return (
      <Box flexDirection="column" height={rows} width={cols}>
        <HelpOverlay
          width={cols}
          height={rows - statusBarHeight}
          scrollOffset={helpScrollOffset}
        />
        <StatusBar
          focusedPanel={focusedPanel}
          jobs={sortedJobs}
          width={cols}
        />
      </Box>
    );
  }

  return (
    <Box flexDirection="column" height={rows} width={cols}>
      {/* Top row: TaskList + JobDetails side by side */}
      <Box flexDirection="row" height={topHeight} width={cols}>
        <Box flexDirection="column" width={taskWidth} height={topHeight}>
          <TaskList
            jobs={sortedJobs}
            selectedIndex={selectedJobIndex}
            scrollOffset={taskScrollOffset}
            height={taskHeight}
            width={taskWidth}
            focused={focusedPanel === "tasks"}
            totalJobCount={allSortedJobs.length}
            filterLabel={statusFilter !== "all" ? FILTER_LABELS[statusFilter] : undefined}
            statusCounts={statusCounts}
            archivedJobs={showArchive ? archivedJobs : []}
            archiveExpanded={archiveExpanded}
            onToggleArchive={() => setArchiveExpanded((prev) => !prev)}
            nextFeedCheck={FeedMonitor.getInstance().nextCheckAt}
            nextDecapiCheck={DecapiMonitor.getInstance().nextCheckAt}
          />
          {addMessage && (
            <Box width={taskWidth} paddingX={1}>
              <Text color={addMessage.color}>{addMessage.text}</Text>
            </Box>
          )}
        </Box>
        <JobDetails
          job={selectedJob}
          width={detailWidth}
          height={topHeight}
          focused={focusedPanel === "details"}
          scrollOffset={detailScrollOffset}
        />
      </Box>
      {/* Bottom: LogViewer */}
      <LogViewer
        logs={logs}
        scrollOffset={logScrollOffset}
        height={logHeight}
        width={cols}
        focused={focusedPanel === "logs"}
        autoScroll={logAutoScroll}
      />
      <StatusBar
        focusedPanel={focusedPanel}
        jobs={sortedJobs}
        width={cols}
      />
    </Box>
  );
}

const STATUS_PRIORITY: Record<string, number> = {
  "Error": 0, "COOKIES?": 1, "Downloading": 2, "Muxing": 3,
  "Live": 4, "Upcoming": 5, "Cancelled": 6, "Finished": 7,
};

function getSortedJobs(jobs: Job[]): Job[] {
  const config = ConfigManager.getInstance().get();
  const hideAgeDays = config.tasklist?.hide_finished_age_days || 30;
  const now = Date.now();

  return jobs
    .filter((job) => {
      if (job.status !== "Finished") return true;
      const diffDays = Math.ceil(
        (now - new Date(job.updatedAt).getTime()) / (1000 * 60 * 60 * 24),
      );
      return diffDays <= hideAgeDays;
    })
    .sort((a, b) => {
      const pa = STATUS_PRIORITY[a.status] ?? 99;
      const pb = STATUS_PRIORITY[b.status] ?? 99;
      if (pa !== pb) return pa - pb;
      // Finished/Cancelled/Error: most recently updated first
      if (pa >= 6) return new Date(b.updatedAt).getTime() - new Date(a.updatedAt).getTime();
      return (a.title || "").localeCompare(b.title || "", undefined, { sensitivity: "base" });
    });
}

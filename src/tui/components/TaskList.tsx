import React from "react";
import { Box, Text } from "ink";
import type { Job } from "../../core/database.js";
import { TaskItem, getStatusColor, getStatusIcon } from "./TaskItem.js";

interface TaskListProps {
  jobs: Job[];
  selectedIndex: number;
  scrollOffset: number;
  height: number;
  width: number;
  focused: boolean;
  totalJobCount?: number;
  filterLabel?: string;
  statusCounts?: Record<string, number>;
  archivedJobs?: Job[];
  archiveExpanded?: boolean;
  onToggleArchive?: () => void;
  nextFeedCheck?: number;
  nextDecapiCheck?: number;
  nextTwitchCheck?: number;
}

// Order in which status icons appear in the summary
const STATUS_DISPLAY_ORDER = ["Live", "Downloading", "Muxing", "Upcoming", "Error", "COOKIES?", "Cancelled", "Finished"];

function formatCountdown(epochMs: number): string {
  if (!epochMs) return "--";
  const remaining = Math.max(0, Math.floor((epochMs - Date.now()) / 1000));
  if (remaining <= 0) return "now";
  const minutes = Math.floor(remaining / 60);
  const seconds = remaining % 60;
  if (minutes > 0) return `${minutes}m ${seconds}s`;
  return `${seconds}s`;
}

type VirtualItem =
  | { type: "job"; job: Job; dimmed?: boolean }
  | { type: "divider"; count: number; expanded: boolean };

export function TaskList({
  jobs,
  selectedIndex,
  scrollOffset,
  height,
  width,
  focused,
  totalJobCount,
  filterLabel,
  statusCounts,
  archivedJobs = [],
  archiveExpanded = false,
  onToggleArchive,
  nextFeedCheck = 0,
  nextDecapiCheck = 0,
  nextTwitchCheck = 0,
}: TaskListProps): React.ReactElement {
  const contentHeight = height - 3; // borders (2) + title row (1)
  const borderColor = focused ? "cyan" : "gray";
  const titleColor = focused ? "cyan" : "white";

  // Build virtual list
  const virtualItems: VirtualItem[] = [];
  for (const job of jobs) {
    virtualItems.push({ type: "job", job });
  }
  if (archivedJobs.length > 0) {
    virtualItems.push({ type: "divider", count: archivedJobs.length, expanded: archiveExpanded });
    if (archiveExpanded) {
      for (const job of archivedJobs) {
        virtualItems.push({ type: "job", job, dimmed: true });
      }
    }
  }

  const visibleItems = virtualItems.slice(scrollOffset, scrollOffset + contentHeight);
  const totalItems = virtualItems.length;

  return (
    <Box
      flexDirection="column"
      height={height}
      width={width}
      borderStyle="round"
      borderColor={borderColor}
    >
      <Box paddingX={1}>
        <Text color={titleColor} bold>Tasks </Text>
        {statusCounts ? (
          <>
            <Text color={titleColor} bold>(</Text>
            {STATUS_DISPLAY_ORDER
              .filter((s) => (statusCounts[s] || 0) > 0)
              .map((status, i, arr) => (
                <React.Fragment key={status}>
                  <Text color={getStatusColor(status)}>{statusCounts[status]}{getStatusIcon(status)}</Text>
                  {i < arr.length - 1 && <Text color="gray"> </Text>}
                </React.Fragment>
              ))}
            <Text color={titleColor} bold>)</Text>
          </>
        ) : (
          <Text color={titleColor} bold>({jobs.length})</Text>
        )}
        {filterLabel && (
          <Text color="yellow"> [{filterLabel}]</Text>
        )}
        {(nextFeedCheck > 0 || nextDecapiCheck > 0 || nextTwitchCheck > 0) && (
          <Text color="gray"> F:{formatCountdown(nextFeedCheck)} D:{formatCountdown(nextDecapiCheck)} T:{formatCountdown(nextTwitchCheck)}</Text>
        )}
        {totalItems > contentHeight && (
          <Text color="gray">
            {" "}
            [{scrollOffset + 1}-{Math.min(scrollOffset + contentHeight, totalItems)}/{totalItems}]
          </Text>
        )}
      </Box>

      {jobs.length === 0 && archivedJobs.length === 0 ? (
        <Box paddingX={1} paddingY={1}>
          <Text color="gray">No tasks. Add videos via Web UI or CLI.</Text>
        </Box>
      ) : (
        <Box flexDirection="column" paddingX={1}>
          {visibleItems.map((item, idx) => {
            const globalIdx = scrollOffset + idx;
            if (item.type === "divider") {
              const icon = item.expanded ? "\u25BE" : "\u25B8";
              const isSelected = globalIdx === selectedIndex;
              return (
                <Box key="archive-divider" height={1} width={width - 4}>
                  <Text
                    backgroundColor={isSelected ? "blue" : undefined}
                    color={isSelected ? "white" : "gray"}
                    dimColor={!isSelected}
                  >
                    {isSelected ? "> " : "  "}{icon} Archived ({item.count})
                  </Text>
                </Box>
              );
            }
            return (
              <TaskItem
                key={item.job.id}
                job={item.job}
                selected={globalIdx === selectedIndex}
                width={width - 4}
                dimmed={item.dimmed}
              />
            );
          })}
        </Box>
      )}
    </Box>
  );
}

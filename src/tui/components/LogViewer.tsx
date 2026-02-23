import React from "react";
import { Box, Text } from "ink";
import { displayWidth, truncateToWidth } from "../textWidth.js";

interface LogViewerProps {
  logs: string[];
  scrollOffset: number;
  height: number;
  width: number;
  focused: boolean;
  autoScroll?: boolean;
  levelFilter?: string;
}

const LEVEL_PRIORITY: Record<string, number> = { ERROR: 0, WARN: 1, WARNING: 1, INFO: 2, DEBUG: 3 };

function filterLogsByLevel(logs: string[], filter: string): string[] {
  if (!filter || filter === "all") return logs;
  const threshold = LEVEL_PRIORITY[filter] ?? 3;
  return logs.filter((log) => {
    const match = log.match(/\[(DEBUG|INFO|WARN(?:ING)?|ERROR)\]/i);
    if (!match) return true;
    const level = match[1].toUpperCase();
    return (LEVEL_PRIORITY[level] ?? 2) <= threshold;
  });
}

export const LogViewer = React.memo(function LogViewer({
  logs,
  scrollOffset,
  height,
  width,
  focused,
  autoScroll = true,
  levelFilter = "all",
}: LogViewerProps): React.ReactElement {
  const filteredLogs = React.useMemo(() => filterLogsByLevel(logs, levelFilter), [logs, levelFilter]);
  const contentHeight = height - 3; // borders (2) + title row (1)
  const maxScroll = Math.max(0, filteredLogs.length - contentHeight);
  const actualOffset = Math.min(scrollOffset, maxScroll);
  const visibleLogs = filteredLogs.slice(actualOffset, actualOffset + contentHeight);

  const borderColor = focused ? "cyan" : "gray";
  const titleColor = focused ? "cyan" : "white";

  // Calculate scroll indicator
  const scrollPercent = filteredLogs.length > contentHeight
    ? Math.round((actualOffset / maxScroll) * 100)
    : 100;

  const filterSuffix = levelFilter !== "all" ? ` [${levelFilter}+]` : "";

  return (
    <Box
      flexDirection="column"
      height={height}
      width={width}
      borderStyle="round"
      borderColor={borderColor}
    >
      <Box paddingX={1}>
        <Text color={titleColor} bold>
          Logs ({filteredLogs.length})
        </Text>
        {filterSuffix && <Text color="yellow">{filterSuffix}</Text>}
        {filteredLogs.length > contentHeight && (
          <Text color="gray">
            {" "}
            [{scrollPercent}%]
          </Text>
        )}
        {!autoScroll && focused && (
          <Text color="yellow"> [PAUSED]</Text>
        )}
      </Box>

      <Box flexDirection="column" paddingX={1} overflow="hidden">
        {visibleLogs.length === 0 ? (
          <Text color="gray">No logs yet.</Text>
        ) : (
          visibleLogs.map((log, idx) => (
            <LogLine key={actualOffset + idx} log={log} width={width - 4} />
          ))
        )}
      </Box>
    </Box>
  );
});

interface LogLineProps {
  log: string;
  width: number;
}

const LogLine = React.memo(function LogLine({ log, width }: LogLineProps): React.ReactElement {
  // Parse log level from format: [LEVEL] message or timestamp [LEVEL] message
  const levelMatch = log.match(/\[(DEBUG|INFO|WARN(?:ING)?|ERROR)\]/i);
  const level = levelMatch ? levelMatch[1].toUpperCase() : "INFO";

  const color = getLogColor(level);

  // Truncate if too long (display-width aware for CJK characters)
  const displayLog = displayWidth(log) > width ? truncateToWidth(log, width) : log;

  return (
    <Box height={1} width={width}>
      <Text color={color} wrap="truncate">{displayLog}</Text>
    </Box>
  );
});

function getLogColor(level: string): string {
  switch (level) {
    case "DEBUG":
      return "gray";
    case "INFO":
      return "white";
    case "WARN":
    case "WARNING":
      return "yellow";
    case "ERROR":
      return "red";
    default:
      return "white";
  }
}

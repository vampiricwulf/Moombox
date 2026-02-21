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
}

export const LogViewer = React.memo(function LogViewer({
  logs,
  scrollOffset,
  height,
  width,
  focused,
  autoScroll = true,
}: LogViewerProps): React.ReactElement {
  const contentHeight = height - 3; // borders (2) + title row (1)
  const maxScroll = Math.max(0, logs.length - contentHeight);
  const actualOffset = Math.min(scrollOffset, maxScroll);
  const visibleLogs = logs.slice(actualOffset, actualOffset + contentHeight);

  const borderColor = focused ? "cyan" : "gray";
  const titleColor = focused ? "cyan" : "white";

  // Calculate scroll indicator
  const scrollPercent = logs.length > contentHeight
    ? Math.round((actualOffset / maxScroll) * 100)
    : 100;

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
          Logs ({logs.length})
        </Text>
        {logs.length > contentHeight && (
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

function LogLine({ log, width }: LogLineProps): React.ReactElement {
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
}

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

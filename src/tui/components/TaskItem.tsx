import React from "react";
import { Box, Text } from "ink";
import type { Job } from "../../core/database.js";
import { displayWidth, truncateToWidth, padEndToWidth } from "../textWidth.js";

interface TaskItemProps {
  job: Job;
  selected: boolean;
  width: number;
  dimmed?: boolean;
}

export const TaskItem = React.memo(function TaskItem({ job, selected, width, dimmed }: TaskItemProps): React.ReactElement {
  const statusColor = getStatusColor(job.status);
  const statusIcon = getStatusIcon(job.status);

  const isTwitch = job.platform === "twitch";
  const platformTag = isTwitch ? "[TW] " : "";
  const platformTagWidth = isTwitch ? 5 : 0;

  // Inline progress for active downloads (e.g. "45%")
  const showProgress = (job.status === "Downloading" || job.status === "Muxing") && job.percent > 0;
  const progressText = showProgress ? `${Math.round(job.percent)}% ` : "";
  const progressTextWidth = progressText.length;

  // Fixed widths
  const selectorWidth = 2;   // "> " or "  "
  const iconWidth = 2;       // icon + space

  // Calculate remaining width for title
  const titleWidth = Math.max(5, width - selectorWidth - iconWidth - platformTagWidth - progressTextWidth);

  // Truncate title if needed (display-width aware for CJK characters)
  const title = displayWidth(job.title) > titleWidth
    ? truncateToWidth(job.title, titleWidth)
    : padEndToWidth(job.title, titleWidth);

  return (
    <Box height={1} width={width}>
      <Text
        backgroundColor={selected ? "blue" : undefined}
        color={selected ? "white" : undefined}
        dimColor={dimmed && !selected}
      >
        {selected ? "> " : "  "}
      </Text>
      <Text color={statusColor} dimColor={dimmed && !selected}>{statusIcon} </Text>
      {progressText && (
        <Text color={statusColor} dimColor={dimmed && !selected}>{progressText}</Text>
      )}
      {isTwitch && (
        <Text color="magenta" dimColor={dimmed && !selected}>{platformTag}</Text>
      )}
      <Text
        backgroundColor={selected ? "blue" : undefined}
        color={selected ? "white" : statusColor}
        dimColor={dimmed && !selected}
      >
        {title}
      </Text>
    </Box>
  );
});

export function getStatusColor(status: string): string {
  switch (status) {
    case "Downloading":
    case "Live":
      return "green";
    case "Muxing":
      return "yellow";
    case "Finished":
      return "cyan";
    case "Error":
      return "red";
    case "Cancelled":
      return "gray";
    case "Upcoming":
      return "blue";
    case "COOKIES?":
      return "magenta";
    default:
      return "white";
  }
}

export function getStatusIcon(status: string): string {
  switch (status) {
    case "Downloading":
      return "\u25BC"; // Down arrow
    case "Live":
      return "\u25CF"; // Filled circle
    case "Muxing":
      return "\u2699"; // Gear
    case "Finished":
      return "\u2713"; // Check
    case "Error":
      return "\u2717"; // X
    case "Cancelled":
      return "\u25CB"; // Empty circle
    case "Upcoming":
      return "\u25CB"; // Empty circle
    case "COOKIES?":
      return "\u26A0"; // Warning
    default:
      return "\u25CB";
  }
}

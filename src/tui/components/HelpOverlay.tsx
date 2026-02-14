import React from "react";
import { Box, Text } from "ink";

interface HelpOverlayProps {
  width: number;
  height: number;
  scrollOffset: number;
}

interface HelpSection {
  title: string;
  bindings: [string, string][];
}

const HELP_SECTIONS: HelpSection[] = [
  {
    title: "Global",
    bindings: [
      ["Tab", "Cycle focus: Tasks > Details > Logs"],
      ["W", "Open web dashboard in browser"],
      ["?", "Toggle this help overlay"],
      ["Q / Ctrl+C", "Quit"],
    ],
  },
  {
    title: "Tasks Panel",
    bindings: [
      ["\u2191/\u2193", "Select previous/next task"],
      ["A", "Add video by URL or ID"],
      ["C", "Cancel selected job"],
      ["R", "Retry failed/cancelled job"],
      ["D", "Delete job (press twice to confirm)"],
      ["F", "Cycle status filter"],
      ["O", "Open output folder (finished jobs)"],
      ["Enter", "Expand/collapse archived jobs"],
    ],
  },
  {
    title: "Details Panel",
    bindings: [
      ["\u2191/\u2193", "Scroll details up/down"],
    ],
  },
  {
    title: "Logs Panel",
    bindings: [
      ["\u2191/\u2193", "Scroll logs up/down"],
      ["PgUp/PgDn", "Page up/down"],
      ["", "Auto-scroll pauses when you scroll up"],
      ["", "Resume by scrolling to bottom"],
    ],
  },
  {
    title: "Mouse",
    bindings: [
      ["Click", "Select task / focus panel"],
      ["Scroll", "Scroll focused panel"],
    ],
  },
];

export function HelpOverlay({ width, height, scrollOffset }: HelpOverlayProps): React.ReactElement {
  const contentHeight = height - 3; // borders (2) + title row (1)

  // Build all lines
  const lines: { type: "title" | "binding" | "blank"; text?: string; key?: string; desc?: string }[] = [];

  for (let i = 0; i < HELP_SECTIONS.length; i++) {
    const section = HELP_SECTIONS[i];
    if (i > 0) lines.push({ type: "blank" });
    lines.push({ type: "title", text: section.title });
    for (const [key, desc] of section.bindings) {
      lines.push({ type: "binding", key, desc });
    }
  }

  const maxScroll = Math.max(0, lines.length - contentHeight);
  const actualOffset = Math.min(scrollOffset, maxScroll);
  const visibleLines = lines.slice(actualOffset, actualOffset + contentHeight);

  const scrollPercent = lines.length > contentHeight
    ? Math.round((actualOffset / maxScroll) * 100)
    : 100;

  return (
    <Box
      flexDirection="column"
      width={width}
      height={height}
      borderStyle="round"
      borderColor="cyan"
    >
      <Box paddingX={1}>
        <Text color="cyan" bold>Help</Text>
        {lines.length > contentHeight && (
          <Text color="gray"> [{scrollPercent}%]</Text>
        )}
        <Text color="gray"> (press ? or Esc to close)</Text>
      </Box>
      <Box flexDirection="column" paddingX={1} overflow="hidden">
        {visibleLines.map((line, idx) => {
          if (line.type === "blank") {
            return <Box key={actualOffset + idx} height={1}><Text> </Text></Box>;
          }
          if (line.type === "title") {
            return (
              <Box key={actualOffset + idx} height={1}>
                <Text color="cyan" bold>{line.text}</Text>
              </Box>
            );
          }
          // binding
          return (
            <Box key={actualOffset + idx} height={1}>
              <Text color="yellow">{(line.key || "").padEnd(14)}</Text>
              <Text>{line.desc}</Text>
            </Box>
          );
        })}
      </Box>
    </Box>
  );
}

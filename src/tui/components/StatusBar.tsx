import React from "react";
import { Box, Text } from "ink";
import type { Job } from "../../core/database.js";
import { CookieJar } from "../../core/cookies.js";

interface StatusBarProps {
  focusedPanel: "tasks" | "details" | "logs";
  jobs: Job[];
  width: number;
}

export function StatusBar({
  focusedPanel,
  jobs,
  width,
}: StatusBarProps): React.ReactElement {
  let controls: string;
  switch (focusedPanel) {
    case "tasks":
      controls = "[\u2191\u2193] Select | [Tab] Switch | [A]dd [C]ancel [R]etry [D]el [F]ilter [O]pen [?] [Q]uit";
      break;
    case "details":
      controls = "[\u2191\u2193] Scroll | [Tab] Switch | [?] [Q]uit";
      break;
    case "logs":
      controls = "[\u2191\u2193] Navigate | [PgUp/Dn] Page | [Tab] Switch | [?] [Q]uit";
      break;
  }

  const hasCookies = CookieJar.hasAuthCookies();
  const cookiesRejected = jobs.some((j) => j.status === "COOKIES?");

  let cookieIcon: string;
  let cookieColor: string;
  let cookieLabel: string;
  if (!hasCookies) {
    cookieIcon = "\u25CB";
    cookieColor = "red";
    cookieLabel = "No Cookies";
  } else if (cookiesRejected) {
    cookieIcon = "\u2717";
    cookieColor = "red";
    cookieLabel = "Cookies";
  } else {
    cookieIcon = "\u25CF";
    cookieColor = "green";
    cookieLabel = "Cookies";
  }

  return (
    <Box width={width} justifyContent="space-between">
      <Text color="gray" dimColor>
        {controls}
      </Text>
      <Box>
        <Text color={cookieColor} dimColor>
          {cookieIcon} {cookieLabel}
        </Text>
      </Box>
    </Box>
  );
}

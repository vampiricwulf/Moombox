import React from "react";
import { Box, Text } from "ink";
import type { Job } from "../../core/database.js";
import { CookieJar } from "../../core/cookies.js";
import { AutoCookieService } from "../../core/autoCookies.js";

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
  const hasTwitchAuth = CookieJar.hasTwitchAuthCookies();
  const reloginRequired = AutoCookieService.getInstance().needsManualRelogin;

  // Warnings
  const warnings: string[] = [];
  if (reloginRequired.youtube) warnings.push("YT: Re-login");
  if (reloginRequired.twitch) warnings.push("TW: Re-login");

  // YT indicator color
  let ytColor: string;
  if (reloginRequired.youtube) ytColor = "red";
  else if (!hasCookies) ytColor = "yellow";
  else if (cookiesRejected) ytColor = "red";
  else ytColor = "green";

  // TW indicator color
  let twColor: string;
  if (reloginRequired.twitch) twColor = "red";
  else if (hasTwitchAuth) twColor = "green";
  else twColor = "gray";

  return (
    <Box width={width} justifyContent="space-between">
      <Text color="gray" dimColor>
        {controls}
      </Text>
      <Box>
        {warnings.length > 0 && (
          <Text color="red" dimColor>
            {warnings.join("  ")}{"  "}
          </Text>
        )}
        <Text color={ytColor} dimColor>
          YT
        </Text>
        <Text color="gray" dimColor>
          {" "}
        </Text>
        <Text color={twColor} dimColor>
          TW
        </Text>
      </Box>
    </Box>
  );
}

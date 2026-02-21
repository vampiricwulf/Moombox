import React from "react";
import { Box, Text } from "ink";
import type { Job } from "../../core/database.js";
import { CookieRefreshService } from "../../core/cookieRefresh.js";
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
  const compact = width < 100;
  switch (focusedPanel) {
    case "tasks":
      controls = compact
        ? "\u2191\u2193 Sel | Tab | A C R D F O ` ? Q"
        : "[\u2191\u2193] Select | [Tab] Switch | [A]dd [C]ancel [R]etry [D]el [F]ilter [O]pen [`]Settings [?] [Q]uit";
      break;
    case "details":
      controls = compact
        ? "\u2191\u2193 Scroll | Tab | ? Q"
        : "[\u2191\u2193] Scroll | [Tab] Switch | [?] [Q]uit";
      break;
    case "logs":
      controls = compact
        ? "\u2191\u2193 Nav | PgUp/Dn | L Filter | Tab | ? Q"
        : "[\u2191\u2193] Navigate | [PgUp/Dn] Page | [L] Filter | [Tab] Switch | [?] [Q]uit";
      break;
  }

  const validation = CookieRefreshService.getInstance().validationState;
  const cookiesRejected = jobs.some((j) => j.status === "COOKIES?");
  const reloginRequired = AutoCookieService.getInstance().needsManualRelogin;

  // Warnings
  const warnings: string[] = [];
  if (reloginRequired.youtube) warnings.push("YT: Re-login");
  if (reloginRequired.twitch) warnings.push("TW: Re-login");

  // YT indicator color — uses real auth validation, not just cookie presence
  let ytColor: string;
  if (reloginRequired.youtube) ytColor = "red";
  else if (!validation.youtube.hasCookies) ytColor = "yellow";
  else if (cookiesRejected || !validation.youtube.authenticated) ytColor = "red";
  else ytColor = "green";

  // TW indicator color — uses real auth validation
  let twColor: string;
  if (reloginRequired.twitch) twColor = "red";
  else if (validation.twitch.authenticated) twColor = "green";
  else if (validation.twitch.hasCookies) twColor = "red";
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

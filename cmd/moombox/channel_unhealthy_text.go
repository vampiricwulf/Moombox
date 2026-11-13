package main

import (
	"fmt"

	"github.com/vampiricwulf/Moombox/internal/monitor"
)

// channelUnhealthyText is the Warn message and the Channel Not Responding
// description for a channel no monitor reaches. A YouTube feed 404 has its own
// wording (owner ruling R2-M3): YouTube's feed answers 404 for a while when it
// is busy, for channels that exist, so "renamed, banned, or misconfigured"
// beside a Last Error that says "usually clears by itself" contradicted itself
// in the one message meant to stop alarming the operator. Every other cause
// reads as it always has. Wording only: when the alert is sent is unchanged.
func channelUnhealthyText(platform string, consecutive int, lastErr string) (warn, description string) {
	if monitor.IsFeedBusy404(lastErr) {
		return "channel's feed keeps answering 404 and no other monitor reaches it; common when YouTube is busy and usually clears by itself, check the channel ID if it lasts",
			fmt.Sprintf("A %s channel's feed has answered 404 for %d checks in a row and no other monitor reaches the channel. YouTube's feed often does this for a while when it is busy and it usually clears by itself; the channel is still checked. If it lasts, check the channel ID: the channel may have been renamed or removed.", platform, consecutive)
	}
	return "channel failing monitor checks — verify it still exists",
		fmt.Sprintf("A %s channel has failed %d consecutive monitor checks — it may be renamed, banned, or misconfigured, and its streams are being missed", platform, consecutive)
}

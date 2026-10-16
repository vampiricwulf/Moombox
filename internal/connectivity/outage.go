package connectivity

import "time"

// MinOutage is the shortest offline stretch Moombox treats as an outage
// rather than a blip, for the two things a transition used to set off on its
// own: the Outage Alert (cmd/moombox/outage_alerts.go), which an outage
// shorter than this does not send, and the restart of a live Twitch capture
// (worker.ExecuteTwitch), which an outage shorter than this does not cancel.
//
// The monitor itself is deliberately quicker than that — it goes offline on
// its second failed poll and back online on the first good one, because the
// subsystems that gate on it must stop spending requests on a dead link at
// once — so a flaky link that drops two polls in three cycles through it
// every ~15 s. A minute is past every flap the monitor's own debounce lets
// through and well inside the outage an operator needs to hear about: the
// engines ride a shorter one out in place, and the monitors re-check on every
// restore whatever its length.
const MinOutage = 60 * time.Second

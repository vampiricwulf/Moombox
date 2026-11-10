// The browser cookie-setup dialog (#auto-cookie-setup-dialog), shared by the
// first-run wizard (setup.js) and Settings > Cookies (settings.js) — K33.
//
// It used to be driven by two controllers in turn, each cloning the Done and
// Cancel buttons to drop the other's handlers, with near-identical finish and
// cancel bodies that needed every fix twice, and the platform recovered from
// the dialog's label: after a reload during a Twitch setup the dialog reopens
// with the default label "Cookie Login", and the finish was read as
// YouTube's. One session object now owns the dialog:
//
//   - its handlers (Done, Cancel, sl-request-close, pagehide) are bound ONCE,
//     so nothing is cloned and nothing stacks;
//   - the platform is stored, never parsed — the label is derived from it,
//     and a setup adopted after a reload takes it from the server
//     (GET /api/cookies/auto-status's setupPlatform);
//   - one phase field says where it stands (PHASE).
//
// Each controller keeps only its own side effects, handed in as an owner:
// the wizard's badges and its abort-report variant, Settings' status reloads
// and the remote-viewer guard on its start.
import {
  cookieSetupAbortReport,
  cookieSetupAcceptedToast,
  cookieSetupProbe,
  cookieSetupRejectedMessage,
  serverErrorMessage,
} from "./utils.js";

/**
 * Where the session stands.
 *  - idle: no setup this page knows of; the dialog is hidden.
 *  - active: the server holds the setup slot this page started; the dialog
 *    waits for "I'm Logged In".
 *  - finishing: the finish POST is out (Done loading, a 60 s cap running).
 *  - aborted: the finish was cut off at 60 s and the server has not
 *    confirmed the slot free; Try Again and Skip are shown.
 *  - rejected: the finish ended without a sign-in the dialog can report as
 *    accepted (no platform authenticated, an error, or an abort the server
 *    confirmed is over); the slot is free and the dialog stays open on the
 *    message.
 *  - adopted: the dialog was reopened for a setup the server holds that no
 *    open page started here (a reload mid-setup, another tab's).
 */
export const PHASE = Object.freeze({
  IDLE: "idle",
  ACTIVE: "active",
  FINISHING: "finishing",
  ABORTED: "aborted",
  REJECTED: "rejected",
  ADOPTED: "adopted",
});

// The phases in which a setup this page STARTED still holds the server's
// slot, so leaving unannounced would wedge acquisition: the pagehide beacon
// fires in these and no other. An abort keeps the slot until the server
// confirms it free.
const HOLDS_SLOT = new Set([PHASE.ACTIVE, PHASE.FINISHING, PHASE.ABORTED]);

const platformName = (p) => (p === "twitch" ? "Twitch" : "YouTube");

export class CookieSetupSession {
  /** @param {object} app — the MoomboxApp, for its toasts and escapeHtml. */
  constructor(app) {
    this.app = app;
    this.phase = PHASE.IDLE;
    // "youtube" | "twitch" | null — null only for an adopted setup whose
    // platform the server did not name.
    this.platform = null;
    // The controller whose setup this is: { wizard, retry(platform),
    // onAccepted(data, ytOk, twOk), afterAbort() }. See begin().
    this.owner = null;
    // Whether the setup was started by this page (begin) rather than adopted
    // (adopt). Not a phase: an adopted setup goes through finishing too, but
    // its slot was never this page's to release — only a successful
    // /auto-setup/start, which is loopback-gated, makes a page one with a
    // setup to abandon (routes/cookies.go, the /abandon gate).
    this._startedHere = false;
    this._boundDialog = null;
  }

  /** True while this page holds the server's setup slot. */
  holdsSlot() {
    return this._startedHere && HOLDS_SLOT.has(this.phase);
  }

  /**
   * Bind the dialog's handlers, once per dialog element. Both controllers
   * call it from their setupListeners; begin() and adopt() call it too.
   */
  bind() {
    const dialog = document.getElementById("auto-cookie-setup-dialog");
    if (!dialog || this._boundDialog === dialog) return;
    this._boundDialog = dialog;

    document.getElementById("btn-auto-cookie-done")?.addEventListener("click", () => this.finish());
    document.getElementById("btn-auto-cookie-cancel")?.addEventListener("click", () => this.cancel());

    // Escape and the overlay click would otherwise close the dialog without
    // telling the server, which keeps holding the setup slot (and, from the
    // wizard, the browser). Only the dialog's own request (children bubble
    // theirs).
    dialog.addEventListener("sl-request-close", (e) => {
      if (e.target !== dialog) return;
      e.preventDefault();
      this.cancel();
    });

    // Tell the server when the tab goes away mid-setup. Nothing did, so
    // closing the tab left a browser registered server-side and every later
    // setup and periodic refresh refused ("setup in progress") until something
    // released the slot. On Windows the server-side reap does that on its own,
    // keyed on the browser actually being gone; where there is no Job Object
    // to ask — Linux, Docker — this beacon is the only release there is.
    //
    // IT POSTS /abandon, NOT /cancel, AND THE DIFFERENCE IS A BROWSER WINDOW.
    // Cancel is the user's abort and closes the setup browser. But this flow's
    // own instructions send the user AWAY from this tab — "a browser window has
    // opened… please sign in" — so closing the now-idle dashboard tab is an
    // entirely natural thing to do MID-LOGIN, and pointing this beacon at
    // /cancel made that a remote kill of the window they were typing their
    // password into. /abandon releases the slot without touching the browser,
    // and declines even to do that where releasing would itself close a Job
    // Object. A click is consent; a tab unload is not.
    //
    // pagehide, NOT beforeunload: Settings' beforeunload can put a "Leave
    // site?" confirm in front of the user, and a beacon fired from there
    // cancels the setup even when they choose to stay. pagehide only fires once
    // the page really is going. sendBeacon survives the unload where fetch does
    // not; its POST carries an Origin, so it passes CSRF.
    //
    // e.persisted is the other half of picking pagehide. It also fires when the
    // page goes into the back/forward cache — a back navigation, or a phone
    // backgrounding the tab — and that page is coming BACK. Releasing there
    // tears down a live setup and, because the phase is cleared on the way
    // out, leaves the restored page unable to cancel it. Skipping the bfcache
    // case needs no pageshow re-arm precisely because the phase is left alone.
    //
    // visibilitychange must NOT be added here: the one thing this flow asks of
    // the user is to switch to the browser window Moombox just opened, which
    // hides this tab.
    window.addEventListener("pagehide", (e) => {
      if (e.persisted) return;
      if (!this.holdsSlot()) return;
      this.phase = PHASE.IDLE;
      navigator.sendBeacon("/api/cookies/auto-setup/abandon");
    });
  }

  /**
   * A setup the owner just started (its POST /start answered success): the
   * server holds the slot. Shows the dialog for platform.
   * @param {"youtube"|"twitch"} platform
   * @param {object} owner — { wizard: bool, retry(platform),
   *   onAccepted(data, ytOk, twOk), afterAbort() }
   */
  begin(platform, owner) {
    this.bind();
    this.platform = platform === "twitch" ? "twitch" : "youtube";
    this.owner = owner;
    this._startedHere = true;
    this.phase = PHASE.ACTIVE;
    this._clearMessages();
    const instructions = document.getElementById("auto-cookie-instructions");
    if (instructions) {
      instructions.textContent = this.platform === "twitch"
        ? "A browser window has opened to Twitch. Please sign in, then click \"I'm Logged In\"."
        : "A browser window has opened to Google. Please sign in to YouTube, then click \"I'm Logged In\".";
    }
    const dialog = document.getElementById("auto-cookie-setup-dialog");
    if (dialog) {
      dialog.label = `Setup: ${platformName(this.platform)}`;
      dialog.show();
    }
  }

  /**
   * A status load found a setup in progress on the server. If this page has
   * none of its own, it adopts it — the platform from the server's
   * setupPlatform — and shows the dialog so it can be finished or cancelled.
   * @param {object} status — GET /api/cookies/auto-status's answer
   * @param {object} owner — as for begin()
   */
  adopt(status, owner) {
    if (!status?.setupInProgress) return;
    this.bind();
    const dialog = document.getElementById("auto-cookie-setup-dialog");
    if (this.phase !== PHASE.IDLE) {
      // Ours already: just make sure it is on screen.
      if (dialog && !dialog.open) dialog.show();
      return;
    }
    const p = status.setupPlatform;
    this.platform = p === "twitch" || p === "youtube" ? p : null;
    this.owner = owner;
    this._startedHere = false;
    this.phase = PHASE.ADOPTED;
    if (dialog) {
      dialog.label = this.platform ? `Setup: ${platformName(this.platform)}` : "Cookie Login";
      if (!dialog.open) dialog.show();
    }
  }

  /** "I'm Logged In": extract, verify and report. */
  async finish() {
    if (this.phase === PHASE.IDLE || this.phase === PHASE.FINISHING) return;
    const owner = this.owner || {};
    const platform = this.platform;
    const resultEl = document.getElementById("auto-cookie-setup-result");
    const doneBtn = document.getElementById("btn-auto-cookie-done");
    const countdownEl = document.getElementById("auto-cookie-countdown");
    const timeoutResultEl = document.getElementById("auto-cookie-result");
    this.phase = PHASE.FINISHING;
    if (doneBtn) { doneBtn.loading = true; doneBtn.disabled = true; }
    if (resultEl) {
      resultEl.textContent = "Extracting cookies...";
      resultEl.style.color = "";
    }
    if (timeoutResultEl) timeoutResultEl.innerHTML = "";

    let remaining = 60;
    if (countdownEl) {
      countdownEl.textContent = `${remaining}s remaining`;
      countdownEl.style.color = "";
    }
    const countdownInterval = setInterval(() => {
      remaining--;
      if (countdownEl) {
        countdownEl.textContent = `${remaining}s remaining`;
        if (remaining <= 10) {
          countdownEl.style.color = "var(--text-warning)";
        }
      }
      if (remaining <= 0) clearInterval(countdownInterval);
    }, 1000);

    const controller = new AbortController();
    const timeoutId = setTimeout(() => controller.abort(), 60000); // 60s timeout

    // Baseline for the abort path, dispatched BEFORE the finish so the
    // lastRefresh comparison there is server clock against server clock and no
    // browser/server skew can turn an old stamp into a fresh one.
    //
    // Deliberately not awaited: it must not add a round trip to the finish, and
    // only the abort branch ever reads it. cookieSetupProbe never rejects, so
    // an unread promise cannot surface as an unhandled rejection.
    const abortBaseline = cookieSetupProbe();

    try {
      const response = await fetch("/api/cookies/auto-setup/finish", { method: "POST", signal: controller.signal });
      // Any ANSWER means FinishSetup reached a conclusion and released the
      // slot, success or not. Only the abort path below skips this, and it
      // must — an abort says nothing about whether the setup is still live,
      // which is exactly when Skip and the unload beacon still have something
      // to tell the server. That path releases the phase itself, but only once
      // the server has confirmed the slot is free.
      this.phase = PHASE.REJECTED;
      // The finish handler's discriminated errors — 422 for an unreadable
      // cookies.txt or a broken profile, 409 for a locked cookie DB — used to
      // reach the user as `HTTP <n>`, which points at none of them.
      if (!response.ok) throw new Error(await serverErrorMessage(response));
      const data = await response.json();
      const ytOk = data.authenticated;
      const twOk = data.twitchAuthenticated;

      if (ytOk || twOk) {
        this.phase = PHASE.IDLE;
        if (resultEl) resultEl.textContent = "";
        document.getElementById("auto-cookie-setup-dialog")?.hide();
        owner.onAccepted?.(data, ytOk, twOk);
        // Accepted is not verified — see cookieSetupAcceptedToast. A sign-in
        // Moombox could not reach the site to confirm was reported as an
        // unqualified success, so a user whose network blipped mid-check was
        // told their cookies were configured with nothing having checked them.
        if (ytOk) {
          const toast = cookieSetupAcceptedToast("YouTube", data.youtubeVerification);
          this.app.showToast(toast.message, toast.variant);
        }
        if (twOk) {
          const toast = cookieSetupAcceptedToast("Twitch", data.twitchVerification);
          this.app.showToast(toast.message, toast.variant);
        }
        owner.afterAccepted?.();
      } else if (resultEl) {
        // Speaks from the verification fields of the platform this setup
        // signed in to — stored, never read off the label. An adopted setup
        // whose platform the server did not name reports whichever field
        // says more, YouTube's first.
        const verification = platform === "twitch" ? data.twitchVerification
          : platform === "youtube" ? data.youtubeVerification
            : (data.youtubeVerification ?? data.twitchVerification);
        resultEl.textContent = cookieSetupRejectedMessage(verification);
        resultEl.style.color = "var(--text-danger)";
      }
    } catch (e) {
      if (e.name === "AbortError") {
        this.phase = PHASE.ABORTED;
        if (resultEl) resultEl.textContent = "";
        // Report what actually happened rather than asserting a timeout. The
        // server writes the merged cookies.txt and reloads the jar BEFORE it
        // verifies, so this abort can fire over work that already committed.
        // The 60 s cap stays as it is — the server-side setup grace window is
        // priced against that exact number.
        const probe = await cookieSetupProbe();
        // Only a definite "no setup here" releases the slot. An unanswered
        // probe keeps it held, which is the safe direction: Skip and the
        // unload beacon then still have something to tell the server about.
        if (probe.ok && !probe.inProgress && this.phase === PHASE.ABORTED) this.phase = PHASE.REJECTED;
        if (timeoutResultEl) {
          // The wizard cannot light its badges from this path, and those are
          // the sole source of the active_platforms it is about to write, so
          // its report says so rather than letting "saved" imply a platform
          // was recorded.
          const report = cookieSetupAbortReport(probe, await abortBaseline, { wizard: owner.wizard === true });
          timeoutResultEl.innerHTML = `
            <sl-alert variant="${report.variant}" open>
              <sl-icon slot="icon" name="${report.icon}"></sl-icon>
              ${this.app.escapeHtml(report.message)}
            </sl-alert>
            <div style="display: flex; gap: 0.5em; margin-top: 0.75em;">
              <sl-button variant="primary" size="small" id="cookie-retry-btn">Try Again</sl-button>
              <sl-button variant="default" size="small" id="cookie-skip-btn">Skip</sl-button>
            </div>`;
          document.getElementById("cookie-retry-btn")?.addEventListener("click", () => {
            timeoutResultEl.innerHTML = "";
            if (countdownEl) countdownEl.textContent = "";
            owner.retry?.(platform || "youtube");
          });
          // Skip goes through the cancel path. It used to hide() the dialog
          // and nothing else, which does not fire sl-request-close, so the
          // server was never told — the setup it is still holding then blocked
          // every later setup and refresh. cancel() hides the dialog and clears
          // these same elements itself.
          document.getElementById("cookie-skip-btn")?.addEventListener("click", () => {
            this.cancel();
          });
        }
        // The finish may well have committed while we stopped waiting, so the
        // dashboard's cookie status is potentially stale.
        this.app.loadStatus();
        owner.afterAbort?.();
        return;
      }
      if (resultEl) {
        resultEl.textContent = "Error: " + e.message;
        resultEl.style.color = "var(--text-danger)";
      }
    } finally {
      clearInterval(countdownInterval);
      clearTimeout(timeoutId);
      if (countdownEl) countdownEl.textContent = "";
      if (doneBtn) { doneBtn.loading = false; doneBtn.disabled = false; }
    }
  }

  /** Cancel (the button, Escape, Skip): the user's abort, closing the setup browser. */
  async cancel() {
    this.phase = PHASE.IDLE;
    this.platform = null;
    try {
      await fetch("/api/cookies/auto-setup/cancel", { method: "POST" });
    } catch {
      // Ignore
    }
    document.getElementById("auto-cookie-setup-dialog")?.hide();
    this._clearMessages();
  }

  _clearMessages() {
    const resultEl = document.getElementById("auto-cookie-setup-result");
    if (resultEl) {
      resultEl.textContent = "";
      resultEl.style.color = "";
    }
    const countdownEl = document.getElementById("auto-cookie-countdown");
    if (countdownEl) countdownEl.textContent = "";
    const timeoutResultEl = document.getElementById("auto-cookie-result");
    if (timeoutResultEl) timeoutResultEl.innerHTML = "";
  }
}

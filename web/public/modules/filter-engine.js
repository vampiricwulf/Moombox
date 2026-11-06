/**
 * Filter engine — evaluates parsed tokens against job objects.
 * All tokens AND (intersect). OR tokens are unions within.
 */

/**
 * The job ID prefix of a Twitch clip job, the clip's slug after it
 * ("tw_clip_FaintLightGullWholeWheat") — internal/twitch ClipJobIDPrefix,
 * which TestTypeClipMatchesTheDashboard (internal/jobfilter) holds this
 * copy to. A clip is told from any other job by its ID alone (owner decision
 * FE-28): its platform is "twitch" like a VOD's or a live capture's, and no
 * column says "clip".
 */
export const CLIP_JOB_ID_PREFIX = "tw_clip_";

/**
 * Whether a job is a Twitch clip job (CLIP_JOB_ID_PREFIX): the job list's
 * CLIP badge and the filter language's type:clip both ask it.
 * @param {{id?: string}} job
 * @returns {boolean}
 */
export function isClipJob(job) {
  return typeof job?.id === "string" && job.id.startsWith(CLIP_JOB_ID_PREFIX);
}

const STATUS_FILTER_MAP = {
  active: ["Downloading", "Live", "Upcoming", "Muxing", "Queued"],
  // "Issues" is everything waiting on a human — failed, cancelled, or parked
  // on credentials — the bucket the TUI's F filter calls Issues too.
  issues: ["Error", "Cancelled", "COOKIES?"],
  finished: ["Finished"],
};
// status:errors was the bucket's name before Cancelled joined it; keep
// hand-typed queries working.
STATUS_FILTER_MAP.errors = STATUS_FILTER_MAP.issues;

/**
 * Test whether a single term matches a job.
 * @param {{ type: string, value: string, negate: boolean }} term
 * @param {object} job
 * @returns {boolean}
 */
function matchTerm(term, job) {
  let result;
  switch (term.type) {
    case "text": {
      // A free-text term is a case-insensitive substring of the title, the
      // channel name or the video ID — the same three fields the TUI's
      // internal/jobfilter matches, so one query reads alike in both UIs.
      const val = term.value.toLowerCase();
      result = (job.title || "").toLowerCase().includes(val) ||
               (job.channelName || "").toLowerCase().includes(val) ||
               (job.videoId || "").toLowerCase().includes(val);
      break;
    }
    case "status": {
      // Case-insensitive like every other term type (the dropdown inserts
      // lowercase, but hand-typed status:Active must work too). Unknown
      // group keys fall back to a direct status-name comparison so real
      // statuses (status:live, status:downloading) match instead of
      // silently emptying the list.
      //
      // Own properties only: the key is user text, and a plain `[key]`
      // lookup would hand status:constructor / status:__proto__ an
      // Object.prototype member, whose `.includes` is not a function — and
      // the filter bar stores the token before it renders, so every later
      // render would throw too and the list would freeze.
      const key = term.value.toLowerCase();
      const allowed = Object.hasOwn(STATUS_FILTER_MAP, key) ? STATUS_FILTER_MAP[key] : undefined;
      if (allowed) {
        result = allowed.includes(job.status);
      } else {
        result = (job.status || "").toLowerCase() === key;
      }
      break;
    }
    case "channel":
      result = (job.channelName || "").toLowerCase() === term.value.toLowerCase();
      break;
    case "platform":
      result = (job.platform || "").toLowerCase() === term.value.toLowerCase();
      break;
    case "type":
      // A job's kind where no column holds it (owner decision FE-28): clip,
      // a Twitch clip job by its ID (isClipJob). The only kind there is; any
      // other value matches nothing, so a typo empties the list rather than
      // passing everything.
      result = term.value.toLowerCase() === "clip" && isClipJob(job);
      break;
    default:
      result = true;
  }
  return term.negate ? !result : result;
}

/**
 * Test whether a token (possibly an OR group) matches a job.
 * @param {object} token
 * @param {object} job
 * @returns {boolean}
 */
function matchToken(token, job) {
  if (token.type === "or") {
    return token.terms.some(t => matchTerm(t, job));
  }
  return matchTerm(token, job);
}

/**
 * Filter a list of jobs using parsed tokens. All tokens must match (AND).
 * @param {object[]} jobs
 * @param {Array} tokens - parsed token array from parseFilterQuery
 * @returns {object[]}
 */
export function applyFilterTokens(jobs, tokens) {
  if (!tokens || tokens.length === 0) return jobs;
  return jobs.filter(job => tokens.every(token => matchToken(token, job)));
}

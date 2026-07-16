/**
 * Filter engine — evaluates parsed tokens against job objects.
 * All tokens AND (intersect). OR tokens are unions within.
 */

const STATUS_FILTER_MAP = {
  active: ["Downloading", "Live", "Upcoming", "Muxing", "Queued"],
  errors: ["Error", "COOKIES?"],
  finished: ["Finished", "Cancelled"],
};

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
      const val = term.value.toLowerCase();
      result = (job.title || "").toLowerCase().includes(val) ||
               (job.channelName || "").toLowerCase().includes(val);
      break;
    }
    case "status": {
      // Case-insensitive like every other term type (the dropdown inserts
      // lowercase, but hand-typed status:Active must work too). Unknown
      // group keys fall back to a direct status-name comparison so real
      // statuses (status:live, status:downloading) match instead of
      // silently emptying the list.
      const key = term.value.toLowerCase();
      const allowed = STATUS_FILTER_MAP[key];
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

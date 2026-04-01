/**
 * Filter engine — evaluates parsed tokens against job objects.
 * All tokens AND (intersect). OR tokens are unions within.
 */

const STATUS_FILTER_MAP = {
  active: ["Downloading", "Live", "Upcoming", "Muxing"],
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
      const allowed = STATUS_FILTER_MAP[term.value];
      result = allowed ? allowed.includes(job.status) : false;
      break;
    }
    case "channel":
      result = (job.channelName || "").toLowerCase() === term.value.toLowerCase();
      break;
    case "platform":
      result = (job.platform || "") === term.value;
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

// n_binding_template.js — Goja-runnable n-param solver binding template.
// Loaded by extractor_full_player.go via go:embed and filled via fmt.Sprintf.
//
// Placeholder map (do NOT add other percent-sign tokens to this file):
//   first  fill  = URL class reference (e.g., g.sB)
//   second fill  = array literal of candidate solver functions
//
// Strategy:
//   1. URL class (newer players): construct a URL with the class, set n via
//      query string, parse the result back. Returns the function bound to
//      a 200ms-EOF window of the player code so we don't need a full DOM.
//   2. Validated array candidates (older players): tests each candidate with
//      a dummy value and only accepts candidates that produce a different
//      string. Falls back to null (passthrough) if neither strategy works.

// _videoplaybackBase is a fake but well-formed videoplayback URL used to drive
// YouTube's URL class through its n-param transform. The host shape mirrors a
// real CDN edge — `rr<N>---sn-<XXX>.googlevideo.com` — so the URL parser
// accepts it; the actual host doesn't matter because we never make a network
// request, we just round-trip the URL through the class to harvest the
// transformed `n` value.
var _videoplaybackBase = "https://rr1---sn-a.googlevideo.com/videoplayback?n=";

_result.n = (function() {
  try {
    var _urlClass = %s;
    if (_urlClass) {
      var testN = "AAAAAA_TESTN_VAL";
      var u = new _urlClass(_videoplaybackBase + testN, true);
      var s = u.get ? u.get("n") : ((typeof u.A_ === "function") ? u.A_() : "" + u);
      if (u.get) {
        if (typeof s === "string" && s.length > 0 && s !== testN) {
          return function(input) {
            var url = new _urlClass(_videoplaybackBase + input, true);
            return url.get("n") || input;
          };
        }
      } else if (typeof s === "string") {
        var m = s.match(/[?&]n=([^&]+)/);
        if (m && m[1] && m[1] !== testN) {
          return function(input) {
            var url = new _urlClass(_videoplaybackBase + input, true);
            var result = (typeof url.A_ === "function") ? url.A_() : "" + url;
            var match = result.match(/[?&]n=([^&]+)/);
            return (match && match[1]) ? match[1] : input;
          };
        }
      }
    }
  } catch(e) {}
  var _cands = %s;
  for (var _i = 0; _i < _cands.length; _i++) {
    try {
      var _test = _cands[_i]("AAAA_VALIDATE");
      if (typeof _test === "string" && _test.length > 0 && _test !== "AAAA_VALIDATE") {
        return _cands[_i];
      }
    } catch(e) {}
  }
  return null;
})();

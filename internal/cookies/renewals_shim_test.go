package cookies

import "net/http"

// processYouTubeSetCookies is how the Set-Cookie rule tests in this package
// hand a reply's headers to the renewal store without a server: the same
// classification and the same keep that CookieJar.OfferYouTubeReply runs once
// it has placed a reply, then the flush, so the test can read cookies.txt.
// The second argument is unused: the store reads the session off the file
// and the jar.
func (rs *RefreshService) processYouTubeSetCookies(resp *http.Response, _ string) {
	j := rs.jar
	j.renew.mu.Lock()
	updates, cleared := classifyYouTubeSetCookies(resp.Header.Values("Set-Cookie"))
	j.warnClearedLocked(cleared, YouTubeReply{Endpoint: guideReplyEndpoint, Client: guideReplyClient})
	j.acceptRenewalsLocked(updates, guideReplyEndpoint, guideReplyClient)
	j.renew.mu.Unlock()
	j.FlushRenewals()
}

// writeAdmittedSetCookies applies a reply's Set-Cookie headers straight to
// cookies.txt through admitSetCookie and updateCookieFile, deletions and all,
// then reloads the jar. It is what the guide reply's handler did before the
// renewal store existed. The store never applies a deletion and keeps tracked
// names only (TestRenewalStoreNeverAppliesADeletion), so the tests that pin
// the WRITER's deletion and platform rules reach the writer this way.
func (rs *RefreshService) writeAdmittedSetCookies(resp *http.Response, sentAs string) {
	updates := make(map[cookieUpdateKey]cookieUpdate)
	for _, sc := range resp.Header.Values("Set-Cookie") {
		if key, cu, ok := admitSetCookie(sc, originYouTube); ok {
			updates[key] = cu
		}
	}
	if len(updates) == 0 {
		return
	}
	if err := rs.updateCookieFile(updates, originYouTube, sentAs); err != nil {
		return
	}
	_ = rs.jar.Reload()
}

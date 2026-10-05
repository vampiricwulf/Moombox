package cookies

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestARotationIsWrittenOnlyIntoItsOwnSession: the background re-check sends
// the guide request with the jar's session and writes the reply's rotated
// cookies into cookies.txt, which it re-reads at write time. An import that
// replaced the file while the request was in flight then received the OLD
// session's rotated __Secure-1PSIDTS on top of the new session's rows — a
// mixed file, right after the operator was told the import worked. The
// rotation is now written only into a file still holding the session it is
// for; without an import in between it lands as before.
//
// The same holds for a session with no LOGIN_INFO: the guard used to key on
// YouTubeIdentity, which is "" without it, and an empty key switched the
// check off.
//
// Mutants: skip the session check in updateCookieFile — the imported file
// carries sidts-rotated-for-A; key it on YouTubeIdentity — the same, for the
// sessions without LOGIN_INFO.
func TestARotationIsWrittenOnlyIntoItsOwnSession(t *testing.T) {
	session := func(name string, loginInfo bool) string {
		f := "# Netscape HTTP Cookie File\n" +
			".youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\tsapisid-" + name + "\n"
		if loginInfo {
			f += ".youtube.com\tTRUE\t/\tTRUE\t0\tLOGIN_INFO\tlogin-" + name + "\n"
		}
		return f
	}
	sidts := func(v string) string { return ".youtube.com\tTRUE\t/\tTRUE\t0\t__Secure-1PSIDTS\t" + v + "\n" }

	for _, tc := range []struct {
		name       string
		loginInfo  bool
		importB    bool
		wantInFile []string
		notInFile  []string
	}{
		{"an import lands mid-request", true, true, []string{"sapisid-B", "sidts-B"}, []string{"sidts-rotated-for-A"}},
		{"nothing changes mid-request", true, false, []string{"sapisid-A", "sidts-rotated-for-A"}, []string{"sidts-A-old"}},
		{"no LOGIN_INFO, an import lands mid-request", false, true, []string{"sapisid-B", "sidts-B"}, []string{"sidts-rotated-for-A"}},
		{"no LOGIN_INFO, nothing changes mid-request", false, false, []string{"sapisid-A", "sidts-rotated-for-A"}, []string{"sidts-A-old"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seedA := session("A", tc.loginInfo) + sidts("sidts-A-old")
			pasteB := session("B", tc.loginInfo) + sidts("sidts-B")
			s, path, jar := importService(t, seedA, true)
			rs := NewRefreshService(jar, 0, nopLogger{})

			arrived := make(chan struct{}, 1)
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				arrived <- struct{}{}
				<-release
				w.Header().Add("Set-Cookie", "__Secure-1PSIDTS=sidts-rotated-for-A; Domain=.youtube.com; Path=/; Max-Age=31536000")
				_, _ = w.Write([]byte(loggedInGuideBody))
			}))
			defer srv.Close()
			pointYouTubeGuideAt(t, srv)

			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _ = rs.checkAndRefreshYouTube(context.Background())
			}()
			<-arrived
			if tc.importB {
				if _, err := s.ImportCookies(context.Background(), pasteB); err != nil {
					t.Fatalf("ImportCookies: %v", err)
				}
			}
			close(release)
			<-done

			onDisk, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.wantInFile {
				if !strings.Contains(string(onDisk), want) {
					t.Errorf("cookies.txt is missing %s", want)
				}
			}
			for _, unwanted := range tc.notInFile {
				if strings.Contains(string(onDisk), unwanted) {
					t.Errorf("cookies.txt holds %s", unwanted)
				}
			}
		})
	}
}

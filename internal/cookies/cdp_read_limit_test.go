package cookies

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

// startLargeAnswerCDP is a fake browser debugging endpoint whose cookie
// answers are as large as the caller makes them: Network.getCookies answers
// with scoped, every other query with allCookies.
func startLargeAnswerCDP(t *testing.T, allCookies, scoped []map[string]any) int {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	wsBase := "ws://" + u.Host
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"webSocketDebuggerUrl":%q}`, wsBase+"/devtools/browser/x")
	})
	mux.HandleFunc("/json", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `[{"type":"page","webSocketDebuggerUrl":%q}]`, wsBase+"/devtools/page/x")
	})
	mux.HandleFunc("/devtools/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		conn.SetReadLimit(1 << 20)
		for {
			_, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var msg struct {
				ID     int    `json:"id"`
				Method string `json:"method"`
			}
			_ = json.Unmarshal(data, &msg)
			set := allCookies
			if msg.Method == "Network.getCookies" {
				set = scoped
			}
			body, _ := json.Marshal(map[string]any{"id": msg.ID, "result": map[string]any{"cookies": set}})
			if err := conn.Write(r.Context(), websocket.MessageText, body); err != nil {
				return
			}
		}
	})
	return port
}

func largeAnswerCookie(name, value, domain string) map[string]any {
	return map[string]any{"name": name, "value": value, "domain": domain, "path": "/",
		"expires": 1893456000.5, "size": len(name) + len(value), "httpOnly": true, "secure": true,
		"session": false, "sameSite": "None", "priority": "High", "sameParty": false,
		"sourceScheme": "Secure", "sourcePort": 443}
}

// TestCDPCookieReadsTakeLargeAnswers: coder/websocket's default read limit is
// 32 KiB, and the CDP cookie answers pass it on an ordinary profile — every
// tier failed with "message too big" although the browser had answered, and
// the browser read reported failure.
//
// Mutant: drop the SetReadLimit in cdpSendCommandWithResult — the read fails.
func TestCDPCookieReadsTakeLargeAnswers(t *testing.T) {
	relevant := []map[string]any{
		largeAnswerCookie("SAPISID", "sapisid-"+strings.Repeat("a", 30), ".youtube.com"),
		largeAnswerCookie("LOGIN_INFO", "login-"+strings.Repeat("b", 260), ".youtube.com"),
	}
	pad := func(n int, domain string) []map[string]any {
		var out []map[string]any
		for i := range n {
			out = append(out, largeAnswerCookie(fmt.Sprintf("pad%d", i), strings.Repeat("x", 120), domain))
		}
		return out
	}
	all := append(append([]map[string]any{}, relevant...), pad(120, ".doubleclick.net")...)
	scoped := append(append([]map[string]any{}, relevant...), pad(110, ".youtube.com")...)
	if b, _ := json.Marshal(scoped); len(b) <= 32<<10 {
		t.Fatalf("precondition: the scoped answer is %d bytes, not over 32 KiB", len(b))
	}
	port := startLargeAnswerCDP(t, all, scoped)
	out, err := cdpGetCookiesAsNetscape(context.Background(), port)
	if err != nil {
		t.Fatalf("browser cookie read: %v", err)
	}
	if !strings.Contains(out, "SAPISID") || !strings.Contains(out, "LOGIN_INFO") {
		t.Errorf("the read lost the session cookies: %d bytes", len(out))
	}
}

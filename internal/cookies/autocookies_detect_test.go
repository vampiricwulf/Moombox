package cookies

import "testing"

// TestParseDefaultBrowserProgID covers the registry-output parser used by
// detectDefaultBrowserWindows so the ProgID matcher table can be exercised
// without shelling out to reg.exe (cookies.md #57). Sample outputs taken
// from real Windows 10/11 `reg query` invocations.
func TestParseDefaultBrowserProgID(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want string
	}{
		{
			name: "chrome",
			out: "\r\nHKEY_CURRENT_USER\\...\\UserChoice\r\n" +
				"    ProgId    REG_SZ    ChromeHTML\r\n",
			want: "chrome",
		},
		{
			name: "firefox",
			out:  "    ProgId    REG_SZ    FirefoxURL-308046B0AF4A39CB\r\n",
			want: "firefox",
		},
		{
			name: "waterfox",
			out:  "    ProgId    REG_SZ    WaterfoxHTML-12345\r\n",
			want: "waterfox",
		},
		{
			name: "edge",
			out:  "    ProgId    REG_SZ    MSEdgeHTM\r\n",
			want: "edge",
		},
		{
			name: "brave",
			out:  "    ProgId    REG_SZ    BraveHTML.B12345-9999\r\n",
			want: "brave",
		},
		{
			name: "opera gx",
			out:  "    ProgId    REG_SZ    OperaGXStable\r\n",
			want: "opera",
		},
		{
			name: "opera regular",
			out:  "    ProgId    REG_SZ    OperaStable\r\n",
			want: "opera",
		},
		{
			name: "vivaldi",
			out:  "    ProgId    REG_SZ    VivaldiHTM.ABC123\r\n",
			want: "vivaldi",
		},
		{
			name: "thorium",
			out:  "    ProgId    REG_SZ    ThoriumHTM\r\n",
			want: "thorium",
		},
		{
			name: "librewolf url form",
			out:  "    ProgId    REG_SZ    LibreWolfURL-308046B0AF4A39CB\r\n",
			want: "librewolf",
		},
		{
			name: "zen",
			out:  "    ProgId    REG_SZ    ZenURL-308046B0AF4A39CB\r\n",
			want: "zen",
		},
		{
			name: "unknown ProgId returns empty",
			out:  "    ProgId    REG_SZ    SomeRandomBrowserHTML\r\n",
			want: "",
		},
		{
			name: "missing ProgId line returns empty",
			out:  "HKEY_CURRENT_USER\\...\\UserChoice\r\n",
			want: "",
		},
		{
			name: "empty output returns empty",
			out:  "",
			want: "",
		},
		{
			name: "case-insensitive (registry returns lowercase)",
			out:  "    ProgId    REG_SZ    chromehtml\r\n",
			want: "chrome",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseDefaultBrowserProgID(tc.out); got != tc.want {
				t.Errorf("parseDefaultBrowserProgID: got %q, want %q\ninput:\n%s",
					got, tc.want, tc.out)
			}
		})
	}
}

func TestDetectBrowsersReturnsSliceNeverNil(t *testing.T) {
	browsers := DetectBrowsers()
	if browsers == nil {
		t.Fatal("DetectBrowsers returned nil; expected empty slice")
	}
	// On a CI runner without browsers installed this may be empty.
	// We only assert non-nil to make ranges safe in callers.
	for _, b := range browsers {
		if b.Type == "" || b.Name == "" || b.Path == "" {
			t.Errorf("incomplete browser entry: %+v", b)
		}
	}
}

func TestDetectBrowsersDeduplicates(t *testing.T) {
	// DetectBrowsers should never return the same Path twice — both
	// the PATH lookup and Windows install-dir search may surface the
	// same binary, so the implementation must dedupe.
	browsers := DetectBrowsers()
	seen := map[string]struct{}{}
	for _, b := range browsers {
		if _, dup := seen[b.Path]; dup {
			t.Errorf("duplicate path returned: %s", b.Path)
		}
		seen[b.Path] = struct{}{}
	}
}

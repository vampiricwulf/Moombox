package docs

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// settingsReferenceDir holds the GitHub Pages site's configuration reference.
// Every config.toml key gets one heading there, written as the key's dotted
// path in backticks, as in
//
//	### `downloader.segment_workers`
//
// and arrays of tables take [] (channels[].archive_slots,
// notifications[].url).
const settingsReferenceDir = "docs-site/docs/configuration"

// settingHeading matches a reference heading: any level, the dotted path in
// backticks, nothing else on the line.
var settingHeading = regexp.MustCompile("^#{2,6} `([a-z0-9_\\[\\].]+)`\\s*$")

// configKeys walks t's toml tags and returns every leaf key as a dotted path.
// A struct that decodes itself (UnmarshalTOML — FlexDuration, ChannelTerms)
// is one leaf, not a section.
func configKeys(t reflect.Type, prefix string) []string {
	unmarshaler := reflect.TypeFor[interface{ UnmarshalTOML(any) error }]()
	var keys []string
	for f := range t.Fields() {
		tag := f.Tag.Get("toml")
		name, _, _ := strings.Cut(tag, ",")
		if name == "-" || !f.IsExported() {
			continue
		}
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		path := prefix + name
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.Struct {
			keys = append(keys, configKeys(ft.Elem(), path+"[].")...)
			continue
		}
		if ft.Kind() == reflect.Struct && !reflect.PointerTo(ft).Implements(unmarshaler) {
			keys = append(keys, configKeys(ft, path+".")...)
			continue
		}
		keys = append(keys, path)
	}
	return keys
}

// documentedKeys reads every settings heading under settingsReferenceDir,
// mapping each key to the file it is documented in.
func documentedKeys(t *testing.T, root string) map[string][]string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(root, settingsReferenceDir, "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no settings reference pages under %s", settingsReferenceDir)
	}
	documented := map[string][]string{}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for line := range strings.SplitSeq(string(data), "\n") {
			if m := settingHeading.FindStringSubmatch(strings.TrimRight(line, "\r")); m != nil {
				documented[m[1]] = append(documented[m[1]], filepath.Base(file))
			}
		}
	}
	return documented
}

// TestSettingsReferenceCoversEveryConfigKey fails when a config.toml key has
// no heading in the Pages site's configuration reference, when a heading names
// a key the config no longer has, or when one key is documented twice.
func TestSettingsReferenceCoversEveryConfigKey(t *testing.T) {
	root := repoRoot(t)
	keys := configKeys(reflect.TypeFor[config.MoomboxConfig](), "")
	if len(keys) < 50 {
		t.Fatalf("walked only %d config keys; the walker has stopped seeing the config", len(keys))
	}
	documented := documentedKeys(t, root)

	for _, key := range keys {
		if _, ok := documented[key]; !ok {
			t.Errorf("config key %s has no heading \"### `%s`\" under %s", key, key, settingsReferenceDir)
		}
	}
	for key, files := range documented {
		if !slices.Contains(keys, key) {
			t.Errorf("%s documents %s, which is not a config key", strings.Join(files, ", "), key)
		}
		if len(files) > 1 {
			t.Errorf("config key %s is documented more than once: %s", key, strings.Join(files, ", "))
		}
	}
}

// TestConfigKeyWalker pins the walker's shape on keys that exercise each rule:
// a plain leaf, a self-decoding struct, a pointer leaf and an array of tables.
func TestConfigKeyWalker(t *testing.T) {
	keys := configKeys(reflect.TypeFor[config.MoomboxConfig](), "")
	for _, want := range []string{
		"network.port",
		"monitors.feed_check_interval",
		"channels[].terms",
		"channels[].enabled",
		"notifications[].url",
	} {
		if !slices.Contains(keys, want) {
			t.Errorf("walker missed %s; got %v", want, keys)
		}
	}
	for _, key := range keys {
		if strings.Contains(key, "feed_check_interval.") || strings.Contains(key, "terms.") {
			t.Errorf("walker descended into a self-decoding value: %s", key)
		}
	}
}

package youtube

import (
	"context"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// TestLatencyClassThroughTheCascade: a response's videoDetails.latencyClass
// reaches VideoInfo (owner ruling RV-19); a response with videoDetails and no
// class is asked-but-unstated; one without videoDetails is not asked; and a
// cascade whose winner carried no videoDetails takes the first response's
// that did (web_embedded and web_creator return none).
//
// Mutants: readLatencyClass reporting asked for an empty videoDetails;
// noteLatencyClass letting a later response replace the first;
// finishExtraction not stamping the extraction's class.
func TestLatencyClassThroughTheCascade(t *testing.T) {
	for _, tc := range []struct {
		vd    string
		class string
		asked bool
	}{
		{``, "", false},
		{`{}`, "", false},
		{`{"videoId": "test1234567"}`, "", true},
		{`{"videoId": "test1234567", "latencyClass": "MDE_STREAM_OPTIMIZATIONS_RENDERER_LATENCY_LOW"}`, "MDE_STREAM_OPTIMIZATIONS_RENDERER_LATENCY_LOW", true},
	} {
		var vd map[string]any
		if tc.vd != "" {
			vd = decodePlayerJSON(t, tc.vd)
		}
		class, asked := readLatencyClass(vd)
		if class != tc.class || asked != tc.asked {
			t.Errorf("%s: class %q asked %v, want %q %v", tc.vd, class, asked, tc.class, tc.asked)
		}
	}

	const formats = `"streamingData": {"adaptiveFormats": [{"itag": 18, "url": "https://x/18", "mimeType": "video/mp4; codecs=\"avc1.42001E, mp4a.40.2\"", "width": 480, "height": 360}]}`
	response := func(vd string) map[string]any {
		body := `{"playabilityStatus": {"status": "OK"}, ` + formats
		if vd != "" {
			body += `, "videoDetails": ` + vd
		}
		return decodePlayerJSON(t, body+`}`)
	}
	p := NewPlayerAPI(nil, noopLogger{})

	single, err := p.parsePlayerResponse(context.Background(),
		response(`{"videoId": "test1234567", "latencyClass": "MDE_STREAM_OPTIMIZATIONS_RENDERER_LATENCY_ULTRA_LOW"}`), "", nil, "test1234567")
	if err != nil {
		t.Fatal(err)
	}
	if single.LatencyClass != "MDE_STREAM_OPTIMIZATIONS_RENDERER_LATENCY_ULTRA_LOW" || !single.LatencyClassAsked {
		t.Errorf("a single parse: %q asked %v", single.LatencyClass, single.LatencyClassAsked)
	}

	ctx := withExtractionState(context.Background())
	for _, vd := range []string{``, `{"videoId": "test1234567", "latencyClass": "MDE_STREAM_OPTIMIZATIONS_RENDERER_LATENCY_NORMAL"}`,
		`{"videoId": "test1234567", "latencyClass": "MDE_STREAM_OPTIMIZATIONS_RENDERER_LATENCY_LOW"}`} {
		if _, err := p.parsePlayerResponse(ctx, response(vd), "", nil, "test1234567"); err != nil {
			t.Fatal(err)
		}
	}
	winner, err := p.parsePlayerResponse(ctx, response(``), "", nil, "test1234567")
	if err != nil {
		t.Fatal(err)
	}
	if winner.LatencyClassAsked {
		t.Fatal("a response without videoDetails reads as asked")
	}
	info, err := p.finishExtraction(ctx, winner, nil, "test1234567", &mismatchTally{attempts: 4, survived: 4}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if info.LatencyClass != "MDE_STREAM_OPTIMIZATIONS_RENDERER_LATENCY_NORMAL" || !info.LatencyClassAsked {
		t.Errorf("the extraction's class = %q asked %v, want the first videoDetails response's NORMAL", info.LatencyClass, info.LatencyClassAsked)
	}
}

// TestStatusProbeCarriesTheLatencyClass: the latency-mode backfill's request
// is the status probe, one ANDROID_VR call outside any cascade, and an ended
// stream's answer carries its class through it (owner ruling RV-19).
//
// Mutant: parsePlayerResponse leaving LatencyClass off the VideoInfo it
// builds (only the cascade's stamp setting it).
func TestStatusProbeCarriesTheLatencyClass(t *testing.T) {
	serveBodies(t, map[string]string{
		clientAndroidVR: `{"playabilityStatus":{"status":"OK"},"videoDetails":{"videoId":"test1234567","title":"t","author":"a",` +
			`"isLive":false,"isLiveContent":true,"latencyClass":"MDE_STREAM_OPTIMIZATIONS_RENDERER_LATENCY_NORMAL"}}`,
	})
	svc := NewService(cookies.NewCookieJar(), noopLogger{})
	info, err := svc.ProbeVideoStatus(context.Background(), "test1234567")
	if err != nil {
		t.Fatal(err)
	}
	if info.LatencyClass != "MDE_STREAM_OPTIMIZATIONS_RENDERER_LATENCY_NORMAL" || !info.LatencyClassAsked {
		t.Errorf("probe: class %q asked %v, want NORMAL asked", info.LatencyClass, info.LatencyClassAsked)
	}
}

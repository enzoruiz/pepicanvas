package latency_test

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/latency"
)

const (
	testCorrelation = "0123456789abcdef0123456789abcdef"
	// clockOffsetMs is server time minus client time in the default fixture.
	clockOffsetMs = 1000.0
	acceptedMs    = 5000.0
	defaultRTTMs  = 10.0
)

func testPins() latency.Pins {
	return latency.Pins{
		ReleaseManifestSHA256: strings.Repeat("a", 64),
		Release:               "0.1.0",
		Go:                    "1.27.2",
		Node:                  "24.16.0",
		PackageManager:        latency.Component{Name: "npm", Version: "11.13.0"},
		GoModules:             []latency.Component{{Name: "github.com/jackc/pgx/v5", Version: "v5.11.0"}},
		FrontendDependencies:  []latency.Component{{Name: "react", Version: "19.3.0"}},
		PostgreSQL:            "17.4",
		S3CompatibleStorage:   "1.0.0-contract",
		FFmpeg:                "7.1.1",
		FFprobe:               "7.1.1",
		Clients: []latency.Client{
			{Family: "chrome", Slot: "current", Version: "155.0.8059.79"},
			{Family: "chrome", Slot: "previous", Version: "154.0.8037.92"},
			{Family: "edge", Slot: "current", Version: "155.0.4283.45"},
			{Family: "edge", Slot: "previous", Version: "154.0.4258.62"},
			{Family: "obs", Slot: "current", Version: "32.2.2"},
			{Family: "obs", Slot: "previous", Version: "32.2.1"},
		},
	}
}

// ping builds a symmetric ping sent at sentMs (client clock) with the given RTT
// and the default clock offset.
func ping(sentMs, rttMs float64) latency.Ping {
	return latency.Ping{
		ClientSentMs:     sentMs,
		ServerAtNs:       int64(math.Round((sentMs + rttMs/2 + clockOffsetMs) * 1e6)),
		ClientReceivedMs: sentMs + rttMs,
	}
}

// sample builds a valid sample whose latency is latencyMs.
func sample(index int, kind string, latencyMs float64) latency.Sample {
	pings := make([]latency.Ping, 0, 5)
	for i := 0; i < 5; i++ {
		pings = append(pings, ping(float64(100+i*20), defaultRTTMs))
	}
	return latency.Sample{
		ID:                   fmt.Sprintf("s-%03d", index),
		Kind:                 kind,
		OperationID:          fmt.Sprintf("op-%03d", index),
		Sequence:             int64(index + 1),
		CommandCorrelationID: testCorrelation,
		RenderCorrelationID:  testCorrelation,
		AcceptedAtNs:         int64(acceptedMs * 1e6),
		RenderedAtMs:         acceptedMs - clockOffsetMs + latencyMs,
		Pings:                pings,
		Disconnects:          0,
	}
}

// evidence builds a MEASURED transmission with n valid samples cycling over
// every kind; sample i has latency i+1 ms.
func evidence(n int) latency.Evidence {
	samples := make([]latency.Sample, 0, n)
	for i := 0; i < n; i++ {
		samples = append(samples, sample(i, latency.Kinds[i%len(latency.Kinds)], float64(i+1)))
	}
	return latency.Evidence{
		SchemaVersion: 1,
		Transmission: latency.Transmission{
			ID:                 "chrome-current-20261024T000000Z",
			ScenarioID:         "remote-render-latency",
			CorrelationID:      testCorrelation,
			StartedAt:          "2026-10-24T00:00:00Z",
			CompletedAt:        "2026-10-24T00:05:00Z",
			MeasurementStartNs: int64(acceptedMs * 1e6),
			Client:             latency.Client{Family: "chrome", Slot: "current", Version: "155.0.8059.79"},
		},
		Versions: testPins(),
		Samples:  samples,
	}
}

func zeroInvalid() map[string]int {
	counts := make(map[string]int)
	for _, reason := range latency.InvalidReasons {
		counts[reason] = 0
	}
	return counts
}

func kindCounts(n int) map[string]int {
	counts := make(map[string]int)
	for _, kind := range latency.Kinds {
		counts[kind] = 0
	}
	for i := 0; i < n; i++ {
		counts[latency.Kinds[i%len(latency.Kinds)]]++
	}
	return counts
}

func floatEqual(got *float64, want float64) bool {
	return got != nil && math.Abs(*got-want) < 1e-9
}

func TestNearestRankP95(t *testing.T) {
	ascending := func(n int) []float64 {
		values := make([]float64, n)
		for i := range values {
			values[i] = float64(i + 1)
		}
		return values
	}
	tests := []struct {
		name   string
		values []float64
		want   float64
	}{
		{name: "single value", values: []float64{7}, want: 7},
		{name: "five values takes the maximum", values: []float64{1, 2, 3, 4, 5}, want: 5},
		{name: "unsorted input", values: []float64{5, 1, 3}, want: 5},
		{name: "twenty values", values: ascending(20), want: 19},
		{name: "one hundred values", values: ascending(100), want: 95},
		{name: "one hundred one values", values: ascending(101), want: 96},
		{name: "outlier excluded at twenty", values: append(ascending(19), 500), want: 19},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := append([]float64(nil), tt.values...)
			if got := latency.NearestRankP95(input); got != tt.want {
				t.Fatalf("NearestRankP95() = %v, want %v", got, tt.want)
			}
			if !reflect.DeepEqual(input, tt.values) {
				t.Fatal("NearestRankP95 mutated its input")
			}
		})
	}
}

func TestEvaluateMeasuredTransmission(t *testing.T) {
	report := latency.Evaluate(evidence(102), testPins())

	if report.Result != latency.Measured || len(report.Reasons) != 0 {
		t.Fatalf("result = %s %v, want MEASURED without reasons", report.Result, report.Reasons)
	}
	if report.TransmissionID != "chrome-current-20261024T000000Z" || report.Samples != 102 || report.Valid != 102 {
		t.Fatalf("report = %+v", report)
	}
	if !reflect.DeepEqual(report.ValidByKind, kindCounts(102)) {
		t.Fatalf("ValidByKind = %v", report.ValidByKind)
	}
	if !reflect.DeepEqual(report.InvalidByReason, zeroInvalid()) {
		t.Fatalf("InvalidByReason = %v", report.InvalidByReason)
	}
	// Latencies are 1..102 ms; nearest rank ceil(0.95*102) = 97.
	if !floatEqual(report.LatencyP95Ms, 97) {
		t.Fatalf("LatencyP95Ms = %v, want 97", report.LatencyP95Ms)
	}
	if !floatEqual(report.RTTP95Ms, defaultRTTMs) {
		t.Fatalf("RTTP95Ms = %v, want %v", report.RTTP95Ms, defaultRTTMs)
	}
}

func TestEvaluateSampleValidity(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*latency.Sample, latency.Transmission)
		wantReason []string
	}{
		{name: "four pings", mutate: func(s *latency.Sample, _ latency.Transmission) {
			s.Pings = s.Pings[:4]
		}, wantReason: []string{latency.ReasonTooFewPings}},
		{name: "five pings pass", mutate: func(s *latency.Sample, _ latency.Transmission) {}},
		{name: "rtt p95 above 100 ms", mutate: func(s *latency.Sample, _ latency.Transmission) {
			s.Pings[4] = ping(s.Pings[4].ClientSentMs, 100.5)
		}, wantReason: []string{latency.ReasonRTTP95Exceeded}},
		{name: "rtt p95 exactly 100 ms passes", mutate: func(s *latency.Sample, _ latency.Transmission) {
			s.Pings[4] = ping(s.Pings[4].ClientSentMs, 100)
		}},
		{name: "rtt outlier below the 95th rank passes", mutate: func(s *latency.Sample, _ latency.Transmission) {
			for i := 0; i < 15; i++ {
				s.Pings = append(s.Pings, ping(float64(300+i*20), defaultRTTMs))
			}
			s.Pings[0] = ping(s.Pings[0].ClientSentMs, 500)
		}},
		{name: "one disconnect", mutate: func(s *latency.Sample, _ latency.Transmission) {
			s.Disconnects = 1
		}, wantReason: []string{latency.ReasonDisconnected}},
		{name: "accepted before measurement start", mutate: func(s *latency.Sample, tr latency.Transmission) {
			s.AcceptedAtNs = tr.MeasurementStartNs - 1
		}, wantReason: []string{latency.ReasonBeforeMeasurementStart}},
		{name: "accepted at measurement start passes", mutate: func(s *latency.Sample, tr latency.Transmission) {
			s.AcceptedAtNs = tr.MeasurementStartNs
		}},
		{name: "correlation mismatch", mutate: func(s *latency.Sample, _ latency.Transmission) {
			s.RenderCorrelationID = strings.Repeat("f", 32)
		}, wantReason: []string{latency.ReasonCorrelationMismatch}},
		{name: "malformed equal correlation", mutate: func(s *latency.Sample, _ latency.Transmission) {
			s.CommandCorrelationID = strings.ToUpper(testCorrelation)
			s.RenderCorrelationID = strings.ToUpper(testCorrelation)
		}, wantReason: []string{latency.ReasonCorrelationMismatch}},
		{name: "negative rtt ping", mutate: func(s *latency.Sample, _ latency.Transmission) {
			s.Pings[2].ClientReceivedMs = s.Pings[2].ClientSentMs - 0.5
		}, wantReason: []string{latency.ReasonNegativeRTT}},
		{name: "render before acceptance", mutate: func(s *latency.Sample, _ latency.Transmission) {
			s.RenderedAtMs -= 2
		}, wantReason: []string{latency.ReasonNegativeLatency}},
		{name: "render at acceptance passes", mutate: func(s *latency.Sample, _ latency.Transmission) {
			s.RenderedAtMs--
		}},
		{name: "every reason counted once", mutate: func(s *latency.Sample, tr latency.Transmission) {
			s.Pings = s.Pings[:4]
			s.Pings[0].ClientReceivedMs = s.Pings[0].ClientSentMs - 1
			s.Disconnects = 2
			s.AcceptedAtNs = tr.MeasurementStartNs - 1
			s.RenderCorrelationID = strings.Repeat("f", 32)
		}, wantReason: []string{
			latency.ReasonTooFewPings, latency.ReasonNegativeRTT, latency.ReasonDisconnected,
			latency.ReasonBeforeMeasurementStart, latency.ReasonCorrelationMismatch,
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value := evidence(102)
			tt.mutate(&value.Samples[0], value.Transmission)

			report := latency.Evaluate(value, testPins())

			wantInvalid := zeroInvalid()
			for _, reason := range tt.wantReason {
				wantInvalid[reason] = 1
			}
			if !reflect.DeepEqual(report.InvalidByReason, wantInvalid) {
				t.Fatalf("InvalidByReason = %v, want %v", report.InvalidByReason, wantInvalid)
			}
			wantValid := 102
			wantKinds := kindCounts(102)
			if len(tt.wantReason) > 0 {
				wantValid = 101
				wantKinds["create"]--
			}
			if report.Samples != 102 || report.Valid != wantValid {
				t.Fatalf("samples=%d valid=%d, want 102 %d", report.Samples, report.Valid, wantValid)
			}
			if !reflect.DeepEqual(report.ValidByKind, wantKinds) {
				t.Fatalf("ValidByKind = %v, want %v", report.ValidByKind, wantKinds)
			}
			if report.Result != latency.Measured {
				t.Fatalf("result = %s %v, want MEASURED", report.Result, report.Reasons)
			}
		})
	}
}

func TestEvaluateExcludesInvalidSamplesFromPercentiles(t *testing.T) {
	value := evidence(102)
	// Sample 101 has the largest latency and an RTT outlier, so it is invalid.
	value.Samples[101].RenderedAtMs += 10000
	value.Samples[101].Pings[4] = ping(value.Samples[101].Pings[4].ClientSentMs, 900)

	report := latency.Evaluate(value, testPins())

	// Valid latencies are 1..101; nearest rank ceil(0.95*101) = 96.
	if !floatEqual(report.LatencyP95Ms, 96) {
		t.Fatalf("LatencyP95Ms = %v, want 96", report.LatencyP95Ms)
	}
	if !floatEqual(report.RTTP95Ms, defaultRTTMs) {
		t.Fatalf("RTTP95Ms = %v, want %v (pings of invalid samples excluded)", report.RTTP95Ms, defaultRTTMs)
	}
}

func TestEvaluateValidSampleThreshold(t *testing.T) {
	tests := []struct {
		name        string
		total       int
		invalid     int
		wantResult  latency.Result
		wantReasons []string
	}{
		{name: "exactly 100 valid", total: 100, wantResult: latency.Measured},
		{name: "99 valid of 100", total: 100, invalid: 1, wantResult: latency.Inconclusive,
			wantReasons: []string{"insufficient_samples"}},
		{name: "99 valid of 99", total: 99, wantResult: latency.Inconclusive,
			wantReasons: []string{"insufficient_samples"}},
		{name: "100 valid of 101", total: 101, invalid: 1, wantResult: latency.Measured},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value := evidence(tt.total)
			for i := 0; i < tt.invalid; i++ {
				// The last samples are not the only ones of their kind.
				value.Samples[tt.total-1-i].Disconnects = 1
			}

			report := latency.Evaluate(value, testPins())

			if report.Result != tt.wantResult || !reflect.DeepEqual(report.Reasons, tt.wantReasons) {
				t.Fatalf("result = %s %v, want %s %v", report.Result, report.Reasons, tt.wantResult, tt.wantReasons)
			}
			if report.Valid != tt.total-tt.invalid {
				t.Fatalf("valid = %d", report.Valid)
			}
		})
	}
}

func TestEvaluateMissingKind(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*latency.Evidence)
		want   []string
	}{
		{name: "no media samples", mutate: func(e *latency.Evidence) {
			for i := range e.Samples {
				if e.Samples[i].Kind == "media" {
					e.Samples[i].Kind = "create"
				}
			}
		}, want: []string{"missing_kind:media"}},
		{name: "only invalid rotate samples", mutate: func(e *latency.Evidence) {
			for i := range e.Samples {
				if e.Samples[i].Kind == "rotate" {
					e.Samples[i].Disconnects = 1
				}
			}
			// Keep 100 valid samples so only the kind is missing.
			for i := 0; i < 30; i++ {
				e.Samples = append(e.Samples, sample(200+i, "move", 1))
			}
		}, want: []string{"missing_kind:rotate"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value := evidence(102)
			tt.mutate(&value)

			report := latency.Evaluate(value, testPins())

			if report.Result != latency.Inconclusive || !reflect.DeepEqual(report.Reasons, tt.want) {
				t.Fatalf("result = %s %v, want INCONCLUSIVE %v", report.Result, report.Reasons, tt.want)
			}
		})
	}
}

func TestEvaluateIncompatibleClient(t *testing.T) {
	tests := []struct {
		name   string
		client latency.Client
	}{
		{name: "wrong version", client: latency.Client{Family: "chrome", Slot: "current", Version: "155.0.8059.80"}},
		{name: "unknown slot", client: latency.Client{Family: "chrome", Slot: "beta", Version: "155.0.8059.79"}},
		{name: "version of another slot", client: latency.Client{Family: "chrome", Slot: "previous", Version: "155.0.8059.79"}},
		{name: "unknown family", client: latency.Client{Family: "firefox", Slot: "current", Version: "155.0.8059.79"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value := evidence(102)
			value.Transmission.Client = tt.client

			report := latency.Evaluate(value, testPins())

			if report.Result != latency.Inconclusive || !reflect.DeepEqual(report.Reasons, []string{"incompatible_client"}) {
				t.Fatalf("result = %s %v", report.Result, report.Reasons)
			}
			// Samples are still measured and reported.
			if report.Valid != 102 || !floatEqual(report.LatencyP95Ms, 97) {
				t.Fatalf("report = %+v", report)
			}
		})
	}
}

func TestEvaluateVersionMismatch(t *testing.T) {
	tests := []struct {
		field  string
		mutate func(*latency.Versions)
	}{
		{"releaseManifestSha256", func(v *latency.Versions) { v.ReleaseManifestSHA256 = strings.Repeat("b", 64) }},
		{"release", func(v *latency.Versions) { v.Release = "0.1.1" }},
		{"go", func(v *latency.Versions) { v.Go = "1.27.3" }},
		{"node", func(v *latency.Versions) { v.Node = "24.16.1" }},
		{"packageManager", func(v *latency.Versions) { v.PackageManager.Version = "11.13.1" }},
		{"packageManager", func(v *latency.Versions) { v.PackageManager.Name = "pnpm" }},
		{"goModules", func(v *latency.Versions) { v.GoModules[0].Version = "v5.11.1" }},
		{"goModules", func(v *latency.Versions) {
			v.GoModules = append(v.GoModules, latency.Component{Name: "golang.org/x/crypto", Version: "v0.58.0"})
		}},
		{"goModules", func(v *latency.Versions) { v.GoModules = nil }},
		{"frontendDependencies", func(v *latency.Versions) { v.FrontendDependencies[0].Name = "preact" }},
		{"postgresql", func(v *latency.Versions) { v.PostgreSQL = "17.5" }},
		{"s3CompatibleStorage", func(v *latency.Versions) { v.S3CompatibleStorage = "1.0.1-contract" }},
		{"ffmpeg", func(v *latency.Versions) { v.FFmpeg = "7.1.2" }},
		{"ffprobe", func(v *latency.Versions) { v.FFprobe = "7.1.2" }},
		{"clients", func(v *latency.Versions) { v.Clients[5].Version = "32.2.0" }},
		{"clients", func(v *latency.Versions) { v.Clients[0], v.Clients[1] = v.Clients[1], v.Clients[0] }},
		{"clients", func(v *latency.Versions) { v.Clients = v.Clients[:5] }},
	}
	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			value := evidence(102)
			tt.mutate(&value.Versions)

			report := latency.Evaluate(value, testPins())

			want := []string{"version_mismatch:" + tt.field}
			if report.Result != latency.Inconclusive || !reflect.DeepEqual(report.Reasons, want) {
				t.Fatalf("result = %s %v, want INCONCLUSIVE %v", report.Result, report.Reasons, want)
			}
		})
	}
}

func TestEvaluateReportsEveryReasonInOrder(t *testing.T) {
	value := evidence(102)
	value.Transmission.Client.Version = "1"
	value.Versions.Go = "0"
	value.Versions.FFprobe = "0"
	for i := range value.Samples {
		if value.Samples[i].Kind == "delete" || value.Samples[i].Kind == "create" {
			value.Samples[i].Disconnects = 1
		}
	}

	report := latency.Evaluate(value, testPins())

	want := []string{
		"incompatible_client", "version_mismatch:go", "version_mismatch:ffprobe",
		"insufficient_samples", "missing_kind:create", "missing_kind:delete",
	}
	if report.Result != latency.Inconclusive || !reflect.DeepEqual(report.Reasons, want) {
		t.Fatalf("result = %s %v, want INCONCLUSIVE %v", report.Result, report.Reasons, want)
	}
	if report.InvalidByReason[latency.ReasonDisconnected] != 34 {
		t.Fatalf("InvalidByReason = %v", report.InvalidByReason)
	}
}

func TestEvaluateRejectsDuplicateSamples(t *testing.T) {
	for _, tc := range []struct {
		name      string
		duplicate func(original latency.Sample, copy *latency.Sample)
	}{
		{"same sample", func(latency.Sample, *latency.Sample) {}},
		{"same id", func(original latency.Sample, copy *latency.Sample) {
			copy.OperationID, copy.Sequence = "op-new", 999
		}},
		{"same operation", func(original latency.Sample, copy *latency.Sample) {
			copy.ID, copy.Sequence = "s-new", 999
		}},
		{"same sequence", func(original latency.Sample, copy *latency.Sample) {
			copy.ID, copy.OperationID = "s-new", "op-new"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := evidence(100)
			copy := value.Samples[0]
			tc.duplicate(value.Samples[0], &copy)
			value.Samples[99] = copy

			report := latency.Evaluate(value, testPins())

			if report.Valid != 99 || report.InvalidByReason[latency.ReasonDuplicateSample] != 1 {
				t.Fatalf("valid = %d, invalid = %v, want 99 valid and 1 duplicate", report.Valid, report.InvalidByReason)
			}
			if report.Result != latency.Inconclusive || !reflect.DeepEqual(report.Reasons, []string{"insufficient_samples"}) {
				t.Fatalf("result = %s %v, want INCONCLUSIVE [insufficient_samples]", report.Result, report.Reasons)
			}
		})
	}
}

// Six real measurements repeated to reach 102 samples must never be MEASURED.
func TestEvaluateRepeatedMeasurementsAreInconclusive(t *testing.T) {
	value := evidence(102)
	for i := range value.Samples {
		value.Samples[i] = value.Samples[i%len(latency.Kinds)]
	}

	report := latency.Evaluate(value, testPins())

	if report.Result != latency.Inconclusive || report.Valid != len(latency.Kinds) {
		t.Fatalf("result = %s valid = %d, want INCONCLUSIVE with %d valid", report.Result, report.Valid, len(latency.Kinds))
	}
	if report.InvalidByReason[latency.ReasonDuplicateSample] != 102-len(latency.Kinds) {
		t.Fatalf("InvalidByReason = %v", report.InvalidByReason)
	}
}

func TestEvaluateUnknownScenario(t *testing.T) {
	value := evidence(102)
	value.Transmission.ScenarioID = "other-scenario"
	value.Versions.Go = "0"

	report := latency.Evaluate(value, testPins())

	want := []string{"unknown_scenario", "version_mismatch:go"}
	if report.Result != latency.Inconclusive || !reflect.DeepEqual(report.Reasons, want) {
		t.Fatalf("result = %s %v, want INCONCLUSIVE %v", report.Result, report.Reasons, want)
	}
}

func TestEvaluateNoValidSamples(t *testing.T) {
	for _, total := range []int{0, 3} {
		t.Run(fmt.Sprint(total), func(t *testing.T) {
			value := evidence(total)
			for i := range value.Samples {
				value.Samples[i].Disconnects = 1
			}

			report := latency.Evaluate(value, testPins())

			want := []string{"insufficient_samples"}
			for _, kind := range latency.Kinds {
				want = append(want, "missing_kind:"+kind)
			}
			if report.Result != latency.Inconclusive || !reflect.DeepEqual(report.Reasons, want) {
				t.Fatalf("result = %s %v", report.Result, report.Reasons)
			}
			if report.Samples != total || report.Valid != 0 || report.RTTP95Ms != nil || report.LatencyP95Ms != nil {
				t.Fatalf("report = %+v", report)
			}
			if report.InvalidByReason[latency.ReasonDisconnected] != total {
				t.Fatalf("InvalidByReason = %v", report.InvalidByReason)
			}
		})
	}
}

func TestEvaluateLatencyUsesMinimumRTTPingOffset(t *testing.T) {
	value := evidence(102)
	for i := range value.Samples {
		s := &value.Samples[i]
		s.Pings = []latency.Ping{
			// RTT 40, asymmetric: the server answered 900 ms ahead of the client midpoint.
			{ClientSentMs: 0, ServerAtNs: 920_000_000, ClientReceivedMs: 40},
			// Minimum RTT 4: offset = 1102 - (100+104)/2 = 1000 ms.
			{ClientSentMs: 100, ServerAtNs: 1_102_000_000, ClientReceivedMs: 104},
			// RTT 10 with a 1300 ms apparent offset.
			{ClientSentMs: 200, ServerAtNs: 1_505_000_000, ClientReceivedMs: 210},
			{ClientSentMs: 300, ServerAtNs: 1_605_000_000, ClientReceivedMs: 310},
			{ClientSentMs: 400, ServerAtNs: 1_705_000_000, ClientReceivedMs: 410},
		}
		s.AcceptedAtNs = 5_000_000_000
		s.RenderedAtMs = 4012.5
	}

	report := latency.Evaluate(value, testPins())

	// latency = 4012.5 + 1000 - 5000.
	if !floatEqual(report.LatencyP95Ms, 12.5) {
		t.Fatalf("LatencyP95Ms = %v, want 12.5", report.LatencyP95Ms)
	}
	// RTTs per sample are [40 4 10 10 10]; over 510 pings rank ceil(0.95*510)=485 is 40.
	if !floatEqual(report.RTTP95Ms, 40) {
		t.Fatalf("RTTP95Ms = %v, want 40", report.RTTP95Ms)
	}
}

func encode(t *testing.T, value latency.Evidence) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestDecodeRoundTrip(t *testing.T) {
	want := evidence(7)

	got, err := latency.Decode(encode(t, want))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Decode() = %+v, want %+v", got, want)
	}
}

func TestDecodeStructuralErrors(t *testing.T) {
	const secret = "SECRET-CONTENT-7f3a"
	valid := func(t *testing.T) string { return string(encode(t, evidence(3))) }
	tests := []struct {
		name    string
		data    func(*testing.T) string
		wantErr string
	}{
		{name: "empty input", data: func(*testing.T) string { return "" }, wantErr: "invalid JSON"},
		{name: "not an object", data: func(*testing.T) string { return "[]" }, wantErr: "$ must be an object"},
		{name: "unknown top-level field", data: func(t *testing.T) string {
			return strings.Replace(valid(t), `{"schemaVersion"`, `{"`+secret+`":1,"schemaVersion"`, 1)
		}, wantErr: "unknown field at $"},
		{name: "unknown ping field", data: func(t *testing.T) string {
			return strings.Replace(valid(t), `{"clientSentMs"`, `{"`+secret+`":1,"clientSentMs"`, 1)
		}, wantErr: "unknown field at $.samples[0].pings[0]"},
		{name: "duplicate key", data: func(t *testing.T) string {
			return strings.Replace(valid(t), `"disconnects":0`, `"disconnects":0,"disconnects":0`, 1)
		}, wantErr: "duplicate field at $.samples[0].disconnects"},
		{name: "trailing value", data: func(t *testing.T) string { return valid(t) + ` {}` }, wantErr: "single JSON value"},
		{name: "trailing garbage", data: func(t *testing.T) string { return valid(t) + secret }, wantErr: "single JSON value"},
		{name: "truncated", data: func(t *testing.T) string { v := valid(t); return v[:len(v)-1] }, wantErr: "invalid JSON"},
		{name: "missing sample field", data: func(t *testing.T) string {
			return strings.Replace(valid(t), `,"disconnects":0`, ``, 1)
		}, wantErr: "missing field $.samples[0].disconnects"},
		{name: "missing client", data: func(t *testing.T) string {
			v := valid(t)
			start := strings.Index(v, `,"client":{`)
			end := start + strings.Index(v[start:], `}`) + 1
			return v[:start] + v[end:]
		}, wantErr: "missing field $.transmission.client"},
		{name: "null field", data: func(t *testing.T) string {
			return strings.Replace(valid(t), `"versions":{`, `"versions":null,"x":{`, 1)
		}, wantErr: "$.versions must be an object"},
		{name: "wrong type", data: func(t *testing.T) string {
			return strings.Replace(valid(t), `"sequence":1`, `"sequence":"`+secret+`"`, 1)
		}, wantErr: "$.samples[0].sequence must be an integer"},
		{name: "fractional integer", data: func(t *testing.T) string {
			return strings.Replace(valid(t), `"sequence":1`, `"sequence":1.5`, 1)
		}, wantErr: "$.samples[0].sequence must be an integer"},
		{name: "schema version 2", data: func(t *testing.T) string {
			return strings.Replace(valid(t), `"schemaVersion":1`, `"schemaVersion":2`, 1)
		}, wantErr: "schemaVersion must be 1"},
		{name: "unknown kind", data: func(t *testing.T) string {
			return strings.Replace(valid(t), `"kind":"create"`, `"kind":"`+secret+`"`, 1)
		}, wantErr: "$.samples[0].kind must be one of create, move, resize, rotate, delete, media"},
		{name: "startedAt not RFC3339", data: func(t *testing.T) string {
			return strings.Replace(valid(t), `"startedAt":"2026-10-24T00:00:00Z"`, `"startedAt":"`+secret+`"`, 1)
		}, wantErr: "$.transmission.startedAt must be RFC3339"},
		{name: "completed before started", data: func(t *testing.T) string {
			return strings.Replace(valid(t), `"completedAt":"2026-10-24T00:05:00Z"`, `"completedAt":"2026-10-23T00:00:00Z"`, 1)
		}, wantErr: "$.transmission.completedAt must not precede startedAt"},
		{name: "malformed transmission correlation", data: func(t *testing.T) string {
			return strings.Replace(valid(t), `"correlationId":"`+testCorrelation+`"`, `"correlationId":"`+secret+`"`, 1)
		}, wantErr: "$.transmission.correlationId must be 32 lowercase hex"},
		{name: "empty transmission id", data: func(t *testing.T) string {
			return strings.Replace(valid(t), `"id":"chrome-current-20261024T000000Z"`, `"id":""`, 1)
		}, wantErr: "$.transmission.id must be an identifier"},
		{name: "transmission id with whitespace", data: func(t *testing.T) string {
			return strings.Replace(valid(t), `"id":"chrome-current-20261024T000000Z"`, `"id":"a\nb `+secret+`"`, 1)
		}, wantErr: "$.transmission.id must be an identifier"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := latency.Decode([]byte(tt.data(t)))
			if err == nil {
				t.Fatal("Decode() error = nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Decode() error = %q, want it to contain %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("Decode() error echoes evidence content: %q", err)
			}
		})
	}
}

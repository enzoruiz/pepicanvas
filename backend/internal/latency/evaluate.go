package latency

import (
	"slices"
	"sort"
)

const (
	minimumPings      = 5
	maximumRTTP95Ms   = 100.0
	minimumValidCount = 100
)

// Evaluate classifies a transmission returned by Decode against the current pins.
func Evaluate(evidence Evidence, pins Pins) Report {
	report := Report{
		TransmissionID:  evidence.Transmission.ID,
		Samples:         len(evidence.Samples),
		ValidByKind:     make(map[string]int, len(Kinds)),
		InvalidByReason: make(map[string]int, len(InvalidReasons)),
	}
	for _, kind := range Kinds {
		report.ValidByKind[kind] = 0
	}
	for _, reason := range InvalidReasons {
		report.InvalidByReason[reason] = 0
	}

	var rtts, latencies []float64
	seen := newSeenSamples()
	for _, sample := range evidence.Samples {
		reasons := sampleReasons(sample, evidence.Transmission.MeasurementStartNs)
		if seen.repeats(sample) {
			reasons = append(reasons, ReasonDuplicateSample)
		}
		for _, reason := range reasons {
			report.InvalidByReason[reason]++
		}
		if len(reasons) > 0 {
			continue
		}
		report.Valid++
		report.ValidByKind[sample.Kind]++
		rtts = append(rtts, pingRTTs(sample.Pings)...)
		latencies = append(latencies, sampleLatencyMs(sample))
	}
	if len(rtts) > 0 {
		value := NearestRankP95(rtts)
		report.RTTP95Ms = &value
	}
	if len(latencies) > 0 {
		value := NearestRankP95(latencies)
		report.LatencyP95Ms = &value
	}

	if evidence.Transmission.ScenarioID != Scenario {
		report.Reasons = append(report.Reasons, "unknown_scenario")
	}
	if !slices.Contains(pins.Clients, evidence.Transmission.Client) {
		report.Reasons = append(report.Reasons, "incompatible_client")
	}
	for _, field := range versionMismatches(evidence.Versions, pins) {
		report.Reasons = append(report.Reasons, "version_mismatch:"+field)
	}
	if report.Valid < minimumValidCount {
		report.Reasons = append(report.Reasons, "insufficient_samples")
	}
	for _, kind := range Kinds {
		if report.ValidByKind[kind] == 0 {
			report.Reasons = append(report.Reasons, "missing_kind:"+kind)
		}
	}
	report.Result = Measured
	if len(report.Reasons) > 0 {
		report.Result = Inconclusive
	}
	return report
}

func sampleReasons(sample Sample, measurementStartNs int64) []string {
	var reasons []string
	if len(sample.Pings) < minimumPings {
		reasons = append(reasons, ReasonTooFewPings)
	}
	rtts := pingRTTs(sample.Pings)
	negative := false
	for _, rtt := range rtts {
		if rtt < 0 {
			negative = true
		}
	}
	switch {
	case negative:
		reasons = append(reasons, ReasonNegativeRTT)
	case len(rtts) > 0 && NearestRankP95(rtts) > maximumRTTP95Ms:
		reasons = append(reasons, ReasonRTTP95Exceeded)
	}
	// A render cannot precede its acceptance, so a negative latency means the
	// clock mapping is broken. It is undefined without a usable ping.
	negativeLatency := len(rtts) > 0 && !negative && sampleLatencyMs(sample) < 0
	if sample.Disconnects != 0 {
		reasons = append(reasons, ReasonDisconnected)
	}
	if sample.AcceptedAtNs < measurementStartNs {
		reasons = append(reasons, ReasonBeforeMeasurementStart)
	}
	if sample.CommandCorrelationID != sample.RenderCorrelationID || !correlationPattern.MatchString(sample.CommandCorrelationID) {
		reasons = append(reasons, ReasonCorrelationMismatch)
	}
	if negativeLatency {
		reasons = append(reasons, ReasonNegativeLatency)
	}
	return reasons
}

// seenSamples remembers the identity of every earlier sample, so a sample
// that repeats an id, operation or sequence is never counted as a second
// measurement.
type seenSamples struct {
	ids, operations map[string]bool
	sequences       map[int64]bool
}

func newSeenSamples() seenSamples {
	return seenSamples{ids: map[string]bool{}, operations: map[string]bool{}, sequences: map[int64]bool{}}
}

// repeats reports whether sample shares an identity with an earlier one and
// then records it.
func (s seenSamples) repeats(sample Sample) bool {
	repeated := s.ids[sample.ID] || s.operations[sample.OperationID] || s.sequences[sample.Sequence]
	s.ids[sample.ID] = true
	s.operations[sample.OperationID] = true
	s.sequences[sample.Sequence] = true
	return repeated
}

func pingRTTs(pings []Ping) []float64 {
	rtts := make([]float64, len(pings))
	for i, ping := range pings {
		rtts[i] = ping.ClientReceivedMs - ping.ClientSentMs
	}
	return rtts
}

// sampleLatencyMs maps the render instant to the server clock with the offset
// of the minimum-RTT ping (the first one on ties). The sample needs a ping.
func sampleLatencyMs(sample Sample) float64 {
	best := sample.Pings[0]
	for _, ping := range sample.Pings[1:] {
		if ping.ClientReceivedMs-ping.ClientSentMs < best.ClientReceivedMs-best.ClientSentMs {
			best = ping
		}
	}
	offsetMs := float64(best.ServerAtNs)/1e6 - (best.ClientSentMs+best.ClientReceivedMs)/2
	return sample.RenderedAtMs + offsetMs - float64(sample.AcceptedAtNs)/1e6
}

// versionMismatches lists the versions fields that differ from the pins, in
// schema field order.
func versionMismatches(got Versions, want Pins) []string {
	checks := []struct {
		field string
		equal bool
	}{
		{"releaseManifestSha256", got.ReleaseManifestSHA256 == want.ReleaseManifestSHA256},
		{"release", got.Release == want.Release},
		{"go", got.Go == want.Go},
		{"node", got.Node == want.Node},
		{"packageManager", got.PackageManager == want.PackageManager},
		{"goModules", slices.Equal(got.GoModules, want.GoModules)},
		{"frontendDependencies", slices.Equal(got.FrontendDependencies, want.FrontendDependencies)},
		{"postgresql", got.PostgreSQL == want.PostgreSQL},
		{"s3CompatibleStorage", got.S3CompatibleStorage == want.S3CompatibleStorage},
		{"ffmpeg", got.FFmpeg == want.FFmpeg},
		{"ffprobe", got.FFprobe == want.FFprobe},
		{"clients", slices.Equal(got.Clients, want.Clients)},
	}
	var fields []string
	for _, check := range checks {
		if !check.equal {
			fields = append(fields, check.field)
		}
	}
	return fields
}

// NearestRankP95 returns the ceil(0.95*n)-th smallest value without changing
// values. values must not be empty.
func NearestRankP95(values []float64) float64 {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	// ceil(0.95*n) in integer arithmetic avoids floating-point rounding.
	rank := (95*len(sorted) + 99) / 100
	return sorted[rank-1]
}

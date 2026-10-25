// Package latency decodes and evaluates TE-010 acceptance-to-render latency
// evidence. It is pure: callers supply the evidence bytes and the current pins.
//
// A sample is valid when it has at least 5 pings, no ping has a negative RTT,
// the nearest-rank p95 of its ping RTTs is at most 100 ms, it saw no
// disconnect, it was accepted at or after measurementStartNs, its command and
// render correlation IDs are equal 32-character lowercase hex values, its
// latency is not negative, and no earlier sample has the same id, operationId
// or sequence. An invalid sample increments every reason it fails (negative
// RTT pings skip the RTT p95 and latency checks) and is excluded from all
// percentiles.
//
// A transmission is MEASURED only when its scenario is Scenario, its client is
// one of the pinned clients, every versions field equals the pins, it has at
// least 100 valid samples and every kind has a valid sample; otherwise it is
// INCONCLUSIVE with the reasons, in this order: unknown_scenario,
// incompatible_client, version_mismatch:<field> in schema field order,
// insufficient_samples, missing_kind:<kind> in Kinds order.
package latency

// Evidence is one transmission's latency evidence file.
type Evidence struct {
	SchemaVersion int          `json:"schemaVersion"`
	Transmission  Transmission `json:"transmission"`
	Versions      Versions     `json:"versions"`
	Samples       []Sample     `json:"samples"`
}

// Transmission identifies the measured transmission and its client.
type Transmission struct {
	ID                 string `json:"id"`
	ScenarioID         string `json:"scenarioId"`
	CorrelationID      string `json:"correlationId"`
	StartedAt          string `json:"startedAt"`
	CompletedAt        string `json:"completedAt"`
	MeasurementStartNs int64  `json:"measurementStartNs"`
	Client             Client `json:"client"`
}

// Client is a browser or OBS client identified by family, slot and exact version.
type Client struct {
	Family  string `json:"family"`
	Slot    string `json:"slot"`
	Version string `json:"version"`
}

// Component is a named exact version.
type Component struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Versions is the TE-010 manifest recorded by a transmission.
type Versions struct {
	ReleaseManifestSHA256 string      `json:"releaseManifestSha256"`
	Release               string      `json:"release"`
	Go                    string      `json:"go"`
	Node                  string      `json:"node"`
	PackageManager        Component   `json:"packageManager"`
	GoModules             []Component `json:"goModules"`
	FrontendDependencies  []Component `json:"frontendDependencies"`
	PostgreSQL            string      `json:"postgresql"`
	S3CompatibleStorage   string      `json:"s3CompatibleStorage"`
	FFmpeg                string      `json:"ffmpeg"`
	FFprobe               string      `json:"ffprobe"`
	Clients               []Client    `json:"clients"`
}

// Pins are the current versions a transmission must match. Clients are the
// compatible clients of the release manifest, in manifest order.
type Pins = Versions

// Sample is one acceptance-to-render measurement.
type Sample struct {
	ID                   string  `json:"id"`
	Kind                 string  `json:"kind"`
	OperationID          string  `json:"operationId"`
	Sequence             int64   `json:"sequence"`
	CommandCorrelationID string  `json:"commandCorrelationId"`
	RenderCorrelationID  string  `json:"renderCorrelationId"`
	AcceptedAtNs         int64   `json:"acceptedAtNs"`
	RenderedAtMs         float64 `json:"renderedAtMs"`
	Pings                []Ping  `json:"pings"`
	Disconnects          int64   `json:"disconnects"`
}

// Ping is one clock-mapping round trip between the client and the server.
type Ping struct {
	ClientSentMs     float64 `json:"clientSentMs"`
	ServerAtNs       int64   `json:"serverAtNs"`
	ClientReceivedMs float64 `json:"clientReceivedMs"`
}

// Result is the outcome of evaluating one transmission.
type Result string

const (
	Measured     Result = "MEASURED"
	Inconclusive Result = "INCONCLUSIVE"
)

// Scenario is the only scenario this package evaluates.
const Scenario = "remote-render-latency"

// Kinds are the measured command kinds, in report order.
var Kinds = []string{"create", "move", "resize", "rotate", "delete", "media"}

// Invalid sample reasons, in report order (InvalidReasons).
const (
	ReasonTooFewPings            = "too_few_pings"
	ReasonNegativeRTT            = "negative_rtt"
	ReasonRTTP95Exceeded         = "rtt_p95_exceeded"
	ReasonDisconnected           = "disconnected"
	ReasonBeforeMeasurementStart = "before_measurement_start"
	ReasonCorrelationMismatch    = "correlation_mismatch"
	ReasonDuplicateSample        = "duplicate_sample"
	ReasonNegativeLatency        = "negative_latency"
)

// InvalidReasons lists every invalid sample reason in report order.
var InvalidReasons = []string{
	ReasonTooFewPings, ReasonNegativeRTT, ReasonRTTP95Exceeded,
	ReasonDisconnected, ReasonBeforeMeasurementStart, ReasonCorrelationMismatch,
	ReasonDuplicateSample, ReasonNegativeLatency,
}

// Report summarizes one evaluated transmission.
type Report struct {
	TransmissionID  string
	Result          Result
	Reasons         []string
	Samples         int
	Valid           int
	ValidByKind     map[string]int
	InvalidByReason map[string]int
	// RTTP95Ms and LatencyP95Ms are nil when there is no valid sample.
	RTTP95Ms     *float64
	LatencyP95Ms *float64
}

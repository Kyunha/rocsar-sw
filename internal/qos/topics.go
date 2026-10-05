package qos

// The published telemetry topics.
//
// Also the wire vocabulary: ARCHITECTURE.md 5.1. A ZeroMQ SUB filters on frame
// PREFIX, which is why the topic is its own leading frame on the wire -- a topic
// sharing a frame with the payload could not be filtered exactly.
//
// Priority classification used to live here alongside the limiter. It is gone:
// artefact bytes go over HTTP, the camera is snapshot-only, and the camera
// command returns a name rather than an image, so there is no ZeroMQ bulk
// traffic and a second priority class has no producer. See ARCHITECTURE.md 6.6.
const (
	TopicTelemetry = "telemetry"
)

// AllTopics lists every published topic, for the subscription filter and for the
// test that keeps the publisher and the filter in agreement.
func AllTopics() []string {
	return []string{TopicTelemetry}
}

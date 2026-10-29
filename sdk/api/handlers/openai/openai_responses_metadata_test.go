package openai

import (
	"bytes"
	"strings"
	"testing"
)

func TestResponsesSSEFramerFiltersMalformedPrivatePayload(t *testing.T) {
	var output bytes.Buffer
	framer := &responsesSSEFramer{}
	framer.WriteChunk(&output, []byte("data: {\"type\":\"response.metadata\",\"metadata\":{\"marker\":\"private\"}\n\n"))
	framer.WriteChunk(&output, []byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"))
	if got := output.String(); strings.Contains(got, "response.metadata") || strings.Contains(got, "private") {
		t.Fatalf("malformed private payload leaked: %q", got)
	}
}

func TestResponsesSSEFramerPrivateMetadataBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name          string
		frame         string
		isCodexClient bool
		wantPrivate   bool
	}{
		{name: "metadata type only", frame: "data: {\"type\":\"response.metadata\",\"marker\":\"private\"}\n\n"},
		{name: "metadata event malformed", frame: "event: response.metadata\ndata: {broken private\n\n"},
		{name: "metadata event public type", frame: "event: response.metadata\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"private\"}\n\n"},
		{name: "public event metadata type", frame: "event: response.output_text.delta\ndata: {\"type\":\"response.metadata\",\"marker\":\"private\"}\n\n"},
		{name: "malformed codex metadata type", frame: "data: {\"type\":\"codex.response.metadata\",\"marker\":\"private\"\n\n"},
		{name: "malformed telemetry type", frame: "data: {\"type\":\"responsesapi.timing\",\"marker\":\"private\"\n\n"},
		{name: "malformed subscription type", frame: "data: {\"type\":\"response.subscription_usage\",\"marker\":\"private\"\n\n"},
		{name: "codex metadata type only", frame: "data: {\"type\":\"response.metadata\",\"marker\":\"private\"}\n\n", isCodexClient: true, wantPrivate: true},
		{name: "codex malformed metadata", frame: "data: {\"type\":\"response.metadata\",\"marker\":\"private\"\n\n", isCodexClient: true, wantPrivate: true},
		{name: "codex malformed subscription", frame: "data: {\"type\":\"response.subscription_usage\",\"marker\":\"private\"\n\n", isCodexClient: true},
		{name: "public malformed payload", frame: "data: {\"type\":\"response.output_text.delta\",\"delta\":\"private\"\n\n", wantPrivate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			framer := &responsesSSEFramer{isCodexClient: tc.isCodexClient}
			framer.WriteChunk(&output, []byte(tc.frame))
			framer.WriteChunk(&output, []byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"))
			got := output.String()
			if strings.Contains(got, "private") != tc.wantPrivate {
				t.Fatalf("private metadata presence = %v, want %v: %q", strings.Contains(got, "private"), tc.wantPrivate, got)
			}
			if !strings.Contains(got, "response.completed") {
				t.Fatalf("completion missing: %q", got)
			}
		})
	}
}

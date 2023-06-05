// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package datadogreceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/datadogreceiver"

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	"github.com/DataDog/datadog-agent/pkg/trace/pb"
	"github.com/davecgh/go-spew/spew"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	vmsgp "github.com/vmihailenco/msgpack/v4"
)

var data = [2]interface{}{
	0: []string{
		0:  "baggage",
		1:  "item",
		2:  "elasticsearch.version",
		3:  "7.0",
		4:  "my-name",
		5:  "X",
		6:  "my-service",
		7:  "my-resource",
		8:  "_dd.sampling_rate_whatever",
		9:  "value whatever",
		10: "sql",
		11: "service.name",
	},
	1: [][][12]interface{}{
		{
			{
				6,
				4,
				7,
				uint64(12345678901234561234),
				uint64(2),
				uint64(3),
				int64(123),
				int64(456),
				1,
				map[interface{}]interface{}{
					8:  9,
					0:  1,
					2:  3,
					11: 6,
				},
				map[interface{}]float64{
					5: 1.2,
				},
				10,
			},
		},
	},
}

func TestTranslateTracePayloadWithLogData(t *testing.T) {
	traces := pb.Traces{
		pb.Trace{
			&pb.Span{
				Service:  "tracey",
				Name:     "__main__.play_game",
				Resource: "__main__.play_game",
				TraceID:  15529312076525553051,
				SpanID:   15109084457746933574,
				ParentID: 0,
				Start:    1685944215735137485,
				Duration: 24043002534,
				Error:    0,
				Meta: map[string]string{
					"_dd.p.dm":              "-0",
					"language":              "python",
					"runtime-id":            "5e769a0c78b94796b1c4570ce51fbf3c",
					"_dd.agent_psr":         "1",
					"_dd.top_level":         "1",
					"_dd.tracer_kr":         "1",
					"_sampling_priority_v1": "1",
					"process_id":            "1",
					"span.kind":             "internal",
					"log":                   "{'timestamp': 1685944215, 'player': 'Mrs Jasmine Burgess', 'message': 'the ball is launched into the air', 'noise': 'thud'}",
				},
			},
		},
	}

	payload, err := vmsgp.Marshal(&data)
	assert.NoError(t, err)

	ddr := &datadogReceiver{}
	req, _ := http.NewRequest(http.MethodPost, "/v0.5/traces", io.NopCloser(bytes.NewReader(payload)))

	translated := ddr.toTraces(&pb.TracerPayload{
		LanguageName:    req.Header.Get("Datadog-Meta-Lang"),
		LanguageVersion: req.Header.Get("Datadog-Meta-Lang-Version"),
		TracerVersion:   req.Header.Get("Datadog-Meta-Tracer-Version"),
		Chunks:          traceChunksFromTraces(traces),
	}, req)

	spew.Dump(translated)
	assert.Equal(t, 1, translated.SpanCount(), "Span Count wrong")
	span := translated.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0)
	assert.NotNil(t, span)
	assert.Equal(t, 9, span.Attributes().Len(), "missing attributes")
	assert.Equal(t, "__main__.play_game", span.Name())

	events := translated.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0).Events()

	assert.Equal(t, 1, events.Len(), "Span Event Count wrong")

	events.At(0).Timestamp()
}

func TestTracePayloadV05Unmarshalling(t *testing.T) {
	var traces pb.Traces

	payload, err := vmsgp.Marshal(&data)
	assert.NoError(t, err)

	require.NoError(t, traces.UnmarshalMsgDictionary(payload), "Must not error when marshaling content")

	ddr := &datadogReceiver{}
	req, _ := http.NewRequest(http.MethodPost, "/v0.5/traces", io.NopCloser(bytes.NewReader(payload)))
	translated := ddr.toTraces(&pb.TracerPayload{
		LanguageName:    req.Header.Get("Datadog-Meta-Lang"),
		LanguageVersion: req.Header.Get("Datadog-Meta-Lang-Version"),
		TracerVersion:   req.Header.Get("Datadog-Meta-Tracer-Version"),
		Chunks:          traceChunksFromTraces(traces),
	}, req)
	assert.Equal(t, 1, translated.SpanCount(), "Span Count wrong")
	span := translated.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0)
	assert.NotNil(t, span)
	assert.Equal(t, 4, span.Attributes().Len(), "missing tags")
	value, exists := span.Attributes().Get("service.name")
	assert.True(t, exists, "service.name missing")
	assert.Equal(t, "my-service", value.AsString(), "service.name tag value incorrect")
	assert.Equal(t, "my-name", span.Name())
}

func TestTracePayloadV07Unmarshalling(t *testing.T) {
	var traces pb.Traces
	payload, err := vmsgp.Marshal(&data)
	assert.NoError(t, err)
	if err2 := traces.UnmarshalMsgDictionary(payload); err2 != nil {
		t.Fatal(err2)
	}
	apiPayload := pb.TracerPayload{
		LanguageName:    "1",
		LanguageVersion: "1",
		Chunks:          traceChunksFromTraces(traces),
		TracerVersion:   "1",
	}
	var reqBytes []byte
	bytez, _ := apiPayload.MarshalMsg(reqBytes)
	req, _ := http.NewRequest(http.MethodPost, "/v0.7/traces", io.NopCloser(bytes.NewReader(bytez)))

	translated, _ := handlePayload(req)
	span := translated.GetChunks()[0].GetSpans()[0]
	assert.NotNil(t, span)
	assert.Equal(t, 4, len(span.GetMeta()), "missing tags")
	value, exists := span.GetMeta()["service.name"]
	assert.True(t, exists, "service.name missing")
	assert.Equal(t, "my-service", value, "service.name tag value incorrect")
	assert.Equal(t, "my-name", span.GetName())
}

func BenchmarkTranslatorv05(b *testing.B) {
	b.StartTimer()
	for n := 0; n < b.N; n++ {
		TestTracePayloadV05Unmarshalling(&testing.T{})
	}
	b.StopTimer()
}

func BenchmarkTranslatorv07(b *testing.B) {
	b.StartTimer()
	for n := 0; n < b.N; n++ {
		TestTracePayloadV07Unmarshalling(&testing.T{})
	}
	b.StopTimer()
}

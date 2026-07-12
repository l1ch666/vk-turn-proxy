package baseline

import (
	"math"
	"testing"
)

func TestScenarioAndCaseValidation(t *testing.T) {
	valid := Scenario{Name: "turn-tcp-bond-n10", TURNTransport: "tcp", ProxyMode: "bond", Sessions: 10}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid scenario rejected: %v", err)
	}
	invalid := []Scenario{
		{Name: "../escape", TURNTransport: "tcp", ProxyMode: "bond", Sessions: 10},
		{Name: "test", TURNTransport: "quic", ProxyMode: "bond", Sessions: 10},
		{Name: "test", TURNTransport: "tcp", ProxyMode: "plain", Sessions: 10},
		{Name: "test", TURNTransport: "tcp", ProxyMode: "bond", Sessions: 0},
	}
	for _, scenario := range invalid {
		if err := scenario.Validate(); err == nil {
			t.Errorf("invalid scenario accepted: %+v", scenario)
		}
	}

	if err := (Case{Direction: DirectionDownload, Parallel: 8}).Validate(); err != nil {
		t.Fatalf("valid case rejected: %v", err)
	}
	if err := (Case{Direction: "sideways", Parallel: 1}).Validate(); err == nil {
		t.Fatal("invalid direction accepted")
	}
	if err := (Case{Direction: DirectionUpload, Parallel: 0}).Validate(); err == nil {
		t.Fatal("invalid parallel count accepted")
	}
}

func TestMedian(t *testing.T) {
	for _, test := range []struct {
		values []float64
		want   float64
	}{
		{[]float64{3, 1, 2}, 2},
		{[]float64{4, 1, 2, 3}, 2.5},
		{[]float64{0}, 0},
		{[]float64{math.MaxFloat64, math.MaxFloat64}, math.MaxFloat64},
	} {
		got, err := Median(test.values)
		if err != nil || got != test.want {
			t.Errorf("Median(%v) = %v, %v; want %v", test.values, got, err, test.want)
		}
	}
	for _, values := range [][]float64{nil, {-1}, {math.Inf(1)}, {math.NaN()}} {
		if _, err := Median(values); err == nil {
			t.Errorf("Median(%v) unexpectedly succeeded", values)
		}
	}
}

func TestSummarizeRequiresThreeRunsAndUsesReceiverRate(t *testing.T) {
	upload := Case{Direction: DirectionUpload, Parallel: 1}
	download := Case{Direction: DirectionDownload, Parallel: 8}
	measurements := []Measurement{
		{Case: upload, IPerf: IPerfSummary{BitsPerSecond: 100, RetransmittedSegments: 9}},
		{Case: upload, IPerf: IPerfSummary{BitsPerSecond: 300, RetransmittedSegments: 3}},
		{Case: upload, IPerf: IPerfSummary{BitsPerSecond: 200, RetransmittedSegments: 6}},
		{Case: download, IPerf: IPerfSummary{BitsPerSecond: 600, RetransmittedSegments: 12}},
		{Case: download, IPerf: IPerfSummary{BitsPerSecond: 400, RetransmittedSegments: 18}},
		{Case: download, IPerf: IPerfSummary{BitsPerSecond: 500, RetransmittedSegments: 15}},
	}
	summaries, err := Summarize(measurements)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if len(summaries) != 2 || summaries[0].Case != upload || summaries[1].Case != download {
		t.Fatalf("summary order = %+v", summaries)
	}
	if summaries[0].MedianBitsPerSecond != 200 || summaries[0].MinimumBitsPerSecond != 100 ||
		summaries[0].MaximumBitsPerSecond != 300 || summaries[0].MedianRetransmittedSegments != 6 {
		t.Fatalf("upload summary = %+v", summaries[0])
	}
	if _, err := Summarize(measurements[:2]); err == nil {
		t.Fatal("two-run summary unexpectedly succeeded")
	}
}

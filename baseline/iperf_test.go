package baseline

import (
	"strings"
	"testing"
)

func TestParseIPerfJSON(t *testing.T) {
	raw := `{
  "start": {
    "version": "iperf 3.17.1",
    "system_info": "test-os",
    "test_start": {"protocol":"TCP","num_streams":8,"duration":20,"reverse":0}
  },
  "end": {
    "sum_sent": {"seconds":20.01,"bytes":250000000,"bits_per_second":99900000,"retransmits":17},
    "sum_received": {"seconds":20.00,"bytes":249000000,"bits_per_second":99600000},
    "cpu_utilization_percent": {"host_total":12.5,"remote_total":20.25},
    "sender_tcp_congestion":"cubic"
  }
}`
	got, err := ParseIPerfJSON([]byte(raw))
	if err != nil {
		t.Fatalf("ParseIPerfJSON: %v", err)
	}
	if got.Version != "iperf 3.17.1" || got.System != "test-os" || got.Protocol != "TCP" ||
		got.RequestedStreams != 8 || got.Reverse || got.Seconds != 20 ||
		got.SentBytes != 250000000 || got.ReceivedBytes != 249000000 ||
		got.SenderBitsPerSecond != 99900000 || got.BitsPerSecond != 99600000 ||
		got.RetransmittedSegments != 17 || got.HostCPUPercent != 12.5 || got.RemoteCPUPercent != 20.25 ||
		got.SenderTCPCongestionControl != "cubic" {
		t.Fatalf("parsed summary = %+v", got)
	}
}

func TestParseIPerfJSONAcceptsBooleanReverseAndOneSum(t *testing.T) {
	raw := `{"start":{"test_start":{"protocol":"TCP","num_streams":1,"reverse":true}},"end":{"sum_received":{"seconds":1,"bytes":10,"bits_per_second":80}}}`
	got, err := ParseIPerfJSON([]byte(raw))
	if err != nil {
		t.Fatalf("ParseIPerfJSON: %v", err)
	}
	if !got.Reverse || got.SentBytes != 10 || got.ReceivedBytes != 10 || got.BitsPerSecond != 80 {
		t.Fatalf("parsed fallback summary = %+v", got)
	}
}

func TestParseIPerfJSONRejectsErrorsAndMalformedResults(t *testing.T) {
	invalid := []string{
		``,
		`{"error":"unable to connect"}`,
		`{"start":{},"end":{}}`,
		`{"end":{"sum_received":{"seconds":0,"bits_per_second":1}}}`,
		`{"end":{"sum_received":{"seconds":1,"bits_per_second":1}}} {}`,
		`{"start":{"test_start":{"reverse":2}},"end":{"sum_received":{"seconds":1,"bits_per_second":1}}}`,
	}
	for _, raw := range invalid {
		if _, err := ParseIPerfJSON([]byte(raw)); err == nil {
			t.Errorf("invalid iperf JSON accepted: %s", strings.TrimSpace(raw))
		}
	}
}

package baseline

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
)

type IPerfSummary struct {
	Version                    string  `json:"version"`
	System                     string  `json:"system"`
	Protocol                   string  `json:"protocol"`
	RequestedStreams           int     `json:"requested_streams"`
	Reverse                    bool    `json:"reverse"`
	Seconds                    float64 `json:"seconds"`
	SentBytes                  uint64  `json:"sent_bytes"`
	ReceivedBytes              uint64  `json:"received_bytes"`
	SenderBitsPerSecond        float64 `json:"sender_bits_per_second"`
	BitsPerSecond              float64 `json:"bits_per_second"`
	RetransmittedSegments      uint64  `json:"retransmitted_segments"`
	HostCPUPercent             float64 `json:"host_cpu_percent"`
	RemoteCPUPercent           float64 `json:"remote_cpu_percent"`
	SenderTCPCongestionControl string  `json:"sender_tcp_congestion_control,omitempty"`
}

type boolish bool

func (b *boolish) UnmarshalJSON(data []byte) error {
	switch string(bytes.TrimSpace(data)) {
	case "true", "1":
		*b = true
		return nil
	case "false", "0":
		*b = false
		return nil
	default:
		return fmt.Errorf("reverse must be a boolean or 0/1")
	}
}

type iperfSum struct {
	Seconds       float64 `json:"seconds"`
	Bytes         uint64  `json:"bytes"`
	BitsPerSecond float64 `json:"bits_per_second"`
	Retransmits   uint64  `json:"retransmits"`
}

type iperfDocument struct {
	Error string `json:"error"`
	Start struct {
		Version    string `json:"version"`
		SystemInfo string `json:"system_info"`
		TestStart  struct {
			Protocol   string  `json:"protocol"`
			NumStreams int     `json:"num_streams"`
			Duration   float64 `json:"duration"`
			Reverse    boolish `json:"reverse"`
		} `json:"test_start"`
	} `json:"start"`
	End struct {
		SumSent     *iperfSum `json:"sum_sent"`
		SumReceived *iperfSum `json:"sum_received"`
		CPU         struct {
			HostTotal   float64 `json:"host_total"`
			RemoteTotal float64 `json:"remote_total"`
		} `json:"cpu_utilization_percent"`
		SenderTCPCongestion string `json:"sender_tcp_congestion"`
	} `json:"end"`
}

func ParseIPerfJSON(data []byte) (IPerfSummary, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var document iperfDocument
	if err := decoder.Decode(&document); err != nil {
		return IPerfSummary{}, fmt.Errorf("decode iperf3 JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return IPerfSummary{}, fmt.Errorf("decode iperf3 JSON: unexpected trailing value")
		}
		return IPerfSummary{}, fmt.Errorf("decode iperf3 JSON trailer: %w", err)
	}
	if document.Error != "" {
		return IPerfSummary{}, fmt.Errorf("iperf3 reported: %s", document.Error)
	}
	if document.End.SumSent == nil && document.End.SumReceived == nil {
		return IPerfSummary{}, fmt.Errorf("iperf3 JSON has no TCP sum_sent or sum_received result")
	}

	sent := document.End.SumSent
	if sent == nil {
		sent = document.End.SumReceived
	}
	received := document.End.SumReceived
	if received == nil {
		received = document.End.SumSent
	}
	if received.Seconds <= 0 || !validRate(received.BitsPerSecond) || !validRate(sent.BitsPerSecond) {
		return IPerfSummary{}, fmt.Errorf("iperf3 JSON contains invalid duration or bitrate")
	}
	if !validRate(document.End.CPU.HostTotal) || !validRate(document.End.CPU.RemoteTotal) {
		return IPerfSummary{}, fmt.Errorf("iperf3 JSON contains invalid CPU utilization")
	}

	return IPerfSummary{
		Version:                    document.Start.Version,
		System:                     document.Start.SystemInfo,
		Protocol:                   document.Start.TestStart.Protocol,
		RequestedStreams:           document.Start.TestStart.NumStreams,
		Reverse:                    bool(document.Start.TestStart.Reverse),
		Seconds:                    received.Seconds,
		SentBytes:                  sent.Bytes,
		ReceivedBytes:              received.Bytes,
		SenderBitsPerSecond:        sent.BitsPerSecond,
		BitsPerSecond:              received.BitsPerSecond,
		RetransmittedSegments:      sent.Retransmits,
		HostCPUPercent:             document.End.CPU.HostTotal,
		RemoteCPUPercent:           document.End.CPU.RemoteTotal,
		SenderTCPCongestionControl: document.End.SenderTCPCongestion,
	}, nil
}

func validRate(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

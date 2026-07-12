package baseline

import (
	"fmt"
	"math"
	"regexp"
	"sort"
)

const MinimumRuns = 3

type Direction string

const (
	DirectionUpload   Direction = "upload"
	DirectionDownload Direction = "download"
)

// Scenario records the proxy configuration that is held constant for one
// benchmark invocation. TURN transport describes the proxy's -udp flag, not
// the iperf payload protocol: iperf always carries TCP through VLESS here.
type Scenario struct {
	Name          string `json:"name"`
	TURNTransport string `json:"turn_transport"`
	ProxyMode     string `json:"proxy_mode"`
	Sessions      int    `json:"sessions"`
}

var scenarioNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func (s Scenario) Validate() error {
	if !scenarioNamePattern.MatchString(s.Name) {
		return fmt.Errorf("scenario name must match %s", scenarioNamePattern)
	}
	if s.TURNTransport != "tcp" && s.TURNTransport != "udp" {
		return fmt.Errorf("TURN transport must be tcp or udp")
	}
	if s.ProxyMode != "multi-session" && s.ProxyMode != "bond" {
		return fmt.Errorf("proxy mode must be multi-session or bond")
	}
	if s.Sessions < 1 || s.Sessions > 64 {
		return fmt.Errorf("session count must be in 1..64")
	}
	return nil
}

type Case struct {
	Direction Direction `json:"direction"`
	Parallel  int       `json:"parallel"`
}

func (c Case) Validate() error {
	if c.Direction != DirectionUpload && c.Direction != DirectionDownload {
		return fmt.Errorf("direction must be upload or download")
	}
	if c.Parallel < 1 || c.Parallel > 128 {
		return fmt.Errorf("parallel flow count must be in 1..128")
	}
	return nil
}

func (c Case) Slug() string {
	return fmt.Sprintf("%s-p%d", c.Direction, c.Parallel)
}

type Measurement struct {
	Case  Case         `json:"case"`
	IPerf IPerfSummary `json:"iperf"`
}

type CaseSummary struct {
	Case                        Case    `json:"case"`
	Runs                        int     `json:"runs"`
	MedianBitsPerSecond         float64 `json:"median_bits_per_second"`
	MinimumBitsPerSecond        float64 `json:"minimum_bits_per_second"`
	MaximumBitsPerSecond        float64 `json:"maximum_bits_per_second"`
	MedianRetransmittedSegments float64 `json:"median_retransmitted_segments"`
}

func Summarize(measurements []Measurement) ([]CaseSummary, error) {
	groups := make(map[Case][]Measurement)
	for _, measurement := range measurements {
		if err := measurement.Case.Validate(); err != nil {
			return nil, fmt.Errorf("invalid measurement case: %w", err)
		}
		groups[measurement.Case] = append(groups[measurement.Case], measurement)
	}
	if len(groups) == 0 {
		return nil, fmt.Errorf("no benchmark measurements")
	}

	cases := make([]Case, 0, len(groups))
	for benchmarkCase := range groups {
		cases = append(cases, benchmarkCase)
	}
	sort.Slice(cases, func(i, j int) bool {
		if cases[i].Direction != cases[j].Direction {
			return directionOrder(cases[i].Direction) < directionOrder(cases[j].Direction)
		}
		return cases[i].Parallel < cases[j].Parallel
	})

	summaries := make([]CaseSummary, 0, len(cases))
	for _, benchmarkCase := range cases {
		runs := groups[benchmarkCase]
		if len(runs) < MinimumRuns {
			return nil, fmt.Errorf("case %s has %d runs; at least %d are required", benchmarkCase.Slug(), len(runs), MinimumRuns)
		}
		throughputs := make([]float64, len(runs))
		retransmits := make([]float64, len(runs))
		for i, run := range runs {
			throughputs[i] = run.IPerf.BitsPerSecond
			retransmits[i] = float64(run.IPerf.RetransmittedSegments)
		}
		medianThroughput, err := Median(throughputs)
		if err != nil {
			return nil, fmt.Errorf("case %s throughput: %w", benchmarkCase.Slug(), err)
		}
		medianRetransmits, err := Median(retransmits)
		if err != nil {
			return nil, fmt.Errorf("case %s retransmits: %w", benchmarkCase.Slug(), err)
		}
		sort.Float64s(throughputs)
		summaries = append(summaries, CaseSummary{
			Case:                        benchmarkCase,
			Runs:                        len(runs),
			MedianBitsPerSecond:         medianThroughput,
			MinimumBitsPerSecond:        throughputs[0],
			MaximumBitsPerSecond:        throughputs[len(throughputs)-1],
			MedianRetransmittedSegments: medianRetransmits,
		})
	}
	return summaries, nil
}

func Median(values []float64) (float64, error) {
	if len(values) == 0 {
		return 0, fmt.Errorf("cannot calculate median of an empty sample")
	}
	ordered := append([]float64(nil), values...)
	for _, value := range ordered {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return 0, fmt.Errorf("sample contains invalid value %v", value)
		}
	}
	sort.Float64s(ordered)
	middle := len(ordered) / 2
	if len(ordered)%2 == 1 {
		return ordered[middle], nil
	}
	lower := ordered[middle-1]
	return lower + (ordered[middle]-lower)/2, nil
}

func directionOrder(direction Direction) int {
	if direction == DirectionUpload {
		return 0
	}
	return 1
}

package main

import (
	"log/slog"
	"maps"
	"net/http"
	"sync"

	"helix-gateway-local/internal/truncationfix"
)

type gatewayStats struct {
	mu     sync.Mutex
	totals statsSnapshot
}

type statsSnapshot struct {
	Requests                uint64            `json:"requests"`
	Interventions           uint64            `json:"interventions"`
	SyntheticRequests       uint64            `json:"synthetic_requests"`
	RepairOnlyRequests      uint64            `json:"repair_only_requests"`
	RecoveredBytes          uint64            `json:"recovered_bytes"`
	LengthDecisions         uint64            `json:"length_decisions"`
	UpstreamLengthDecisions uint64            `json:"upstream_length_decisions"`
	GatewayLengthDecisions  uint64            `json:"gateway_length_decisions"`
	GuardTrips              uint64            `json:"guard_trips"`
	StreamMissingDone       uint64            `json:"stream_missing_done"`
	StructuralRepairs       uint64            `json:"structural_repairs"`
	FallbackReasons         map[string]uint64 `json:"fallback_reasons"`
}

func (s *gatewayStats) record(fix *truncationfix.Request, bypass string) {
	var report truncationfix.Report
	mode := "passthrough"
	if fix != nil {
		report = fix.Report()
		mode = "repair-only"
		if fix.Synthetic() {
			mode = "synthetic"
		}
	} else if bypass != "" {
		report.Reasons = map[string]uint64{bypass: 1}
		if bypass == "request_limit" {
			report.GuardTrips++
		}
	}
	s.mu.Lock()
	s.totals.Requests++
	if fix != nil {
		s.totals.Interventions++
		if fix.Synthetic() {
			s.totals.SyntheticRequests++
		} else {
			s.totals.RepairOnlyRequests++
		}
	}
	s.totals.RecoveredBytes += report.RecoveredBytes
	s.totals.UpstreamLengthDecisions += report.UpstreamLength
	s.totals.GatewayLengthDecisions += report.GatewayLength
	s.totals.GuardTrips += report.GuardTrips
	s.totals.StreamMissingDone += report.MissingDone
	s.totals.StructuralRepairs += report.Repairs
	if s.totals.FallbackReasons == nil {
		s.totals.FallbackReasons = make(map[string]uint64)
	}
	for reason, count := range report.Reasons {
		s.totals.FallbackReasons[reason] += count
	}
	s.mu.Unlock()
	if bypass != "disabled" {
		slog.Info("chat completion processed", "mode", mode, "recovered_bytes", report.RecoveredBytes,
			"length_decisions", report.UpstreamLength+report.GatewayLength,
			"upstream_length_decisions", report.UpstreamLength, "gateway_length_decisions", report.GatewayLength,
			"guard_trips", report.GuardTrips, "stream_missing_done", report.MissingDone,
			"structural_repairs", report.Repairs, "fallback_reasons", report.Reasons)
	}
}

func (s *gatewayStats) serve(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	snapshot := s.totals
	snapshot.LengthDecisions = snapshot.UpstreamLengthDecisions + snapshot.GatewayLengthDecisions
	snapshot.FallbackReasons = maps.Clone(snapshot.FallbackReasons)
	s.mu.Unlock()
	if snapshot.FallbackReasons == nil {
		snapshot.FallbackReasons = map[string]uint64{}
	}
	writeJSON(w, http.StatusOK, snapshot)
}

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
	models map[string]modelStats
}

type modelStats struct {
	Requests                uint64  `json:"requests"`
	UpstreamCalls           uint64  `json:"upstream_calls"`
	UpstreamCallsPerRequest float64 `json:"upstream_calls_per_request"`
	RecoveredBytes          uint64  `json:"recovered_bytes"`
	DeduplicatedBytes       uint64  `json:"deduplicated_bytes"`
	ToolHits                uint64  `json:"tool_hits"`
	ToolUnused              uint64  `json:"tool_unused"`
	LengthDecisions         uint64  `json:"length_decisions"`
	ObservedRequests        uint64  `json:"observed_requests"`
	UpstreamLengthRequests  uint64  `json:"upstream_length_requests"`
	UpstreamLengthRate      float64 `json:"upstream_length_rate"`
	UpstreamLengthDecisions uint64  `json:"upstream_length_decisions"`
	GatewayLengthDecisions  uint64  `json:"gateway_length_decisions"`
	GuardTrips              uint64  `json:"guard_trips"`
	ClientCapSuspected      uint64  `json:"client_cap_suspected"`
	TokenCapIncreases       uint64  `json:"token_cap_increases"`
	ContinuationHops        uint64  `json:"continuation_hops"`
	SyntheticRequests       uint64  `json:"synthetic_requests"`
	RepairOnlyRequests      uint64  `json:"repair_only_requests"`
}

func (s modelStats) withRates() modelStats {
	if s.Requests != 0 {
		s.UpstreamCallsPerRequest = float64(s.UpstreamCalls) / float64(s.Requests)
	}
	if s.ObservedRequests != 0 {
		s.UpstreamLengthRate = float64(s.UpstreamLengthRequests) / float64(s.ObservedRequests)
	}
	return s
}

func (s *modelStats) record(state *chatState, report truncationfix.Report, clientCapSuspected bool) {
	s.Requests++
	s.UpstreamCalls += state.upstreamCalls
	s.ContinuationHops += state.continuationHops
	if state.fix != nil {
		if state.fix.Synthetic() {
			s.SyntheticRequests++
		} else {
			s.RepairOnlyRequests++
		}
	}
	s.RecoveredBytes += report.RecoveredBytes
	s.DeduplicatedBytes += report.DeduplicatedBytes
	s.ToolHits += report.ToolHits
	s.ToolUnused += report.Reasons["tool_unused"]
	s.UpstreamLengthDecisions += report.UpstreamLength
	s.GatewayLengthDecisions += report.GatewayLength
	s.LengthDecisions += report.UpstreamLength + report.GatewayLength
	s.GuardTrips += report.GuardTrips
	if state.observed {
		s.ObservedRequests++
		if report.UpstreamLength > 0 {
			s.UpstreamLengthRequests++
		}
	}
	if clientCapSuspected {
		s.ClientCapSuspected++
	}
	if state.upstreamCalls > 0 && state.sentCap > state.clientCap {
		s.TokenCapIncreases++
	}
}

type statsSnapshot struct {
	modelStats
	Interventions     uint64            `json:"interventions"`
	StreamMissingDone uint64            `json:"stream_missing_done"`
	StructuralRepairs uint64            `json:"structural_repairs"`
	FallbackReasons   map[string]uint64 `json:"fallback_reasons"`
}

func (s *gatewayStats) record(state *chatState, bypass string) {
	fix := state.fix
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
	clientCapSuspected := state.observed && report.UpstreamLength > 0 && state.clientCap > 0 &&
		state.clientCap <= 512 && state.sentCap == state.clientCap
	s.mu.Lock()
	s.totals.modelStats.record(state, report, clientCapSuspected)
	if state.model != "" && len(state.model) <= 256 {
		if s.models == nil {
			s.models = make(map[string]modelStats)
		}
		if model, exists := s.models[state.model]; exists || len(s.models) < 256 {
			model.record(state, report, clientCapSuspected)
			s.models[state.model] = model
		}
	}
	if fix != nil {
		s.totals.Interventions++
	}
	s.totals.StreamMissingDone += report.MissingDone
	s.totals.StructuralRepairs += report.Repairs
	if s.totals.FallbackReasons == nil {
		s.totals.FallbackReasons = make(map[string]uint64)
	}
	for reason, count := range report.Reasons {
		s.totals.FallbackReasons[reason] += count
	}
	s.mu.Unlock()
	if clientCapSuspected {
		slog.Info("chat completion truncated at client cap", "model", state.model, "max_tokens", state.clientCap)
	}
	if bypass != "disabled" {
		slog.Info("chat completion processed", "mode", mode, "model", state.model,
			"upstream_calls", state.upstreamCalls, "continuation_hops", state.continuationHops,
			"client_cap", state.clientCap, "sent_cap", state.sentCap,
			"recovered_bytes", report.RecoveredBytes, "deduplicated_bytes", report.DeduplicatedBytes, "tool_hits", report.ToolHits,
			"length_decisions", report.UpstreamLength+report.GatewayLength,
			"upstream_length_decisions", report.UpstreamLength, "gateway_length_decisions", report.GatewayLength,
			"guard_trips", report.GuardTrips, "stream_missing_done", report.MissingDone,
			"structural_repairs", report.Repairs, "fallback_reasons", report.Reasons)
	}
}

func (s *gatewayStats) serve(w http.ResponseWriter, r *http.Request) {
	byModel := r.URL.Query().Get("by") == "model"
	var models map[string]modelStats
	s.mu.Lock()
	snapshot := s.totals
	snapshot.FallbackReasons = maps.Clone(snapshot.FallbackReasons)
	if byModel {
		models = maps.Clone(s.models)
	}
	s.mu.Unlock()
	snapshot.modelStats = snapshot.modelStats.withRates()
	if snapshot.FallbackReasons == nil {
		snapshot.FallbackReasons = map[string]uint64{}
	}
	if byModel {
		if models == nil {
			models = map[string]modelStats{}
		}
		for name, model := range models {
			models[name] = model.withRates()
		}
		writeJSON(w, http.StatusOK, struct {
			statsSnapshot
			ByModel map[string]modelStats `json:"by_model"`
		}{snapshot, models})
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

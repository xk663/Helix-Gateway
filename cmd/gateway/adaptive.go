package main

import (
	"math/bits"
	"sync"
)

type adaptivePolicy struct {
	mu     sync.Mutex
	models map[retryKey]adaptiveModel
}

type adaptiveModel struct {
	lengths   uint8
	misses    uint8
	synthetic bool
	disabled  bool
}

func (p *adaptivePolicy) synthetic(key retryKey) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.models[key].synthetic
}

// Only complete responses contribute. Three length decisions in the last
// eight usable responses are sufficient to try the synthetic channel.
func (p *adaptivePolicy) observe(key retryKey, synthetic, length, unused bool) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	model, exists := p.models[key]
	if model.disabled || model.synthetic != synthetic {
		return ""
	}
	if !exists && len(p.models) >= policyEntries {
		for old := range p.models {
			delete(p.models, old)
			break
		}
	}
	event := ""
	if synthetic {
		if unused {
			model.misses++
		} else {
			model.misses = 0
		}
		if model.misses == 3 {
			model.synthetic, model.disabled = false, true
			event = "synthetic_disabled_after_misses"
		}
	} else {
		model.lengths <<= 1
		if length {
			model.lengths |= 1
		}
		if bits.OnesCount8(model.lengths) >= 3 {
			model.synthetic = true
			event = "synthetic_enabled_after_length"
		}
	}
	p.models[key] = model
	return event
}

package truncationfix

import (
	"os"
	"path"
	"strconv"
	"strings"
)

// Config controls transport wrapping on the OpenAI Chat Completions API.
type Config struct {
	Enabled                bool
	AssumeNoTools          bool
	AutoToolChoice         bool
	WrapperMode            string
	DisabledModels         []string
	MaxSyntheticToolLength int
	StreamBufferSize       int
	RepairLevel            string
}

func (cfg Config) Resolve() Config {
	cfg.Enabled = env("ENABLED", cfg.Enabled, strconv.ParseBool)
	cfg.AssumeNoTools = env("ASSUME_NO_TOOLS", cfg.AssumeNoTools, strconv.ParseBool)
	if forced, ok := os.LookupEnv("TRUNCATIONFIX_FORCE_TOOL_CHOICE"); ok {
		if value, err := strconv.ParseBool(strings.TrimSpace(forced)); err == nil {
			cfg.AutoToolChoice = !value
		}
	}
	cfg.WrapperMode = env("WRAPPER_MODE", cfg.WrapperMode, identity)
	cfg.RepairLevel = env("REPAIR_LEVEL", cfg.RepairLevel, identity)
	cfg.DisabledModels = env("DISABLED_MODELS", cfg.DisabledModels, splitModels)
	cfg.MaxSyntheticToolLength = env("MAX_LENGTH", cfg.MaxSyntheticToolLength, strconv.Atoi)
	cfg.StreamBufferSize = env("BUFFER_SIZE", cfg.StreamBufferSize, strconv.Atoi)
	switch cfg.WrapperMode {
	case "":
		cfg.WrapperMode = "synthetic"
	case "adaptive", "synthetic", "repair-only", "aggressive", "off":
	default:
		cfg.WrapperMode = "off"
	}
	cfg.Enabled = cfg.Enabled && cfg.WrapperMode != "off"
	switch cfg.RepairLevel {
	case "conservative", "moderate", "aggressive":
	case "":
		if cfg.WrapperMode == "aggressive" {
			cfg.RepairLevel = "aggressive"
		} else {
			cfg.RepairLevel = "conservative"
		}
	default:
		cfg.RepairLevel = "conservative"
	}
	if cfg.MaxSyntheticToolLength <= 0 {
		cfg.MaxSyntheticToolLength = 65536
	}
	cfg.MaxSyntheticToolLength = min(cfg.MaxSyntheticToolLength, 4<<20)
	if cfg.StreamBufferSize <= 0 {
		cfg.StreamBufferSize = 4096
	}
	cfg.StreamBufferSize = min(cfg.StreamBufferSize, 64<<10)
	return cfg
}

func env[T any](key string, fallback T, parse func(string) (T, error)) T {
	if raw, exists := os.LookupEnv("TRUNCATIONFIX_" + key); exists {
		if value, err := parse(strings.TrimSpace(raw)); err == nil {
			return value
		}
	}
	return fallback
}

func identity(value string) (string, error) { return value, nil }

func splitModels(value string) ([]string, error) {
	return strings.Split(value, ","), nil
}

func matches(patterns []string, fullName, shortName string) bool {
	for _, pattern := range patterns {
		pattern = strings.ToLower(strings.TrimSpace(pattern))
		if full, _ := path.Match(pattern, fullName); full {
			return true
		}
		if short, _ := path.Match(pattern, shortName); short {
			return true
		}
	}
	return false
}

func (cfg Config) wraps(model string) bool {
	name := strings.ToLower(strings.TrimSpace(model))
	short := name[strings.LastIndexByte(name, '/')+1:]
	bare := strings.TrimPrefix(short, "agy-")
	family := bare
	if _, after, ok := strings.Cut(bare, "anthropic."); ok && strings.HasPrefix(after, "claude") {
		family, _, _ = strings.Cut(after, ":")
	}
	family = strings.Replace(family, "claude.", "claude-", 1)
	if short == "" || matches(cfg.DisabledModels, name, short) ||
		matches(cfg.DisabledModels, bare, family) {
		return false
	}
	return !matches(nonTextModels, bare, family)
}

var nonTextModels = []string{
	"*embedding*", "*-embed*", "*rerank*", "dall-e*", "whisper*", "tts*", "gpt-image*",
	"audio-*", "image-*", "*-audio-*", "*-realtime*", "*-tts*", "*-speech*", "*-image",
	"sora-*", "veo-*", "*-video", "*-moderation*", "*-image-*",
	"gpt-3.5-*instruct*", "text-davinci-*", "babbage-002", "davinci-002",
}

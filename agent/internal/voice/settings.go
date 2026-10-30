// Package voice resolves a per-call snapshot of speech-provider settings.
package voice

import (
	"fmt"
	"math"
	"net/url"
	"os"
	"phoneagent/internal/secrets"
	"strconv"
	"strings"
)

type Settings struct {
	STTProvider, TTSProvider                       string
	STTURL, TTSURL                                 string
	APIKey                                         string `json:"-"`
	ServiceAPIKey                                  string `json:"-"`
	BaseURL, Language, STTModel, TTSModel, VoiceID string
	ServiceSTTModel, ServiceTTSModel, ServiceVoice string
	AutoLanguage                                   bool
	// Nil preserves the provider's stored voice settings; zero is a valid override.
	Stability *float64
}

func Load(raw map[string]string) (Settings, error) {
	value := func(key, env, fallback string) string {
		if v, ok := raw[key]; ok {
			return strings.TrimSpace(v)
		}
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return v
		}
		return fallback
	}
	s := Settings{
		STTProvider:     value("voice_stt_provider", "STT_PROVIDER", "elevenlabs"),
		TTSProvider:     value("voice_tts_provider", "TTS_PROVIDER", "elevenlabs"),
		STTURL:          value("voice_stt_url", "STT_URL", "http://stt:9000"),
		TTSURL:          value("voice_tts_url", "TTS_URL", "http://tts:8000/synthesize"),
		BaseURL:         value("voice_base_url", "ELEVENLABS_BASE_URL", "https://api.elevenlabs.io/v1"),
		APIKey:          value("voice_api_key", "ELEVENLABS_API_KEY", ""),
		Language:        value("voice_language", "ELEVENLABS_LANGUAGE", "en"),
		AutoLanguage:    value("voice_auto_language", "VOICE_AUTO_LANGUAGE", "true") != "false",
		STTModel:        value("voice_stt_model", "ELEVENLABS_STT_MODEL", "scribe_v2_realtime"),
		TTSModel:        value("voice_tts_model", "ELEVENLABS_TTS_MODEL", "eleven_flash_v2_5"),
		VoiceID:         value("voice_id", "ELEVENLABS_VOICE_ID", ""),
		ServiceAPIKey:   value("voice_service_api_key", "SPEECH_SERVICE_API_KEY", ""),
		ServiceSTTModel: value("voice_service_stt_model", "STT_MODEL", ""),
		ServiceTTSModel: value("voice_service_tts_model", "TTS_MODEL", ""),
		ServiceVoice:    value("voice_service_voice", "TTS_VOICE", ""),
	}
	if rawStability := value("voice_stability", "ELEVENLABS_STABILITY", ""); rawStability != "" {
		v, err := strconv.ParseFloat(rawStability, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
			return s, fmt.Errorf("voice stability must be between 0 and 1")
		}
		s.Stability = &v
	}
	if u, err := url.Parse(s.TTSURL); s.TTSProvider == "service" && err == nil && (u.Path == "" || u.Path == "/") {
		s.TTSURL = strings.TrimRight(s.TTSURL, "/") + "/synthesize"
	}
	s.BaseURL = strings.TrimRight(s.BaseURL, "/")
	validProvider := func(p string) bool { return p == "service" || p == "elevenlabs" || p == "openai" }
	if !validProvider(s.STTProvider) || !validProvider(s.TTSProvider) {
		return s, fmt.Errorf("unsupported speech provider")
	}
	for _, api := range []struct{ provider, endpoint string }{{s.STTProvider, s.STTURL}, {s.TTSProvider, s.TTSURL}} {
		if api.provider == "elevenlabs" {
			continue
		}
		u, err := url.Parse(api.endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return s, fmt.Errorf("speech API URLs must use HTTP or HTTPS without embedded credentials")
		}
	}
	if s.STTProvider == "openai" || s.TTSProvider == "openai" {
		key, err := secrets.Decrypt(s.ServiceAPIKey, "voice_service_api_key")
		if err != nil {
			return s, err
		}
		s.ServiceAPIKey = key
	}
	if s.STTProvider == "elevenlabs" || s.TTSProvider == "elevenlabs" {
		key, err := secrets.Decrypt(s.APIKey, "voice_api_key")
		if err != nil {
			return s, err
		}
		s.APIKey = key
		if key == "" {
			return s, fmt.Errorf("ElevenLabs API key is missing")
		}
	}
	// Preserve legacy batch deployments after the provider retired Scribe v1.
	if s.STTProvider == "elevenlabs" && s.STTModel == "scribe_v1" {
		s.STTModel = "scribe_v2"
	}
	if s.TTSProvider == "elevenlabs" && s.VoiceID == "" {
		return s, fmt.Errorf("ElevenLabs voice is missing")
	}
	return s, nil
}

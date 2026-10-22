package voice

import "testing"

func TestLegacyDefaultsAndPerCallOverrides(t *testing.T) {
	t.Setenv("STT_PROVIDER", "service")
	t.Setenv("TTS_PROVIDER", "service")
	t.Setenv("TTS_URL", "http://legacy:8000")
	t.Setenv("ELEVENLABS_API_KEY", "env-fixture")
	s, err := Load(nil)
	if err != nil || s.STTProvider != "service" || s.TTSProvider != "service" || s.TTSURL != "http://legacy:8000/synthesize" {
		t.Fatalf("legacy changed: %v", err)
	}
	raw := map[string]string{"voice_stt_provider": "elevenlabs", "voice_stt_model": "scribe_v2_realtime", "voice_language": "en"}
	s, err = Load(raw)
	if err != nil || s.APIKey != "env-fixture" || s.Language != "en" {
		t.Fatalf("override failed: %v", err)
	}
	raw["voice_language"] = "fr"
	if s.Language != "en" {
		t.Fatal("active call changed with settings")
	}
	raw["voice_stt_model"] = "scribe_v1"
	legacy, err := Load(raw)
	if err != nil || legacy.STTModel != "scribe_v2" {
		t.Fatal("retired batch alias was not migrated")
	}
	raw["voice_api_key"] = ""
	if _, err := Load(raw); err == nil {
		t.Fatal("cleared key resurrected env credential")
	}
}

func TestVoiceStabilityOverrideAndValidation(t *testing.T) {
	t.Setenv("ELEVENLABS_STABILITY", "")
	for _, value := range []string{"", "0", "0.65", "1"} {
		s, err := Load(map[string]string{"voice_stability": value, "voice_stt_provider": "service", "voice_tts_provider": "service"})
		if err != nil {
			t.Fatal(err)
		}
		if (s.Stability == nil) != (value == "") {
			t.Fatalf("lost override %q", value)
		}
	}
	for _, value := range []string{"NaN", "Inf", "-0.1", "1.1", "invalid"} {
		if _, err := Load(map[string]string{"voice_stability": value}); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
}

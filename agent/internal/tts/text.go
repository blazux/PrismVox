package tts

import (
	"os"
	"phoneagent/internal/locale"
	"regexp"
	"strings"
	"unicode"
)

var letters = []string{"a", "bé", "cé", "dé", "e", "effe", "gé", "ache", "i", "ji", "ka", "elle", "emme", "enne", "o", "pé", "ku", "erre", "esse", "té", "u", "vé", "double vé", "ixe", "i grec", "zède"}
var wordToken = regexp.MustCompile(`[\pL\pN_]+`)

func (c *Client) prepareText(text string) string {
	code := c.settings.Language
	if c.settings.AutoLanguage {
		code = locale.Detect(text)
	}
	if locale.Normalize(code) != "fr" {
		return text
	}
	split := func(s string) []string { return strings.Fields(strings.ReplaceAll(s, ",", " ")) }
	words := c.SpellWords
	if words == "" {
		words = os.Getenv("TTS_SPELL_WORDS")
	}
	exceptions := c.SpellExceptions
	if exceptions == "" {
		exceptions = os.Getenv("TTS_SPELL_EXCEPTIONS")
		if exceptions == "" {
			exceptions = "OK"
		}
	}
	force, skip := map[string]bool{}, map[string]bool{}
	for _, w := range split(words) {
		force[strings.ToUpper(w)] = true
	}
	for _, w := range split(exceptions) {
		skip[strings.ToUpper(w)] = true
	}
	return wordToken.ReplaceAllStringFunc(text, func(w string) string {
		upper := strings.ToUpper(w)
		asciiCaps := true
		for _, r := range w {
			if r < 'A' || r > 'Z' {
				asciiCaps = false
			}
		}
		automatic := asciiCaps && os.Getenv("TTS_SPELL_ACRONYMS") != "false" && len(w) >= 2 && len(w) <= 5 && w == upper && !skip[upper]
		if !force[upper] && !automatic {
			return w
		}
		var out []string
		for _, r := range w {
			r = unicode.ToUpper(r)
			if r >= 'A' && r <= 'Z' {
				out = append(out, letters[r-'A'])
			} else {
				out = append(out, string(r))
			}
		}
		return strings.Join(out, " ")
	})
}

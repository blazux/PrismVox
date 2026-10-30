// Package locale handles language hints and static call phrases without asking
// the conversation agent to classify text or follow a metadata protocol.
package locale

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"unicode"

	"github.com/abadojack/whatlanggo"
	"golang.org/x/text/language"
)

//go:embed catalogue.json
var catalogueJSON []byte
var catalogue = func() map[string]map[string]string {
	var result map[string]map[string]string
	if err := json.Unmarshal(catalogueJSON, &result); err != nil {
		panic(err)
	}
	return result
}()

func Normalize(code string) string {
	tag, err := language.Parse(strings.TrimSpace(code))
	if err != nil {
		return ""
	}
	base, _ := tag.Base()
	if base.String() == "und" {
		return ""
	}
	return base.String()
}

// Short acknowledgements and names are insufficient evidence for a switch.
func Detect(text string) string {
	letters := 0
	for _, r := range text {
		if unicode.IsLetter(r) {
			letters++
		}
	}
	if letters < 12 {
		return ""
	}
	info := whatlanggo.Detect(text)
	// The library's 0.8 reliability threshold targets longer documents. A
	// complete phone sentence often scores 0.4–0.7; short fragments stay excluded.
	if info.Confidence < 0.35 {
		return ""
	}
	return Normalize(info.Lang.Iso6391())
}

func Default(code, key string) string { return catalogue[Normalize(code)][key] }
func Defaults(code string) map[string]string {
	result := map[string]string{}
	for key, value := range catalogue[Normalize(code)] {
		result[key] = value
	}
	return result
}
func Keys() []string {
	keys := []string{}
	for key := range catalogue["en"] {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
func Signature(phrases map[string]string) string {
	keys := make([]string, 0, len(phrases))
	for key := range phrases {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, key := range keys {
		h.Write([]byte(key + "\x00" + phrases[key] + "\x00"))
	}
	return hex.EncodeToString(h.Sum(nil))
}
func ValidTranslation(source, translated map[string]string) bool {
	if len(source) != len(translated) {
		return false
	}
	for key, text := range source {
		value := translated[key]
		if strings.TrimSpace(value) == "" || len(value) > 4000 || strings.Count(value, "%s") != strings.Count(text, "%s") {
			return false
		}
	}
	return true
}

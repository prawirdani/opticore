package store

import (
	"encoding/json"
	"regexp"
	"strings"
)

// secretLeafRE follows README §9. It matches a suffix, which is why
// ParameterKey needs the explicit exclusion: RE2 has no lookbehind.
var secretLeafRE = regexp.MustCompile(`(?i)(password|passphrase|secret|psk|key)$`)

// IsSecretPath reports whether a parameter path's last segment names a secret.
func IsSecretPath(path string) bool {
	leaf := path
	if i := strings.LastIndexByte(path, '.'); i >= 0 {
		leaf = path[i+1:]
	}
	if strings.EqualFold(leaf, "ParameterKey") {
		return false
	}
	return secretLeafRE.MatchString(leaf)
}

// encodeParams converts a cache map to JSONB, keeping only the secret marker
// for secret-named leaves.
func encodeParams(in map[string]Param) ([]byte, error) {
	out := make(map[string]Param, len(in))
	for k, v := range in {
		if IsSecretPath(k) {
			out[k] = Param{Secret: true}
			continue
		}
		out[k] = v
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return b, nil
}

package secret

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
)

// tokenBytes is the entropy of every generated token.
const tokenBytes = 32

var tokenPrefixes = map[Kind]string{
	KindInvitationToken: "pci_",
	KindSessionToken:    "pcs_",
	KindOBSCredential:   "pco_",
}

var prefixKinds = map[string]Kind{
	"pci_": KindInvitationToken,
	"pcs_": KindSessionToken,
	"pco_": KindOBSCredential,
}

// Generate returns a new random token of the given kind. Tokens carry a
// kind prefix so that Scrub can recognize them inside free text.
func Generate(kind Kind) (Value, error) {
	prefix, ok := tokenPrefixes[kind]
	if !ok {
		return Value{}, fmt.Errorf("secret: kind %q is not a generated token", string(kind))
	}
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return Value{}, fmt.Errorf("secret: read random bytes: %w", err)
	}
	return New(kind, prefix+base64.RawURLEncoding.EncodeToString(buf)), nil
}

var (
	tokenPattern       = regexp.MustCompile(`pc[iso]_[A-Za-z0-9_-]{43}`)
	absoluteURLPattern = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s"'<>]+`)
	relativeURLPattern = regexp.MustCompile(`(^|[\s("'=])(/[^\s?#"'<>]*)([?#][^\s"'<>]*)`)
	userinfoPattern    = regexp.MustCompile(`^([a-zA-Z][a-zA-Z0-9+.-]*://)[^/?#]*@`)
)

// Scrub removes secret material from free text: generated tokens, URL
// userinfo, query strings, and fragments. It is the last line of defense
// for text that did not travel as a Value, such as wrapped error messages.
func Scrub(s string) string {
	s = tokenPattern.ReplaceAllStringFunc(s, func(m string) string {
		return "[REDACTED:" + string(prefixKinds[m[:4]]) + "]"
	})
	s = absoluteURLPattern.ReplaceAllStringFunc(s, func(u string) string {
		u = userinfoPattern.ReplaceAllString(u, "${1}[REDACTED]@")
		return cutQueryAndFragment(u)
	})
	return relativeURLPattern.ReplaceAllStringFunc(s, func(m string) string {
		parts := relativeURLPattern.FindStringSubmatch(m)
		return parts[1] + cutQueryAndFragment(parts[2]+parts[3])
	})
}

func cutQueryAndFragment(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		return u[:i] + "?[REDACTED]"
	}
	if i := strings.IndexByte(u, '#'); i >= 0 {
		return u[:i] + "#[REDACTED]"
	}
	return u
}

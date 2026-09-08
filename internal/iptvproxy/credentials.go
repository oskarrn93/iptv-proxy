package iptvproxy

import (
	"crypto/subtle"
	"net/url"
	"strings"
)

// Credentials is one username/password pair. Every place this proxy handles
// the two together — a feed's own configured pair, the pair an Xtream
// upstream_url embeds, the pair a request presents as query params, Basic
// Auth, or stream-path segments — passes them as one value, so the two can
// never be matched, swapped, or written out of step with each other.
//
// It is embedded inline into FeedConfig, so config.yaml still spells the pair
// as plain `username:`/`password:` keys on a feed.
type Credentials struct {
	Username string `yaml:"username" validate:"required"`
	Password string `yaml:"password" validate:"required"`
}

// credentialsFromURL reads the credentials an Xtream upstream URL carries as
// query params. A URL with no such params yields a zero Credentials, which
// every operation here treats as "nothing to match or swap".
func credentialsFromURL(u *url.URL) Credentials {
	q := u.Query()
	return Credentials{Username: q.Get("username"), Password: q.Get("password")}
}

// matches reports whether other is exactly c, comparing in constant time.
// Username and password are reported separately so a caller can tell a
// request for a known feed with the wrong password from one for no feed at
// all.
func (c Credentials) matches(other Credentials) (userOK, passOK bool) {
	return subtle.ConstantTimeCompare([]byte(other.Username), []byte(c.Username)) == 1,
		subtle.ConstantTimeCompare([]byte(other.Password), []byte(c.Password)) == 1
}

// unsafePathField names the first of c's fields that a URL path would spell
// differently than it is configured, or "" when both survive a round trip
// through a path unchanged. Credentials are matched and written as whole path
// segments without escaping either way (see credentialSwap), so a field that
// fails this could never match where it needs to.
func (c Credentials) unsafePathField() string {
	switch {
	case c.Username != url.PathEscape(c.Username):
		return "username"
	case c.Password != url.PathEscape(c.Password):
		return "password"
	}
	return ""
}

// credentialSwap rewrites one credential pair into another wherever it turns
// up as a whole path segment or a whole query value. Matching is exact rather
// than a substring replace, so a credential that happens to be a prefix of
// some other value in the URL can't corrupt it.
//
// An empty field on either side disables that half of the swap: an upstream
// with no credentials in its URL has nothing to hide, and so nothing to put
// back.
type credentialSwap struct {
	from, to Credentials
}

// reversed returns the swap that undoes c.
func (c credentialSwap) reversed() credentialSwap {
	return credentialSwap{from: c.to, to: c.from}
}

// noop reports whether c has nothing to swap — an upstream whose URL carries
// no credentials. apply then leaves the URL exactly as it found it rather than
// round-tripping it through ParseQuery/Encode, which would reorder and
// re-escape query parameters for every feed to serve a rule only Xtream feeds
// with embedded credentials need.
func (c credentialSwap) noop() bool {
	return (c.from.Username == "" || c.to.Username == "") && (c.from.Password == "" || c.to.Password == "")
}

func (c credentialSwap) value(v string) string {
	if c.from.Username != "" && c.to.Username != "" && v == c.from.Username {
		return c.to.Username
	}
	if c.from.Password != "" && c.to.Password != "" && v == c.from.Password {
		return c.to.Password
	}
	return v
}

// apply returns path and rawQuery with every credential occurrence swapped. A
// query pair too malformed for url.ParseQuery is dropped rather than passed
// through: it could be a mangled spelling of the credentials themselves, and
// passing it through is what would leak them.
func (c credentialSwap) apply(path, rawQuery string) (string, string) {
	if c.noop() {
		return path, rawQuery
	}
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		segments[i] = c.value(seg)
	}
	path = strings.Join(segments, "/")

	if rawQuery == "" {
		return path, ""
	}
	values, _ := url.ParseQuery(rawQuery)
	for _, vs := range values {
		for i, v := range vs {
			vs[i] = c.value(v)
		}
	}
	return path, values.Encode()
}

package config

import (
	"fmt"
	"strings"
)

// DBKeys holds resolved database passphrases. Empty means plaintext.
type DBKeys struct {
	State string
	Cache string
}

// ResolveDBKeys resolves db.encryption_key and/or db.cache.encryption_key.
// Like ServeAuth it runs only when a command needs that file, so run-rules
// (state only) never needs the cache key or, for a plaintext state DB,
// datawatch. A reference that cannot be resolved fails closed (D5); a key is
// never generated.
func (c *Config) ResolveDBKeys(state, cache bool) (DBKeys, error) {
	var out DBKeys
	var resolver *secretResolver
	resolve := func(field, v string) (string, error) {
		if v == "" {
			return "", nil
		}
		if hasSecretRef(v) {
			if resolver == nil {
				if c.Datawatch == nil || strings.TrimSpace(c.Datawatch.APIURL) == "" || strings.TrimSpace(c.Datawatch.Token) == "" {
					return "", fmt.Errorf("%s: ${secret:...} needs a datawatch block with api_url and token", field)
				}
				resolver = newSecretResolver(c.Datawatch.APIURL, c.Datawatch.Token)
			}
			var err error
			if v, err = resolver.expand(v); err != nil {
				return "", fmt.Errorf("%s: %w", field, err)
			}
		}
		if unresolvedRe.MatchString(v) {
			return "", fmt.Errorf("%s: key reference is unresolved (environment variable unset?)", field)
		}
		if v == "" {
			return "", fmt.Errorf("%s: resolved to an empty key", field)
		}
		return v, nil
	}
	var err error
	if state {
		if out.State, err = resolve("db.encryption_key", c.DB.EncryptionKey); err != nil {
			return DBKeys{}, err
		}
	}
	if cache {
		if out.Cache, err = resolve("db.cache.encryption_key", c.DB.Cache.EncryptionKey); err != nil {
			return DBKeys{}, err
		}
	}
	return out, nil
}

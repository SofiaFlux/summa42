package climodel

import (
	"fmt"
	"path"
	"slices"
)

// Capacity groups a profile's models into pools. Models in one pool share one
// limit at the provider, so a refusal for one blocks the whole pool.
type Capacity struct {
	Pools []Pool `json:"pools,omitempty"`
}

// Pool is one limit: the models whose ids match any of Models.
type Pool struct {
	Name string `json:"name"`
	// Models are path.Match patterns, e.g. "gemini-*".
	Models []string `json:"models"`
}

// PoolFor names the pool a model draws on. A model no pool lists falls into
// the profile's default pool, so an unknown model is still counted.
func (p Profile) PoolFor(model string) string {
	for _, pool := range p.Capacity.Pools {
		for _, pattern := range pool.Models {
			if ok, _ := path.Match(pattern, model); ok {
				return p.Name + ":" + pool.Name
			}
		}
	}
	return p.Name + ":default"
}

// Identify names the harness and model a configuration runs, for the record of
// who gave a verdict. A plain argv binary has no model id, so it is named by
// its binary base name.
func Identify(config Config) (harness, model string) {
	switch {
	case config.CommandCodeModel != "":
		return "commandcode", config.CommandCodeModel
	case config.Profile != "":
		return config.Profile, config.ProfileModel
	}
	return path.Base(config.Binary), ""
}

// PoolOf names the pool a configuration's model draws on, or "" when the
// harness has no capacity model (a plain argv binary).
func PoolOf(config Config) (string, error) {
	switch {
	case config.CommandCodeModel != "":
		return commandCodePool(config.CommandCodeModel), nil
	case config.Profile != "":
		p, _, err := resolveProfile(config)
		if err != nil {
			return "", err
		}
		return p.PoolFor(config.ProfileModel), nil
	}
	return "", nil
}

// validate refuses pools whose patterns cannot be matched or that overlap.
// Two patterns overlap when one matches the other as a literal, which catches
// "gemini-*" against "gemini-3*". It is a check on the pattern text, not a
// proof about every model id.
// ponytail: text check, not set arithmetic; a pattern pair that overlaps only
// through a model id neither pattern spells out would slip past.
func (c Capacity) validate() error {
	type owned struct{ pool, pattern string }
	var seen []owned
	for _, pool := range c.Pools {
		if pool.Name == "" {
			return fmt.Errorf("capacity pool has no name")
		}
		for _, pattern := range pool.Models {
			if _, err := path.Match(pattern, ""); err != nil {
				return fmt.Errorf("capacity pattern %q: %w", pattern, err)
			}
			for _, other := range seen {
				if other.pool == pool.Name {
					continue
				}
				if ok, _ := path.Match(other.pattern, pattern); ok {
					return fmt.Errorf("capacity pools %s and %s overlap on %q", other.pool, pool.Name, pattern)
				}
				if ok, _ := path.Match(pattern, other.pattern); ok {
					return fmt.Errorf("capacity pools %s and %s overlap on %q", other.pool, pool.Name, other.pattern)
				}
			}
			seen = append(seen, owned{pool.Name, pattern})
		}
	}
	return nil
}

// CheckTrustedFile refuses a file that is not a regular file owned by this user
// or that others can write. The Owner's lists are read through it, as profiles are.
func CheckTrustedFile(path string) error { return trustedPath(path, false) }

// IsFreeCommandCodeModel reports whether model is billed at $0 on Command Code.
func IsFreeCommandCodeModel(model string) bool { return slices.Contains(commandCodeFree, model) }

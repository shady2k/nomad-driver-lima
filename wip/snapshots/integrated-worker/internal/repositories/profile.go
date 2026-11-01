// Package repositories resolves pinned, non-secret operator repository profiles.
package repositories

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type APT struct {
	SuiteURIs  map[string]string `json:"suite_uris"`
	Components []string          `json:"components,omitempty"`
}
type NPM struct {
	Registry string `json:"registry"`
}
type Go struct {
	Proxy               string `json:"proxy"`
	DistributionBaseURL string `json:"distribution_base_url,omitempty"`
}
type Profile struct {
	SchemaVersion int      `json:"schema_version"`
	ID            string   `json:"id"`
	Version       string   `json:"version"`
	Architectures []string `json:"architectures,omitempty"`
	APT           *APT     `json:"apt,omitempty"`
	NPM           *NPM     `json:"npm,omitempty"`
	Go            *Go      `json:"go,omitempty"`
}
type Config struct {
	Default  string             `json:"repository_profile_default,omitempty"`
	Profiles map[string]Profile `json:"repository_profiles"`
}
type Selection struct {
	Profile           Profile `json:"profile"`
	SHA256            string  `json:"sha256"`
	EnvironmentSHA256 string  `json:"environment_sha256,omitempty"`
	ExecutorSHA256    string  `json:"executor_sha256,omitempty"`
}

var shaPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func Hash(raw []byte) string { h := sha256.Sum256(raw); return hex.EncodeToString(h[:]) }
func Canonical(p Profile) ([]byte, error) {
	raw, e := json.Marshal(p)
	if e != nil {
		return nil, e
	}
	var v any
	if e = json.Unmarshal(raw, &v); e != nil {
		return nil, e
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	e = enc.Encode(v)
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), e
}
func endpoint(value string) (string, error) {
	for _, c := range value {
		if c < 33 || c > 126 || strings.ContainsRune(`\"'${},|`, c) {
			return "", fmt.Errorf("repository HTTPS endpoint required")
		}
	}
	u, e := url.Parse(value)
	if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(value) > 2048 {
		return "", fmt.Errorf("repository HTTPS endpoint required")
	}
	if u.Port() != "" {
		if _, e = fmt.Sscanf(u.Port(), "%d", new(int)); e != nil {
			return "", fmt.Errorf("repository port invalid")
		}
	}
	return strings.TrimRight(value, "/") + "/", nil
}
func Normalize(p Profile) (Profile, error) {
	if p.SchemaVersion != 1 || !regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`).MatchString(p.ID) || !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,79}$`).MatchString(p.Version) || (p.APT == nil && p.NPM == nil && p.Go == nil) {
		return p, fmt.Errorf("repository profile schema/identity invalid")
	}
	if p.Architectures != nil {
		if len(p.Architectures) == 0 {
			return p, fmt.Errorf("empty profile architectures")
		}
		seen := map[string]bool{}
		for _, a := range p.Architectures {
			if (a != "amd64" && a != "arm64") || seen[a] {
				return p, fmt.Errorf("unsupported profile architecture")
			}
			seen[a] = true
		}
		p.Architectures = append([]string(nil), p.Architectures...)
		sort.Strings(p.Architectures)
	}
	if p.APT != nil {
		if len(p.APT.SuiteURIs) == 0 {
			return p, fmt.Errorf("empty apt suite mapping")
		}
		copy := map[string]string{}
		for suite, uri := range p.APT.SuiteURIs {
			switch suite {
			case "noble", "noble-updates", "noble-backports", "noble-security":
			default:
				return p, fmt.Errorf("unsupported apt suite")
			}
			v, e := endpoint(uri)
			if e != nil {
				return p, e
			}
			copy[suite] = v
		}
		components := p.APT.Components
		if components != nil {
			if len(components) == 0 {
				return p, fmt.Errorf("empty apt components")
			}
			seen := map[string]bool{}
			for _, v := range components {
				if (v != "main" && v != "restricted" && v != "universe" && v != "multiverse") || seen[v] {
					return p, fmt.Errorf("unsupported apt component")
				}
				seen[v] = true
			}
			components = append([]string(nil), components...)
			sort.Strings(components)
		}
		p.APT = &APT{SuiteURIs: copy, Components: components}
	}
	if p.NPM != nil {
		v, e := endpoint(p.NPM.Registry)
		if e != nil {
			return p, e
		}
		p.NPM = &NPM{v}
	}
	if p.Go != nil {
		v, e := endpoint(p.Go.Proxy)
		if e != nil {
			return p, e
		}
		g := &Go{Proxy: strings.TrimRight(v, "/")}
		if p.Go.DistributionBaseURL != "" {
			g.DistributionBaseURL, e = endpoint(p.Go.DistributionBaseURL)
			if e != nil {
				return p, e
			}
		}
		p.Go = g
	}
	return p, nil
}
func Load(path, sha string) (Config, error) {
	var c Config
	if path == "" {
		if sha != "" {
			return c, fmt.Errorf("repository file required")
		}
		return c, nil
	}
	if !shaPattern.MatchString(sha) {
		return c, fmt.Errorf("repository config SHA required")
	}
	if !filepath.IsAbs(path) {
		return c, fmt.Errorf("absolute repository config path required")
	}
	st, e := os.Lstat(path)
	if e != nil {
		return c, e
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0022 != 0 || st.Size() > 1048576 {
		return c, fmt.Errorf("unsafe repository config")
	}
	raw, e := os.ReadFile(path)
	if e != nil {
		return c, e
	}
	if Hash(raw) != sha {
		return c, fmt.Errorf("repository config checksum mismatch")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e = d.Decode(&c); e != nil {
		return c, fmt.Errorf("repository config schema invalid")
	}
	if d.Decode(new(any)) != io.EOF {
		return c, fmt.Errorf("repository config trailing data")
	}
	for id, p := range c.Profiles {
		v, e := Normalize(p)
		if e != nil || v.ID != id {
			return c, fmt.Errorf("repository profile invalid")
		}
		c.Profiles[id] = v
	}
	if c.Default != "" {
		if _, ok := c.Profiles[c.Default]; !ok {
			return c, fmt.Errorf("repository default not defined")
		}
	}
	return c, nil
}
func Select(c Config, id, expected string) (*Selection, error) {
	if id == "" {
		id = c.Default
	}
	if id == "" {
		if expected != "" {
			return nil, fmt.Errorf("repository ID required")
		}
		return nil, nil
	}
	p, ok := c.Profiles[id]
	if !ok {
		return nil, fmt.Errorf("repository profile not approved")
	}
	raw, e := Canonical(p)
	if e != nil {
		return nil, e
	}
	sha := Hash(raw)
	if expected != "" && expected != sha {
		return nil, fmt.Errorf("repository task profile changed")
	}
	return &Selection{Profile: p, SHA256: sha}, nil
}
func (s *Selection) Validate() error {
	if s == nil {
		return nil
	}
	p, e := Normalize(s.Profile)
	if e != nil {
		return e
	}
	raw, e := Canonical(p)
	if e != nil || Hash(raw) != s.SHA256 {
		return fmt.Errorf("stored repository profile changed")
	}
	return nil
}
func (s *Selection) Reference() (map[string]string, error) {
	if s == nil {
		return nil, nil
	}
	if e := s.Validate(); e != nil {
		return nil, e
	}
	if !shaPattern.MatchString(s.EnvironmentSHA256) || !shaPattern.MatchString(s.ExecutorSHA256) {
		return nil, fmt.Errorf("repository profile not ready")
	}
	return map[string]string{"profile_sha256": s.SHA256, "environment_sha256": s.EnvironmentSHA256, "executor_sha256": s.ExecutorSHA256}, nil
}

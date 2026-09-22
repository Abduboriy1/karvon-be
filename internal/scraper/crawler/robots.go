package crawler

import (
	"bufio"
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// robotsTTL is how long a parsed robots.txt stays cached per host.
const robotsTTL = time.Hour

// robotsMaxBytes bounds how much of a robots.txt we will read.
const robotsMaxBytes = 512 << 10

type robotsRule struct {
	allow   bool
	pattern string
}

type robotsGroup struct {
	agents []string
	rules  []robotsRule
}

// RobotsPolicy is a parsed robots.txt.
type RobotsPolicy struct {
	groups []robotsGroup
}

// Allowed reports whether path may be fetched by userAgent.
func (p *RobotsPolicy) Allowed(userAgent, path string) bool {
	if p == nil || len(p.groups) == 0 {
		return true
	}
	group := p.matchGroup(userAgent)
	if group == nil {
		return true
	}

	bestLen, bestAllow := -1, true
	for _, rule := range group.rules {
		if rule.pattern == "" {
			continue
		}
		if !matchRobotsPattern(rule.pattern, path) {
			continue
		}
		length := len(rule.pattern)
		// Longest match wins; Allow wins a tie, per the de-facto standard.
		if length > bestLen || (length == bestLen && rule.allow) {
			bestLen, bestAllow = length, rule.allow
		}
	}
	if bestLen == -1 {
		return true
	}
	return bestAllow
}

// matchGroup picks the most specific group for a user agent, falling back to "*".
func (p *RobotsPolicy) matchGroup(userAgent string) *robotsGroup {
	ua := strings.ToLower(userAgent)
	var wildcard *robotsGroup
	var best *robotsGroup
	bestLen := -1

	for i := range p.groups {
		group := &p.groups[i]
		for _, agent := range group.agents {
			if agent == "*" {
				if wildcard == nil {
					wildcard = group
				}
				continue
			}
			if strings.Contains(ua, agent) && len(agent) > bestLen {
				best, bestLen = group, len(agent)
			}
		}
	}
	if best != nil {
		return best
	}
	return wildcard
}

// matchRobotsPattern implements the `*` and `$` wildcards.
func matchRobotsPattern(pattern, path string) bool {
	if !strings.Contains(pattern, "*") && !strings.HasSuffix(pattern, "$") {
		return strings.HasPrefix(path, pattern)
	}

	mustEnd := strings.HasSuffix(pattern, "$")
	pattern = strings.TrimSuffix(pattern, "$")
	parts := strings.Split(pattern, "*")

	pos := 0
	for i, part := range parts {
		if part == "" {
			continue
		}
		if i == 0 {
			if !strings.HasPrefix(path, part) {
				return false
			}
			pos = len(part)
			continue
		}
		idx := strings.Index(path[pos:], part)
		if idx < 0 {
			return false
		}
		pos += idx + len(part)
	}
	if mustEnd {
		last := parts[len(parts)-1]
		return strings.HasSuffix(path, last)
	}
	return true
}

// ParseRobots reads a robots.txt body into a policy.
func ParseRobots(body []byte) *RobotsPolicy {
	policy := &RobotsPolicy{}
	var current *robotsGroup
	startingGroup := false

	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	scanner.Buffer(make([]byte, 0, 64*1024), robotsMaxBytes)

	for scanner.Scan() {
		line := scanner.Text()
		if idx := strings.Index(line, "#"); idx >= 0 {
			line = line[:idx]
		}
		field, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		field = strings.ToLower(strings.TrimSpace(field))
		value = strings.TrimSpace(value)

		switch field {
		case "user-agent":
			if current == nil || !startingGroup {
				policy.groups = append(policy.groups, robotsGroup{})
				current = &policy.groups[len(policy.groups)-1]
				startingGroup = true
			}
			current.agents = append(current.agents, strings.ToLower(value))
		case "disallow", "allow":
			if current == nil {
				continue
			}
			startingGroup = false
			current.rules = append(current.rules, robotsRule{allow: field == "allow", pattern: value})
		}
	}
	return policy
}

// RobotsCache fetches and caches robots.txt per host.
type RobotsCache struct {
	fetcher   *Fetcher
	userAgent string

	mu      sync.Mutex
	entries map[string]*robotsEntry
}

type robotsEntry struct {
	once   sync.Once
	policy *RobotsPolicy
	at     time.Time
}

// NewRobotsCache builds a cache backed by a Fetcher.
func NewRobotsCache(fetcher *Fetcher, userAgent string) *RobotsCache {
	return &RobotsCache{fetcher: fetcher, userAgent: userAgent, entries: make(map[string]*robotsEntry)}
}

// Allowed reports whether target may be fetched. A robots.txt that cannot be
// retrieved is treated as "no restrictions", which is what the standard prescribes
// for 4xx responses and what every mainstream crawler does on network errors.
func (c *RobotsCache) Allowed(ctx context.Context, target *url.URL) bool {
	policy := c.policyFor(ctx, target)
	path := target.EscapedPath()
	if !strings.HasPrefix(path, "/") {
		// url.JoinPath can leave the leading slash off when the base URL has no path.
		path = "/" + path
	}
	if target.RawQuery != "" {
		path += "?" + target.RawQuery
	}
	return policy.Allowed(c.userAgent, path)
}

func (c *RobotsCache) policyFor(ctx context.Context, target *url.URL) *RobotsPolicy {
	key := target.Scheme + "://" + target.Host

	c.mu.Lock()
	entry, ok := c.entries[key]
	if ok && time.Since(entry.at) > robotsTTL {
		ok = false
	}
	if !ok {
		entry = &robotsEntry{at: time.Now()}
		c.entries[key] = entry
	}
	c.mu.Unlock()

	entry.once.Do(func() {
		robotsURL := &url.URL{Scheme: target.Scheme, Host: target.Host, Path: "/robots.txt"}
		body, status, err := c.fetcher.GetRaw(ctx, robotsURL, robotsMaxBytes)
		if err != nil || status != http.StatusOK {
			entry.policy = &RobotsPolicy{}
			return
		}
		entry.policy = ParseRobots(body)
	})
	return entry.policy
}

package healthcheck

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
)

// Mode describes how the filter should treat matched health-check traffic.
type Mode string

const (
	// ModeTag keeps the log entry but adds a marker attribute.
	ModeTag Mode = "tag"
	// ModeDemote reduces the log level before emission.
	ModeDemote Mode = "demote"
	// ModeDrop suppresses matched log entries entirely.
	ModeDrop Mode = "drop"
)

// Config configures detection signals and behaviour for health-check traffic.
type Config struct {
	Enabled bool

	Mode Mode

	// TagKey controls the attribute inserted when ModeTag or ModeDemote is used.
	// When empty, no attribute is added outside of ModeDrop.
	TagKey string

	// TagValue controls the value used when TagKey is emitted. Defaults to true.
	TagValue bool

	// DemoteTo controls the slog level used when ModeDemote is active.
	// When nil, slog.LevelDebug is used.
	DemoteTo *slog.Level

	// Paths are exact HTTP URL paths that should be treated as health checks.
	Paths []string
	// PathPrefixes are HTTP path prefixes matched using strings.HasPrefix.
	PathPrefixes []string

	// Methods are full gRPC method names (e.g. "/package.Service/Method") treated
	// as health checks.
	Methods []string
	// MethodPrefixes are prefix matches applied to gRPC method names.
	MethodPrefixes []string

	// HeaderEquals matches request headers (HTTP or metadata) by canonical name
	// and exact value. Header names are normalised to lower-case.
	HeaderEquals map[string][]string

	// UserAgentPatterns holds regular expressions that match health-check User-Agents.
	UserAgentPatterns []*regexp.Regexp

	// RemoteCIDRs lists IP networks treated as health-check sources. Both HTTP
	// RemoteAddr and X-Forwarded-For values are evaluated.
	RemoteCIDRs []*net.IPNet

	// InspectXForwardedFor controls whether the first X-Forwarded-For entry is
	// evaluated against RemoteCIDRs. Defaults to true.
	InspectXForwardedFor *bool
}

// Decision describes how a match should be handled by callers.
type Decision struct {
	Matched bool
	Mode    Mode

	TagKey   string
	TagValue bool

	DemoteLevel slog.Level
	HasDemote   bool
}

// ShouldTag reports whether the decision requires a tag attribute.
func (d Decision) ShouldTag() bool {
	return d.Matched && d.TagKey != ""
}

// ApplyLevel applies the decision to an existing slog level.
func (d Decision) ApplyLevel(level slog.Level) slog.Level {
	if !d.Matched {
		return level
	}
	if d.Mode != ModeDemote || !d.HasDemote {
		return level
	}
	if level > d.DemoteLevel {
		return d.DemoteLevel
	}
	return level
}

// Clone shallow-copies the configuration while duplicating slice and map data.
func (c Config) Clone() Config {
	out := c
	out.Paths = append([]string(nil), c.Paths...)
	out.PathPrefixes = append([]string(nil), c.PathPrefixes...)
	out.Methods = append([]string(nil), c.Methods...)
	out.MethodPrefixes = append([]string(nil), c.MethodPrefixes...)
	out.UserAgentPatterns = append([]*regexp.Regexp(nil), c.UserAgentPatterns...)
	out.RemoteCIDRs = append([]*net.IPNet(nil), c.RemoteCIDRs...)
	if len(c.HeaderEquals) > 0 {
		out.HeaderEquals = make(map[string][]string, len(c.HeaderEquals))
		for k, v := range c.HeaderEquals {
			out.HeaderEquals[k] = append([]string(nil), v...)
		}
	} else if c.HeaderEquals != nil {
		out.HeaderEquals = map[string][]string{}
	}
	return out
}

// DefaultConfig returns a configuration seeded with common Google Cloud probe
// signals. The configuration is disabled by default and uses tag mode so
// adopting projects must opt in explicitly and can observe behaviour safely.
func DefaultConfig() Config {
	inspect := true
	demote := slog.LevelDebug

	mustCIDR := func(raw string) *net.IPNet {
		_, network, err := net.ParseCIDR(raw)
		if err != nil {
			panic(err)
		}
		return network
	}

	return Config{
		Enabled:              false,
		Mode:                 ModeTag,
		TagKey:               "is_health_check",
		TagValue:             true,
		DemoteTo:             &demote,
		Paths:                []string{"/", "/healthz", "/_ah/health"},
		Methods:              []string{"/grpc.health.v1.Health/Check", "/grpc.health.v1.Health/Watch"},
		UserAgentPatterns:    []*regexp.Regexp{regexp.MustCompile(`^GoogleHC/`), regexp.MustCompile(`^GoogleStackdriverMonitoring-UptimeChecks`)},
		RemoteCIDRs:          []*net.IPNet{mustCIDR("35.191.0.0/16"), mustCIDR("130.211.0.0/22")},
		InspectXForwardedFor: &inspect,
		HeaderEquals:         map[string][]string{},
	}
}

// Filter performs health-check classification using a normalised Config.
type Filter struct {
	cfg Config

	pathExact    map[string]struct{}
	methodExact  map[string]struct{}
	headerEquals map[string]map[string]struct{}
}

// NewFilter prepares a Filter from the provided configuration. The configuration
// is cloned to avoid external mutation.
func NewFilter(cfg Config) *Filter {
	if !cfg.Enabled {
		return nil
	}
	clone := cfg.Clone()
	normaliseConfig(&clone)

	f := &Filter{
		cfg:          clone,
		pathExact:    toSet(clone.Paths),
		methodExact:  toSet(clone.Methods),
		headerEquals: make(map[string]map[string]struct{}, len(clone.HeaderEquals)),
	}
	for key, values := range clone.HeaderEquals {
		key = strings.ToLower(key)
		valueSet := make(map[string]struct{}, len(values))
		for _, v := range values {
			valueSet[strings.TrimSpace(v)] = struct{}{}
		}
		f.headerEquals[key] = valueSet
	}
	return f
}

func toSet(values []string) map[string]struct{} {
	if len(values) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(values))
	for _, v := range values {
		if v == "" {
			continue
		}
		set[v] = struct{}{}
	}
	return set
}

func normaliseConfig(cfg *Config) {
	if cfg.TagValue == false && cfg.Mode != ModeDrop && cfg.TagKey != "" {
		cfg.TagValue = true
	}
	if cfg.TagKey == "" && cfg.Mode != ModeDrop {
		cfg.TagKey = "is_health_check"
		cfg.TagValue = true
	}
	if cfg.Mode == "" {
		cfg.Mode = ModeTag
	}
	if cfg.DemoteTo == nil {
		d := slog.LevelDebug
		cfg.DemoteTo = &d
	}
	if cfg.HeaderEquals == nil {
		cfg.HeaderEquals = map[string][]string{}
	}
	if cfg.InspectXForwardedFor == nil {
		defaultInspect := true
		cfg.InspectXForwardedFor = &defaultInspect
	}
}

// MatchHTTP evaluates whether the provided request should be considered a health check.
func (f *Filter) MatchHTTP(r *http.Request) Decision {
	if f == nil || r == nil {
		return Decision{}
	}

	if f.pathExact != nil {
		if _, ok := f.pathExact[r.URL.Path]; ok {
			return f.decision()
		}
	}
	for _, prefix := range f.cfg.PathPrefixes {
		if strings.HasPrefix(r.URL.Path, prefix) {
			return f.decision()
		}
	}

	if ua := r.Header.Get("User-Agent"); ua != "" {
		for _, re := range f.cfg.UserAgentPatterns {
			if re != nil && re.MatchString(ua) {
				return f.decision()
			}
		}
	}

	if f.matchHeaders(httpHeaderToMap(r.Header)) {
		return f.decision()
	}

	if f.matchIP(r.RemoteAddr) {
		return f.decision()
	}
	if inspect := f.cfg.InspectXForwardedFor; inspect != nil && *inspect {
		if ip := firstForwardedIP(r.Header.Values("X-Forwarded-For")); ip != "" && f.matchIP(ip) {
			return f.decision()
		}
	}

	return Decision{}
}

// MatchGRPC evaluates whether the gRPC call described by method, metadata, and peer address should be treated as a health check.
func (f *Filter) MatchGRPC(fullMethod string, metadata map[string][]string, peerAddr string) Decision {
	if f == nil {
		return Decision{}
	}
	if f.methodExact != nil {
		if _, ok := f.methodExact[fullMethod]; ok {
			return f.decision()
		}
	}
	for _, prefix := range f.cfg.MethodPrefixes {
		if strings.HasPrefix(fullMethod, prefix) {
			return f.decision()
		}
	}

	if len(metadata) > 0 {
		if f.matchHeaders(metadata) {
			return f.decision()
		}
		if uaVals, ok := metadata["user-agent"]; ok {
			for _, ua := range uaVals {
				if ua == "" {
					continue
				}
				for _, re := range f.cfg.UserAgentPatterns {
					if re != nil && re.MatchString(ua) {
						return f.decision()
					}
				}
			}
		}
	}

	if f.matchIP(peerAddr) {
		return f.decision()
	}

	return Decision{}
}

func (f *Filter) decision() Decision {
	d := Decision{
		Matched: true,
		Mode:    f.cfg.Mode,
	}
	if f.cfg.TagKey != "" && f.cfg.Mode != ModeDrop {
		d.TagKey = f.cfg.TagKey
		d.TagValue = f.cfg.TagValue
	}
	if f.cfg.Mode == ModeDemote && f.cfg.DemoteTo != nil {
		d.DemoteLevel = *f.cfg.DemoteTo
		d.HasDemote = true
	}
	return d
}

func (f *Filter) matchHeaders(headers map[string][]string) bool {
	if len(f.headerEquals) == 0 || len(headers) == 0 {
		return false
	}
	for rawKey, values := range headers {
		key := strings.ToLower(rawKey)
		expected, ok := f.headerEquals[key]
		if !ok {
			continue
		}
		for _, v := range values {
			if _, match := expected[strings.TrimSpace(v)]; match {
				return true
			}
		}
	}
	return false
}

func (f *Filter) matchIP(addr string) bool {
	if len(f.cfg.RemoteCIDRs) == 0 || addr == "" {
		return false
	}
	ip := parseIP(addr)
	if ip == nil {
		return false
	}
	for _, cidr := range f.cfg.RemoteCIDRs {
		if cidr != nil && cidr.Contains(ip) {
			return true
		}
	}
	return false
}

func parseIP(input string) net.IP {
	if input == "" {
		return nil
	}
	if host, _, err := net.SplitHostPort(strings.TrimSpace(input)); err == nil {
		input = host
	}
	return net.ParseIP(strings.TrimSpace(input))
}

func httpHeaderToMap(header http.Header) map[string][]string {
	if header == nil {
		return nil
	}
	result := make(map[string][]string, len(header))
	for k, v := range header {
		lower := strings.ToLower(k)
		result[lower] = append([]string(nil), v...)
	}
	return result
}

func firstForwardedIP(values []string) string {
	for _, value := range values {
		parts := strings.Split(value, ",")
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			return part
		}
	}
	return ""
}

type ctxKey struct{}

// ContextWithDecision annotates ctx with the supplied decision.
func ContextWithDecision(ctx context.Context, decision Decision) context.Context {
	if !decision.Matched {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, decision)
}

// DecisionFromContext retrieves a health-check decision previously stored in the context.
func DecisionFromContext(ctx context.Context) (Decision, bool) {
	if ctx == nil {
		return Decision{}, false
	}
	if d, ok := ctx.Value(ctxKey{}).(Decision); ok && d.Matched {
		return d, true
	}
	return Decision{}, false
}

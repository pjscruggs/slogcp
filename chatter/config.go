package chatter

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	envPrefix                 = "SLOGCP_CHATTER_"
	envMode                   = envPrefix + "MODE"
	envHTTPEnabled            = envPrefix + "HTTP_ENABLED"
	envGRPCEnabled            = envPrefix + "GRPC_ENABLED"
	envAction                 = envPrefix + "ACTION"
	envAlwaysLogErrors        = envPrefix + "ALWAYS_LOG_ERRORS"
	envLatencyThreshold       = envPrefix + "LATENCY_THRESHOLD"
	envHTTPIgnoreIPs          = envPrefix + "HTTP_IGNORE_IPS"
	envGRPCIgnoreMethods      = envPrefix + "GRPC_IGNORE_METHODS"
	envGRPCIgnoreOKOnly       = envPrefix + "GRPC_IGNORE_OK_ONLY"
	envHTTPIgnorePaths        = envPrefix + "HTTP_IGNORE_PATHS"
	envHTTPIgnorePrefixes     = envPrefix + "HTTP_IGNORE_PREFIXES"
	envHTTPIgnoreRegex        = envPrefix + "HTTP_IGNORE_REGEX"
	envHTTPMethods            = envPrefix + "HTTP_METHODS"
	envHTTPIgnoreHeaders      = envPrefix + "HTTP_IGNORE_HEADERS"
	envAppEngineSuppressAH    = envPrefix + "APPENGINE_SUPPRESS_AH"
	envAppEngineCronAction    = envPrefix + "APPENGINE_CRON_ACTION"
	envTrustProxy             = envPrefix + "TRUST_PROXY"
	envTrustedHops            = envPrefix + "TRUSTED_HOPS"
	envTrustedProxyCIDRs      = envPrefix + "TRUSTED_PROXY_CIDRS"
	envClientIPHeader         = envPrefix + "CLIENT_IP_HEADER"
	envHTTPSampleKeepEvery    = envPrefix + "HTTP_SAMPLE_KEEP_EVERY"
	envGRPCSampleKeepEvery    = envPrefix + "GRPC_SAMPLE_KEEP_EVERY"
	envHTTPSampleRate         = envPrefix + "HTTP_SAMPLE_RATE"
	envGRPCSampleRate         = envPrefix + "GRPC_SAMPLE_RATE"
	envHTTPSampleMaxPerMinute = envPrefix + "HTTP_SAMPLE_MAX_PER_MINUTE"
	envGRPCSampleMaxPerMinute = envPrefix + "GRPC_SAMPLE_MAX_PER_MINUTE"
	envSampleMaxPerMinute     = envPrefix + "SAMPLE_MAX_PER_MINUTE"
	envSummaryInterval        = envPrefix + "SUMMARY_INTERVAL"
	envReasonField            = envPrefix + "REASON_FIELD"
	envDecisionField          = envPrefix + "DECISION_FIELD"
	envRuleField              = envPrefix + "RULE_FIELD"
	envSafetyRailField        = envPrefix + "SAFETY_RAIL_FIELD"
	envAuditPrefix            = envPrefix + "AUDIT_PREFIX"
	envHTTPOverrideDefaults   = envPrefix + "HTTP_OVERRIDE_DEFAULTS"
	envGRPCOverrideDefaults   = envPrefix + "GRPC_OVERRIDE_DEFAULTS"
	envWatchDedupInterval     = envPrefix + "GRPC_WATCH_DEDUP_INTERVAL"
)

var (
	defaultGRPCMethods = []string{"/grpc.health.v1.Health/Check", "/grpc.health.v1.Health/Watch"}
)

// DefaultsMode captures whether built-in defaults are additive or overridden.
type DefaultsMode string

const (
	DefaultsAdditive DefaultsMode = "additive"
	DefaultsOverride DefaultsMode = "override"
)

// HeaderRule represents a header presence or equality rule.
type HeaderRule struct {
	Key      string
	Value    string
	HasValue bool
}

// Config contains the fully merged chatter reduction configuration. It is an
// immutable snapshot and should be cloned before modification.
type Config struct {
	Mode             Mode
	Action           Action
	AlwaysLogErrors  bool
	LatencyThreshold time.Duration

	HTTPEnabledOverride *bool
	GRPCEnabledOverride *bool

	HTTP  HTTPSettings
	GRPC  GRPCSettings
	Proxy ProxySettings

	Sampling SamplingSettings
	Summary  SummarySettings
	Audit    AuditSettings

	configErrors []error
}

// HTTPSettings covers HTTP-specific chatter detection knobs.
type HTTPSettings struct {
	IgnoreCIDRs         []string
	Paths               []string
	Prefixes            []string
	Regexes             []string
	Headers             []HeaderRule
	Methods             []string
	AppEngineSuppressAH bool
	AppEngineCronAction Action
	DefaultsMode        DefaultsMode
}

// GRPCSettings covers gRPC-specific chatter detection knobs.
type GRPCSettings struct {
	IgnoreMethods      []string
	IgnoreOKOnly       bool
	DefaultsMode       DefaultsMode
	WatchDedupInterval time.Duration
}

// ProxySettings configures trusted proxy behaviour for client IP extraction.
type ProxySettings struct {
	TrustProxy bool
	// TrustedHops controls which element in the forwarded-for chain is treated
	// as the client IP: the engine selects len(X-Forwarded-For)-TrustedHops-1,
	// mirroring common 'trust proxy' semantics in web frameworks.
	TrustedHops       int
	TrustedProxyCIDRs []string
	ClientIPHeader    string
}

// SamplingSettings captures deterministic/probabilistic sampling knobs.
type SamplingSettings struct {
	HTTPSampleKeepEvery    int
	GRPCSampleKeepEvery    int
	HTTPSampleRate         float64
	GRPCSampleRate         float64
	HTTPSampleMaxPerMinute int
	GRPCSampleMaxPerMinute int
	// SampleMaxPerMinute preserves the legacy shared quota setting. When set it
	// acts as a fallback for protocols without an explicit per-minute cap.
	SampleMaxPerMinute int
}

// SummarySettings controls optional summary emission.
type SummarySettings struct {
	Interval time.Duration
}

// AuditSettings governs annotation field names.
type AuditSettings struct {
	ReasonField     string
	DecisionField   string
	RuleField       string
	SafetyRailField string
	AuditPrefix     string
}

// AuditKeys contains audit metadata keys and optional prefix information.
type AuditKeys struct {
	Prefix     string
	Decision   string
	Reason     string
	Rule       string
	SafetyRail string
}

// Keys returns the audit key configuration.
func (a AuditSettings) Keys() AuditKeys {
	return AuditKeys{
		Prefix:     strings.TrimSpace(a.AuditPrefix),
		Decision:   strings.TrimSpace(a.DecisionField),
		Reason:     strings.TrimSpace(a.ReasonField),
		Rule:       strings.TrimSpace(a.RuleField),
		SafetyRail: strings.TrimSpace(a.SafetyRailField),
	}
}

// DefaultConfig returns the baseline chatter configuration prior to applying
// environment variables or programmatic overrides.
func DefaultConfig() Config {
	return Config{
		Mode:             ModeAuto,
		Action:           ActionDrop,
		AlwaysLogErrors:  true,
		LatencyThreshold: 500 * time.Millisecond,

		HTTPEnabledOverride: nil,
		GRPCEnabledOverride: nil,

		HTTP: HTTPSettings{
			// Default to an empty ignore list because Google Front Ends that back
			// external HTTP(S) load balancers also originate from the documented
			// health-check IP ranges. Shipping those ranges by default would
			// suppress legitimate user traffic behind a load balancer.
			IgnoreCIDRs:         nil,
			AppEngineSuppressAH: true,
			AppEngineCronAction: ActionMark,
			DefaultsMode:        DefaultsAdditive,
		},
		GRPC: GRPCSettings{
			IgnoreMethods:      append([]string(nil), defaultGRPCMethods...),
			IgnoreOKOnly:       true,
			DefaultsMode:       DefaultsAdditive,
			WatchDedupInterval: 10 * time.Second,
		},
		Proxy: ProxySettings{
			TrustProxy:        false,
			TrustedHops:       1,
			TrustedProxyCIDRs: nil,
			ClientIPHeader:    "X-Forwarded-For",
		},
		Sampling: SamplingSettings{},
		Summary:  SummarySettings{},
		Audit: AuditSettings{
			ReasonField:     "observability.chatter.reason",
			DecisionField:   "observability.chatter.decision",
			RuleField:       "observability.chatter.rule",
			SafetyRailField: "observability.chatter.safety_rail",
		},
	}
}

// Clone produces a deep copy of the configuration.
func (c Config) Clone() Config {
	c.HTTP.IgnoreCIDRs = append([]string(nil), c.HTTP.IgnoreCIDRs...)
	c.HTTP.Paths = append([]string(nil), c.HTTP.Paths...)
	c.HTTP.Prefixes = append([]string(nil), c.HTTP.Prefixes...)
	c.HTTP.Regexes = append([]string(nil), c.HTTP.Regexes...)
	c.HTTP.Headers = append([]HeaderRule(nil), c.HTTP.Headers...)
	c.HTTP.Methods = append([]string(nil), c.HTTP.Methods...)

	c.GRPC.IgnoreMethods = append([]string(nil), c.GRPC.IgnoreMethods...)

	c.Proxy.TrustedProxyCIDRs = append([]string(nil), c.Proxy.TrustedProxyCIDRs...)

	c.configErrors = append([]error(nil), c.configErrors...)
	return c
}

// ConfigFromEnv loads configuration using os.Getenv.
func ConfigFromEnv() Config {
	cfg, _ := LoadConfigFromEnv(os.LookupEnv)
	return cfg
}

// LoadConfigFromEnv applies environment variable overrides to DefaultConfig.
// It returns the resulting configuration along with any parsing errors.
func LoadConfigFromEnv(lookup func(string) (string, bool)) (Config, []error) {
	cfg := DefaultConfig()
	var errs []error

	parseBool := func(raw string) (bool, bool) {
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "true", "1", "yes", "on":
			return true, true
		case "false", "0", "no", "off":
			return false, true
		default:
			return false, false
		}
	}

	parseInt := func(raw string) (int, error) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return 0, strconv.ErrSyntax
		}
		i, err := strconv.Atoi(raw)
		if err != nil {
			return 0, err
		}
		return i, nil
	}

	parseFloat := func(raw string) (float64, error) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return 0, strconv.ErrSyntax
		}
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return 0, err
		}
		return f, nil
	}

	parseDuration := func(raw string) (time.Duration, error) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return 0, strconv.ErrSyntax
		}
		d, err := time.ParseDuration(raw)
		if err != nil {
			return 0, err
		}
		return d, nil
	}

	// Override flags first so subsequent values honour them.
	if raw, ok := lookup(envHTTPOverrideDefaults); ok {
		if v, valid := parseBool(raw); valid && v {
			cfg.HTTP.DefaultsMode = DefaultsOverride
			cfg.HTTP.IgnoreCIDRs = nil
		}
	}
	if raw, ok := lookup(envGRPCOverrideDefaults); ok {
		if v, valid := parseBool(raw); valid && v {
			cfg.GRPC.DefaultsMode = DefaultsOverride
			cfg.GRPC.IgnoreMethods = nil
		}
	}

	if raw, ok := lookup(envMode); ok {
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "auto":
			cfg.Mode = ModeAuto
		case "on":
			cfg.Mode = ModeOn
		case "off":
			cfg.Mode = ModeOff
		default:
			errs = append(errs, fmt.Errorf("%s: invalid mode %q", envMode, raw))
		}
	}
	if raw, ok := lookup(envAction); ok {
		cfg.Action = sanitizeAction(raw, cfg.Action)
	}
	if raw, ok := lookup(envAlwaysLogErrors); ok {
		if v, valid := parseBool(raw); valid {
			cfg.AlwaysLogErrors = v
		} else {
			errs = append(errs, fmt.Errorf("%s: invalid boolean %q", envAlwaysLogErrors, raw))
		}
	}
	if raw, ok := lookup(envLatencyThreshold); ok {
		if d, err := parseDuration(raw); err == nil {
			if d < 0 {
				d = 0
			}
			cfg.LatencyThreshold = d
		} else {
			errs = append(errs, fmt.Errorf("%s: %w", envLatencyThreshold, err))
		}
	}

	if raw, ok := lookup(envHTTPEnabled); ok {
		if v, valid := parseBool(raw); valid {
			cfg.HTTPEnabledOverride = &v
		} else {
			errs = append(errs, fmt.Errorf("%s: invalid boolean %q", envHTTPEnabled, raw))
		}
	}
	if raw, ok := lookup(envGRPCEnabled); ok {
		if v, valid := parseBool(raw); valid {
			cfg.GRPCEnabledOverride = &v
		} else {
			errs = append(errs, fmt.Errorf("%s: invalid boolean %q", envGRPCEnabled, raw))
		}
	}

	if raw, ok := lookup(envHTTPIgnoreIPs); ok {
		cfg.HTTP.IgnoreCIDRs = mergeList(cfg.HTTP.IgnoreCIDRs, raw, cfg.HTTP.DefaultsMode == DefaultsOverride)
		if err := validateCIDRs(cfg.HTTP.IgnoreCIDRs); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", envHTTPIgnoreIPs, err))
		}
	}
	if raw, ok := lookup(envHTTPIgnorePaths); ok {
		cfg.HTTP.Paths = mergeList(cfg.HTTP.Paths, raw, cfg.HTTP.DefaultsMode == DefaultsOverride)
	}
	if raw, ok := lookup(envHTTPIgnorePrefixes); ok {
		cfg.HTTP.Prefixes = mergeList(cfg.HTTP.Prefixes, raw, cfg.HTTP.DefaultsMode == DefaultsOverride)
	}
	if raw, ok := lookup(envHTTPIgnoreRegex); ok {
		cfg.HTTP.Regexes = mergeList(cfg.HTTP.Regexes, raw, cfg.HTTP.DefaultsMode == DefaultsOverride)
	}
	if raw, ok := lookup(envHTTPMethods); ok {
		methods := splitAndTrim(raw)
		for i, m := range methods {
			methods[i] = strings.ToUpper(m)
		}
		cfg.HTTP.Methods = methods
	}
	if raw, ok := lookup(envHTTPIgnoreHeaders); ok {
		headers, err := parseHeaderRules(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", envHTTPIgnoreHeaders, err))
		} else {
			cfg.HTTP.Headers = headers
		}
	}
	if raw, ok := lookup(envAppEngineSuppressAH); ok {
		if v, valid := parseBool(raw); valid {
			cfg.HTTP.AppEngineSuppressAH = v
		} else {
			errs = append(errs, fmt.Errorf("%s: invalid boolean %q", envAppEngineSuppressAH, raw))
		}
	}
	if raw, ok := lookup(envAppEngineCronAction); ok {
		cfg.HTTP.AppEngineCronAction = sanitizeAction(raw, cfg.HTTP.AppEngineCronAction)
	}

	if raw, ok := lookup(envGRPCIgnoreMethods); ok {
		cfg.GRPC.IgnoreMethods = mergeList(cfg.GRPC.IgnoreMethods, raw, cfg.GRPC.DefaultsMode == DefaultsOverride)
	}
	if raw, ok := lookup(envGRPCIgnoreOKOnly); ok {
		if v, valid := parseBool(raw); valid {
			cfg.GRPC.IgnoreOKOnly = v
		} else {
			errs = append(errs, fmt.Errorf("%s: invalid boolean %q", envGRPCIgnoreOKOnly, raw))
		}
	}
	if raw, ok := lookup(envWatchDedupInterval); ok {
		if d, err := parseDuration(raw); err == nil {
			if d < 0 {
				d = 0
			}
			cfg.GRPC.WatchDedupInterval = d
		} else {
			errs = append(errs, fmt.Errorf("%s: %w", envWatchDedupInterval, err))
		}
	}

	if raw, ok := lookup(envTrustProxy); ok {
		if v, valid := parseBool(raw); valid {
			cfg.Proxy.TrustProxy = v
		} else {
			errs = append(errs, fmt.Errorf("%s: invalid boolean %q", envTrustProxy, raw))
		}
	}
	if raw, ok := lookup(envTrustedHops); ok {
		if v, err := parseInt(raw); err == nil {
			if v < 0 {
				v = 0
			}
			cfg.Proxy.TrustedHops = v
		} else {
			errs = append(errs, fmt.Errorf("%s: %w", envTrustedHops, err))
		}
	}
	if raw, ok := lookup(envTrustedProxyCIDRs); ok {
		cfg.Proxy.TrustedProxyCIDRs = mergeList(cfg.Proxy.TrustedProxyCIDRs, raw, true)
		if err := validateCIDRs(cfg.Proxy.TrustedProxyCIDRs); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", envTrustedProxyCIDRs, err))
		}
	}
	if raw, ok := lookup(envClientIPHeader); ok {
		trimmed := strings.TrimSpace(raw)
		if trimmed != "" {
			cfg.Proxy.ClientIPHeader = trimmed
		}
	}

	if raw, ok := lookup(envHTTPSampleKeepEvery); ok {
		if v, err := parseInt(raw); err == nil {
			if v < 0 {
				v = 0
			}
			cfg.Sampling.HTTPSampleKeepEvery = v
		} else {
			errs = append(errs, fmt.Errorf("%s: %w", envHTTPSampleKeepEvery, err))
		}
	}
	if raw, ok := lookup(envGRPCSampleKeepEvery); ok {
		if v, err := parseInt(raw); err == nil {
			if v < 0 {
				v = 0
			}
			cfg.Sampling.GRPCSampleKeepEvery = v
		} else {
			errs = append(errs, fmt.Errorf("%s: %w", envGRPCSampleKeepEvery, err))
		}
	}
	if raw, ok := lookup(envHTTPSampleRate); ok {
		if v, err := parseFloat(raw); err == nil {
			if v < 0 {
				v = 0
			}
			if v > 1 {
				v = 1
			}
			cfg.Sampling.HTTPSampleRate = v
		} else {
			errs = append(errs, fmt.Errorf("%s: %w", envHTTPSampleRate, err))
		}
	}
	if raw, ok := lookup(envGRPCSampleRate); ok {
		if v, err := parseFloat(raw); err == nil {
			if v < 0 {
				v = 0
			}
			if v > 1 {
				v = 1
			}
			cfg.Sampling.GRPCSampleRate = v
		} else {
			errs = append(errs, fmt.Errorf("%s: %w", envGRPCSampleRate, err))
		}
	}
	if raw, ok := lookup(envHTTPSampleMaxPerMinute); ok {
		if v, err := parseInt(raw); err == nil {
			if v < 0 {
				v = 0
			}
			cfg.Sampling.HTTPSampleMaxPerMinute = v
		} else {
			errs = append(errs, fmt.Errorf("%s: %w", envHTTPSampleMaxPerMinute, err))
		}
	}
	if raw, ok := lookup(envGRPCSampleMaxPerMinute); ok {
		if v, err := parseInt(raw); err == nil {
			if v < 0 {
				v = 0
			}
			cfg.Sampling.GRPCSampleMaxPerMinute = v
		} else {
			errs = append(errs, fmt.Errorf("%s: %w", envGRPCSampleMaxPerMinute, err))
		}
	}
	if raw, ok := lookup(envSampleMaxPerMinute); ok {
		if v, err := parseInt(raw); err == nil {
			if v < 0 {
				v = 0
			}
			cfg.Sampling.SampleMaxPerMinute = v
		} else {
			errs = append(errs, fmt.Errorf("%s: %w", envSampleMaxPerMinute, err))
		}
	}

	if raw, ok := lookup(envSummaryInterval); ok {
		if d, err := parseDuration(raw); err == nil {
			if d < 0 {
				d = 0
			}
			cfg.Summary.Interval = d
		} else {
			errs = append(errs, fmt.Errorf("%s: %w", envSummaryInterval, err))
		}
	}

	if raw, ok := lookup(envReasonField); ok {
		cfg.Audit.ReasonField = strings.TrimSpace(raw)
	}
	if raw, ok := lookup(envDecisionField); ok {
		cfg.Audit.DecisionField = strings.TrimSpace(raw)
	}
	if raw, ok := lookup(envRuleField); ok {
		cfg.Audit.RuleField = strings.TrimSpace(raw)
	}
	if raw, ok := lookup(envSafetyRailField); ok {
		cfg.Audit.SafetyRailField = strings.TrimSpace(raw)
	}
	if raw, ok := lookup(envAuditPrefix); ok {
		cfg.Audit.AuditPrefix = strings.TrimSpace(raw)
	}

	cfg.configErrors = append(cfg.configErrors, errs...)
	return cfg, errs
}

// Errors returns configuration errors accumulated during parsing.
func (c Config) Errors() []error {
	return append([]error(nil), c.configErrors...)
}

func mergeList(existing []string, raw string, replace bool) []string {
	values := splitAndTrim(raw)
	if replace {
		return values
	}
	return append(existing, values...)
}

func splitAndTrim(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	return out
}

func parseHeaderRules(raw string) ([]HeaderRule, error) {
	parts := splitAndTrim(raw)
	rules := make([]HeaderRule, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		// Support both key=value and key:value syntaxes so callers can mirror
		// familiar header formats.
		delimiter := strings.Index(part, "=")
		if delimiter == -1 {
			delimiter = strings.Index(part, ":")
		}
		if delimiter > -1 {
			key := strings.TrimSpace(part[:delimiter])
			val := strings.TrimSpace(part[delimiter+1:])
			if key == "" {
				return nil, fmt.Errorf("header key missing in %q", part)
			}
			rules = append(rules, HeaderRule{Key: strings.ToLower(key), Value: val, HasValue: true})
			continue
		}
		rules = append(rules, HeaderRule{Key: strings.ToLower(part)})
	}
	return rules, nil
}

func validateCIDRs(cidrs []string) error {
	for _, raw := range cidrs {
		if raw == "" {
			continue
		}
		if _, _, err := net.ParseCIDR(raw); err != nil {
			return fmt.Errorf("invalid CIDR %q: %w", raw, err)
		}
	}
	return nil
}

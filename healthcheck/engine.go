package healthcheck

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cloud.google.com/go/compute/metadata"
)

// Engine evaluates HTTP and gRPC traffic against the configured chatter rules.
// It is safe for concurrent use.
type Engine struct {
	cfg          Config
	httpFailOpen bool
	grpcFailOpen bool
	httpActive   bool
	grpcActive   bool

	envDetection EnvDetection

	httpMatch httpRuntime
	grpcMatch grpcRuntime
	proxy     proxyRuntime
	sampling  sampler
	metrics   *Metrics

	summaryInterval time.Duration
	lastSummary     SummarySnapshot
	summaryMu       sync.RWMutex
	summaryStop     chan struct{}
	summaryDone     chan struct{}
	summaryOnce     sync.Once

	watchMu    sync.Mutex
	watchState map[watchKey]watchEntry

	now func() time.Time

	configHash string

	warnings      []string
	warningsMutex sync.Mutex
	routeWarnOnce sync.Once
}

// EnvDetection records how environment auto-mode decisions were made.
type EnvDetection struct {
	Method string
	OnGCE  bool
	Hints  []string
}

type httpRuntime struct {
	cidrs             []*net.IPNet
	paths             map[string]struct{}
	prefixes          []string
	regex             []*regexp.Regexp
	headers           []headerMatcher
	methods           map[string]struct{}
	hasMethodLimit    bool
	appEngineCron     Action
	appEngineSuppress bool
}

type headerMatcher struct {
	key    string
	values map[string]struct{} // nil -> presence only
}

func (h headerMatcher) String() string {
	if len(h.values) == 0 {
		return h.key
	}
	for v := range h.values {
		return fmt.Sprintf("%s=%s", h.key, v)
	}
	return h.key
}

type grpcRuntime struct {
	methods      map[string]struct{}
	ignoreOKOnly bool
	watchDedup   time.Duration
}

type proxyRuntime struct {
	trust        bool
	trustedHops  int
	header       string
	trustedCIDRs []*net.IPNet
}

type watchKey struct {
	peer    string
	service string
}

type watchEntry struct {
	lastStatus        string
	lastEmittedStatus string
	lastEmittedAt     time.Time
}

type sampler struct {
	httpKeepEvery    int
	grpcKeepEvery    int
	httpRate         float64
	grpcRate         float64
	httpMaxPerMinute int
	grpcMaxPerMinute int

	httpCount atomic.Uint64
	grpcCount atomic.Uint64

	randomMu sync.Mutex
	random   *rand.Rand

	httpQuotaMu      sync.Mutex
	httpQuotaWindow  time.Time
	httpQuotaCurrent int

	grpcQuotaMu      sync.Mutex
	grpcQuotaWindow  time.Time
	grpcQuotaCurrent int
}

type engineOptions struct {
	onGCE func() bool
	now   func() time.Time
}

// EngineOption mutates engine construction behaviour.
type EngineOption func(*engineOptions)

// WithOnGCEFunc injects a custom environment detector (useful for tests).
func WithOnGCEFunc(fn func() bool) EngineOption {
	return func(o *engineOptions) {
		if fn != nil {
			o.onGCE = fn
		}
	}
}

// WithNowFunc overrides the time source used for sampling buckets.
func WithNowFunc(fn func() time.Time) EngineOption {
	return func(o *engineOptions) {
		if fn != nil {
			o.now = fn
		}
	}
}

// NewEngine constructs a chatter evaluation engine. It honours fail-open
// semantics: configuration errors disable suppression but still allow callers
// to retrieve the warnings for logging.
func NewEngine(cfg Config, opts ...EngineOption) (*Engine, error) {
	cfg = cfg.Clone()

	opt := engineOptions{
		onGCE: metadata.OnGCE,
		now:   time.Now,
	}
	for _, o := range opts {
		if o != nil {
			o(&opt)
		}
	}

	engine := &Engine{
		cfg:     cfg,
		now:     opt.now,
		metrics: NewMetrics(),
	}

	if errs := cfg.Errors(); len(errs) > 0 {
		engine.recordWarning("chatter configuration contains errors; running fail-open")
		engine.httpFailOpen = true
		engine.grpcFailOpen = true
		for _, err := range errs {
			engine.recordWarning(err.Error())
		}
	}

	detection := EnvDetection{
		Method: "metadata.OnGCE",
	}
	defer func() {
		engine.envDetection = detection
	}()

	defer func() {
		engine.configHash = computeConfigHash(cfg, engine.httpActive, engine.grpcActive, detection.OnGCE)
	}()

	if opt.onGCE != nil {
		detection.OnGCE = safeOnGCE(opt.onGCE, engine)
	} else {
		detection.OnGCE = metadata.OnGCE()
	}

	detection.Hints = collectEnvHints()

	engine.httpActive, engine.grpcActive = resolveProtocolEnabling(cfg, detection.OnGCE)
	if engine.httpFailOpen {
		engine.httpActive = false
	}
	if engine.grpcFailOpen {
		engine.grpcActive = false
	}

	if err := engine.buildHTTP(cfg.HTTP); err != nil {
		engine.recordWarning(fmt.Sprintf("http chatter disabled: %v", err))
		engine.httpActive = false
		engine.httpFailOpen = true
	}
	if err := engine.buildGRPC(cfg.GRPC); err != nil {
		engine.recordWarning(fmt.Sprintf("grpc chatter disabled: %v", err))
		engine.grpcActive = false
		engine.grpcFailOpen = true
	}
	if err := engine.buildProxy(cfg.Proxy); err != nil {
		engine.recordWarning(fmt.Sprintf("proxy trust disabled: %v", err))
	}

	httpMax := cfg.Sampling.HTTPSampleMaxPerMinute
	if httpMax == 0 {
		httpMax = cfg.Sampling.SampleMaxPerMinute
	}
	grpcMax := cfg.Sampling.GRPCSampleMaxPerMinute
	if grpcMax == 0 {
		grpcMax = cfg.Sampling.SampleMaxPerMinute
	}

	engine.sampling = sampler{
		httpKeepEvery:    cfg.Sampling.HTTPSampleKeepEvery,
		grpcKeepEvery:    cfg.Sampling.GRPCSampleKeepEvery,
		httpRate:         cfg.Sampling.HTTPSampleRate,
		grpcRate:         cfg.Sampling.GRPCSampleRate,
		httpMaxPerMinute: httpMax,
		grpcMaxPerMinute: grpcMax,
		random:           rand.New(rand.NewSource(time.Now().UnixNano())),
	}

	if cfg.Summary.Interval > 0 {
		engine.startSummaryLoop(cfg.Summary.Interval)
	}

	return engine, nil
}

func (e *Engine) startSummaryLoop(interval time.Duration) {
	if interval <= 0 {
		return
	}
	if e.summaryStop != nil {
		return
	}
	e.summaryInterval = interval
	stop := make(chan struct{})
	done := make(chan struct{})
	e.summaryStop = stop
	e.summaryDone = done

	ticker := time.NewTicker(interval)
	e.captureSummary(e.now())

	go func() {
		defer ticker.Stop()
		defer close(done)
		for {
			select {
			case <-ticker.C:
				e.captureSummary(e.now())
			case <-stop:
				return
			}
		}
	}()
}

func (e *Engine) captureSummary(ts time.Time) {
	if e.metrics == nil {
		return
	}
	summary := SummarySnapshot{
		GeneratedAt:      ts,
		ProxyFailures:    e.metrics.SnapshotProxyFailures(),
		WatchTransitions: nil,
	}

	filteredMap := e.metrics.SnapshotFiltered()
	if len(filteredMap) > 0 {
		filtered := make([]FilteredSummary, 0, len(filteredMap))
		for k, v := range filteredMap {
			filtered = append(filtered, FilteredSummary{
				Protocol: k.Protocol,
				Reason:   k.Reason,
				Action:   k.Action,
				Count:    v,
			})
		}
		sort.Slice(filtered, func(i, j int) bool {
			if filtered[i].Protocol != filtered[j].Protocol {
				return filtered[i].Protocol < filtered[j].Protocol
			}
			if filtered[i].Reason != filtered[j].Reason {
				return filtered[i].Reason < filtered[j].Reason
			}
			if filtered[i].Action != filtered[j].Action {
				return filtered[i].Action < filtered[j].Action
			}
			return false
		})
		summary.Filtered = filtered
	}

	forcedMap := e.metrics.SnapshotForced()
	if len(forcedMap) > 0 {
		forced := make([]ForcedSummary, 0, len(forcedMap))
		for k, v := range forcedMap {
			forced = append(forced, ForcedSummary{
				Protocol:   k.Protocol,
				SafetyRail: k.SafetyRail,
				Count:      v,
			})
		}
		sort.Slice(forced, func(i, j int) bool {
			if forced[i].Protocol != forced[j].Protocol {
				return forced[i].Protocol < forced[j].Protocol
			}
			if forced[i].SafetyRail != forced[j].SafetyRail {
				return forced[i].SafetyRail < forced[j].SafetyRail
			}
			return false
		})
		summary.Forced = forced
	}

	watchMap := e.metrics.SnapshotWatch()
	if len(watchMap) > 0 {
		summary.WatchTransitions = make(map[string]uint64, len(watchMap))
		for k, v := range watchMap {
			summary.WatchTransitions[k] = v
		}
	}

	e.summaryMu.Lock()
	e.lastSummary = summary
	e.summaryMu.Unlock()
}

// SummaryInterval reports the configured summary emission interval.
func (e *Engine) SummaryInterval() time.Duration {
	return e.summaryInterval
}

// LastSummary returns the most recent summary snapshot if one has been
// generated. The returned snapshot is a deep copy safe for caller mutation.
func (e *Engine) LastSummary() (SummarySnapshot, bool) {
	e.summaryMu.RLock()
	defer e.summaryMu.RUnlock()
	if e.lastSummary.GeneratedAt.IsZero() {
		return SummarySnapshot{}, false
	}
	return e.lastSummary.Clone(), true
}

// Shutdown stops the background summary ticker if it is running.
func (e *Engine) Shutdown() {
	e.summaryOnce.Do(func() {
		if e.summaryStop != nil {
			close(e.summaryStop)
			if e.summaryDone != nil {
				<-e.summaryDone
			}
			e.summaryStop = nil
			e.summaryDone = nil
		}
	})
}

func (e *Engine) recordWarning(msg string) {
	e.warningsMutex.Lock()
	defer e.warningsMutex.Unlock()
	e.warnings = append(e.warnings, msg)
}

func safeOnGCE(fn func() bool, e *Engine) bool {
	defer func() {
		if r := recover(); r != nil {
			e.recordWarning(fmt.Sprintf("metadata.OnGCE panicked: %v (fail-open)", r))
		}
	}()
	return fn()
}

func resolveProtocolEnabling(cfg Config, onGCE bool) (httpEnabled, grpcEnabled bool) {
	switch cfg.Mode {
	case ModeOn:
		httpEnabled = true
		grpcEnabled = true
	case ModeOff:
		httpEnabled = false
		grpcEnabled = false
	case ModeAuto:
		httpEnabled = onGCE
		grpcEnabled = onGCE
	default:
		httpEnabled = onGCE
		grpcEnabled = onGCE
	}
	if cfg.HTTPEnabledOverride != nil {
		httpEnabled = *cfg.HTTPEnabledOverride
	}
	if cfg.GRPCEnabledOverride != nil {
		grpcEnabled = *cfg.GRPCEnabledOverride
	}
	return
}

func (e *Engine) buildHTTP(cfg HTTPSettings) error {
	rt := httpRuntime{
		paths:             make(map[string]struct{}, len(cfg.Paths)),
		prefixes:          append([]string(nil), cfg.Prefixes...),
		appEngineCron:     cfg.AppEngineCronAction,
		appEngineSuppress: cfg.AppEngineSuppressAH,
	}
	for _, path := range cfg.Paths {
		if path == "" {
			continue
		}
		rt.paths[path] = struct{}{}
	}
	for _, raw := range cfg.Regexes {
		if raw == "" {
			continue
		}
		re, err := regexp.Compile(raw)
		if err != nil {
			return fmt.Errorf("compile regex %q: %w", raw, err)
		}
		rt.regex = append(rt.regex, re)
	}
	for _, cidr := range cfg.IgnoreCIDRs {
		if cidr == "" {
			continue
		}
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			return fmt.Errorf("parse CIDR %q: %w", cidr, err)
		}
		rt.cidrs = append(rt.cidrs, network)
	}
	rt.headers = make([]headerMatcher, 0, len(cfg.Headers))
	for _, hdr := range cfg.Headers {
		if hdr.Key == "" {
			continue
		}
		hm := headerMatcher{
			key: strings.ToLower(hdr.Key),
		}
		if hdr.HasValue {
			hm.values = map[string]struct{}{
				strings.ToLower(hdr.Value): {},
			}
		}
		rt.headers = append(rt.headers, hm)
	}
	if len(cfg.Methods) > 0 {
		rt.methods = make(map[string]struct{}, len(cfg.Methods))
		for _, method := range cfg.Methods {
			if method == "" {
				continue
			}
			rt.methods[strings.ToUpper(method)] = struct{}{}
		}
		rt.hasMethodLimit = true
	}

	e.httpMatch = rt
	return nil
}

func (e *Engine) buildGRPC(cfg GRPCSettings) error {
	rt := grpcRuntime{
		methods:      make(map[string]struct{}, len(cfg.IgnoreMethods)),
		ignoreOKOnly: cfg.IgnoreOKOnly,
		watchDedup:   cfg.WatchDedupInterval,
	}
	for _, m := range cfg.IgnoreMethods {
		if m == "" {
			continue
		}
		rt.methods[m] = struct{}{}
	}
	e.grpcMatch = rt
	return nil
}

func (e *Engine) buildProxy(cfg ProxySettings) error {
	rt := proxyRuntime{
		trust:       cfg.TrustProxy,
		trustedHops: cfg.TrustedHops,
		header:      strings.TrimSpace(cfg.ClientIPHeader),
	}
	if rt.header == "" {
		rt.header = "X-Forwarded-For"
	}
	for _, raw := range cfg.TrustedProxyCIDRs {
		if raw == "" {
			continue
		}
		_, network, err := net.ParseCIDR(raw)
		if err != nil {
			return fmt.Errorf("parse trusted proxy CIDR %q: %w", raw, err)
		}
		rt.trustedCIDRs = append(rt.trustedCIDRs, network)
	}
	e.proxy = rt
	return nil
}

// Metrics returns the engine metrics collector.
func (e *Engine) Metrics() *Metrics {
	return e.metrics
}

// HTTPEnabled reports whether chatter suppression is active for HTTP.
func (e *Engine) HTTPEnabled() bool {
	return e.httpActive && !e.httpFailOpen
}

// GRPCEnabled reports whether chatter suppression is active for gRPC.
func (e *Engine) GRPCEnabled() bool {
	return e.grpcActive && !e.grpcFailOpen
}

// FailOpen reports whether configuration issues forced suppression off.
func (e *Engine) FailOpen() bool {
	return e.httpFailOpen || e.grpcFailOpen
}

// Warnings returns a copy of accumulated warnings.
func (e *Engine) Warnings() []string {
	e.warningsMutex.Lock()
	defer e.warningsMutex.Unlock()
	return append([]string(nil), e.warnings...)
}

// ConfigHash returns the stable hash representing the effective configuration.
func (e *Engine) ConfigHash() string {
	return e.configHash
}

// EffectiveConfig returns a flat map of effective configuration keys for
// startup logging.
func (e *Engine) EffectiveConfig() map[string]string {
	out := map[string]string{
		"mode":               string(e.cfg.Mode),
		"action":             string(e.cfg.Action),
		"http_enabled":       strconv.FormatBool(e.HTTPEnabled()),
		"grpc_enabled":       strconv.FormatBool(e.GRPCEnabled()),
		"always_log_errors":  strconv.FormatBool(e.cfg.AlwaysLogErrors),
		"latency_threshold":  e.cfg.LatencyThreshold.String(),
		"http_defaults_mode": string(e.cfg.HTTP.DefaultsMode),
		"grpc_defaults_mode": string(e.cfg.GRPC.DefaultsMode),
		"summary_interval":   e.cfg.Summary.Interval.String(),
		"reason_field":       e.cfg.Audit.ReasonField,
		"decision_field":     e.cfg.Audit.DecisionField,
		"rule_field":         e.cfg.Audit.RuleField,
		"safety_rail_field":  e.cfg.Audit.SafetyRailField,
		"audit_prefix":       e.cfg.Audit.AuditPrefix,
		"env_on_gce":         strconv.FormatBool(e.envDetection.OnGCE),
		"env_method":         e.envDetection.Method,
		"env_hints":          strings.Join(e.envDetection.Hints, ","),
		"config_hash":        e.configHash,
	}

	if len(e.cfg.HTTP.IgnoreCIDRs) > 0 {
		out["http_ignore_cidrs"] = strings.Join(e.cfg.HTTP.IgnoreCIDRs, ",")
	}
	if len(e.cfg.GRPC.IgnoreMethods) > 0 {
		out["grpc_ignore_methods"] = strings.Join(e.cfg.GRPC.IgnoreMethods, ",")
	}
	if len(e.cfg.HTTP.Paths) > 0 {
		out["http_ignore_paths"] = strings.Join(e.cfg.HTTP.Paths, ",")
	}
	if len(e.cfg.HTTP.Prefixes) > 0 {
		out["http_ignore_prefixes"] = strings.Join(e.cfg.HTTP.Prefixes, ",")
	}
	if len(e.cfg.HTTP.Regexes) > 0 {
		out["http_ignore_regex"] = strings.Join(e.cfg.HTTP.Regexes, ",")
	}
	if len(e.cfg.HTTP.Methods) > 0 {
		out["http_methods"] = strings.Join(e.cfg.HTTP.Methods, ",")
	}
	if len(e.cfg.Proxy.TrustedProxyCIDRs) > 0 {
		out["trusted_proxy_cidrs"] = strings.Join(e.cfg.Proxy.TrustedProxyCIDRs, ",")
	}
	out["trust_proxy"] = strconv.FormatBool(e.cfg.Proxy.TrustProxy)
	out["trusted_hops"] = strconv.Itoa(e.cfg.Proxy.TrustedHops)
	out["client_ip_header"] = e.cfg.Proxy.ClientIPHeader
	out["http_appengine_suppress_ah"] = strconv.FormatBool(e.cfg.HTTP.AppEngineSuppressAH)
	out["http_appengine_cron_action"] = string(e.cfg.HTTP.AppEngineCronAction)
	out["grpc_ignore_ok_only"] = strconv.FormatBool(e.cfg.GRPC.IgnoreOKOnly)
	out["grpc_watch_dedup"] = e.cfg.GRPC.WatchDedupInterval.String()
	out["http_sample_keep_every"] = strconv.Itoa(e.cfg.Sampling.HTTPSampleKeepEvery)
	out["grpc_sample_keep_every"] = strconv.Itoa(e.cfg.Sampling.GRPCSampleKeepEvery)
	out["http_sample_rate"] = floatToString(e.cfg.Sampling.HTTPSampleRate)
	out["grpc_sample_rate"] = floatToString(e.cfg.Sampling.GRPCSampleRate)
	out["http_sample_max_per_minute"] = strconv.Itoa(e.cfg.Sampling.HTTPSampleMaxPerMinute)
	out["grpc_sample_max_per_minute"] = strconv.Itoa(e.cfg.Sampling.GRPCSampleMaxPerMinute)
	if e.cfg.Sampling.SampleMaxPerMinute > 0 {
		out["sample_max_per_minute"] = strconv.Itoa(e.cfg.Sampling.SampleMaxPerMinute)
	}
	return out
}

func floatToString(f float64) string {
	if f == 0 {
		return "0"
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func computeConfigHash(cfg Config, httpEnabled, grpcEnabled, onGCE bool) string {
	builder := strings.Builder{}
	appendKV := func(k, v string) {
		builder.WriteString(k)
		builder.WriteByte('=')
		builder.WriteString(v)
		builder.WriteByte('\n')
	}
	appendKV("mode", string(cfg.Mode))
	appendKV("action", string(cfg.Action))
	appendKV("http_enabled", strconv.FormatBool(httpEnabled))
	appendKV("grpc_enabled", strconv.FormatBool(grpcEnabled))
	appendKV("always_log_errors", strconv.FormatBool(cfg.AlwaysLogErrors))
	appendKV("latency_threshold", cfg.LatencyThreshold.String())
	appendKV("http_defaults_mode", string(cfg.HTTP.DefaultsMode))
	appendKV("grpc_defaults_mode", string(cfg.GRPC.DefaultsMode))
	appendKV("http_ignore_cidrs", strings.Join(cfg.HTTP.IgnoreCIDRs, ","))
	appendKV("http_paths", strings.Join(cfg.HTTP.Paths, ","))
	appendKV("http_prefixes", strings.Join(cfg.HTTP.Prefixes, ","))
	appendKV("http_regex", strings.Join(cfg.HTTP.Regexes, ","))
	appendKV("http_methods", strings.Join(cfg.HTTP.Methods, ","))
	appendKV("grpc_methods", strings.Join(cfg.GRPC.IgnoreMethods, ","))
	appendKV("grpc_ignore_ok_only", strconv.FormatBool(cfg.GRPC.IgnoreOKOnly))
	appendKV("grpc_watch_dedup", cfg.GRPC.WatchDedupInterval.String())
	appendKV("trust_proxy", strconv.FormatBool(cfg.Proxy.TrustProxy))
	appendKV("trusted_hops", strconv.Itoa(cfg.Proxy.TrustedHops))
	appendKV("trusted_proxy_cidrs", strings.Join(cfg.Proxy.TrustedProxyCIDRs, ","))
	appendKV("client_ip_header", cfg.Proxy.ClientIPHeader)
	appendKV("summary_interval", cfg.Summary.Interval.String())
	appendKV("sample_http_keep_every", strconv.Itoa(cfg.Sampling.HTTPSampleKeepEvery))
	appendKV("sample_grpc_keep_every", strconv.Itoa(cfg.Sampling.GRPCSampleKeepEvery))
	appendKV("sample_http_rate", floatToString(cfg.Sampling.HTTPSampleRate))
	appendKV("sample_grpc_rate", floatToString(cfg.Sampling.GRPCSampleRate))
	appendKV("sample_http_max_per_minute", strconv.Itoa(cfg.Sampling.HTTPSampleMaxPerMinute))
	appendKV("sample_grpc_max_per_minute", strconv.Itoa(cfg.Sampling.GRPCSampleMaxPerMinute))
	appendKV("sample_max_per_minute", strconv.Itoa(cfg.Sampling.SampleMaxPerMinute))
	appendKV("audit_reason_field", cfg.Audit.ReasonField)
	appendKV("audit_decision_field", cfg.Audit.DecisionField)
	appendKV("audit_rule_field", cfg.Audit.RuleField)
	appendKV("audit_safety_rail_field", cfg.Audit.SafetyRailField)
	appendKV("audit_prefix", cfg.Audit.AuditPrefix)
	appendKV("appengine_suppress_ah", strconv.FormatBool(cfg.HTTP.AppEngineSuppressAH))
	appendKV("appengine_cron_action", string(cfg.HTTP.AppEngineCronAction))
	appendKV("on_gce", strconv.FormatBool(onGCE))

	sum := sha256.Sum256([]byte(builder.String()))
	return hex.EncodeToString(sum[:8])
}

func collectEnvHints() []string {
	hints := []string{}
	add := func(key string) {
		if val, ok := os.LookupEnv(key); ok && strings.TrimSpace(val) != "" {
			hints = append(hints, key)
		}
	}
	add("GOOGLE_CLOUD_PROJECT")
	add("GOOGLE_CLOUD_REGION")
	add("K_SERVICE")
	add("K_REVISION")
	add("GAE_ENV")
	add("GCP_PROJECT")
	return hints
}

// EvaluateHTTP inspects the request and returns a mutable decision pointer. It
// never returns nil. Callers should subsequently invoke FinalizeHTTP with the
// same pointer.
func (e *Engine) EvaluateHTTP(req *http.Request) *Decision {
	decision := &Decision{
		Protocol: ProtocolHTTP,
	}
	if req == nil || !e.HTTPEnabled() {
		return decision
	}

	clientIP, trustOK := e.extractClientIP(req.RemoteAddr, req.Header.Values(e.proxy.header))
	if !trustOK {
		e.metrics.IncProxyTrustFailure()
	}

	if matchCIDR(clientIP, e.httpMatch.cidrs) {
		decision.Matched = true
		decision.Reason = ReasonLBIP
		decision.Rule = clientIP
	} else if cronDecision := matchAppEngineCron(req, e.cfg.HTTP.AppEngineCronAction); cronDecision != nil {
		decision = cronDecision
	} else if aeDecision := matchAppEnginePath(req, e.httpMatch.appEngineSuppress); aeDecision != nil {
		decision = aeDecision
	} else if methodAllowed(req.Method, e.httpMatch.methods, e.httpMatch.hasMethodLimit) {
		if reason, rule, ok := matchHTTPUserRules(req, e.httpMatch); ok {
			decision.Matched = true
			decision.Reason = reason
			decision.Rule = rule
		}
	}

	if !decision.Matched {
		return decision
	}
	if decision.Action == actionUnknown {
		decision.Action = e.cfg.Action
		decision.EffectiveAction = decision.Action
	}
	e.ensureSupportedAction(decision)
	decision = e.applySampling(decision, ProtocolHTTP)
	return decision
}

func (e *Engine) extractClientIP(remoteAddr string, forwarded []string) (string, bool) {
	host := parseIP(remoteAddr)
	if !e.proxy.trust {
		return host, true
	}
	if len(e.proxy.trustedCIDRs) == 0 {
		return host, false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return host, false
	}
	trusted := false
	for _, cidr := range e.proxy.trustedCIDRs {
		if cidr != nil && cidr.Contains(ip) {
			trusted = true
			break
		}
	}
	if !trusted {
		return host, false
	}
	if len(forwarded) == 0 {
		return host, false
	}
	client := selectForwardedIP(forwarded, e.proxy.trustedHops)
	if client == "" {
		return host, false
	}
	if net.ParseIP(client) == nil {
		return host, false
	}
	return client, true
}

func selectForwardedIP(values []string, trustedHops int) string {
	if trustedHops < 0 {
		trustedHops = 0
	}
	tokens := make([]string, 0, len(values))
	for _, raw := range values {
		parts := strings.Split(raw, ",")
		for _, part := range parts {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				tokens = append(tokens, trimmed)
			}
		}
	}
	if len(tokens) == 0 {
		return ""
	}
	index := len(tokens) - trustedHops - 1
	if index < 0 || index >= len(tokens) {
		return ""
	}
	return tokens[index]
}

func matchCIDR(ip string, cidrs []*net.IPNet) bool {
	if ip == "" || len(cidrs) == 0 {
		return false
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, cidr := range cidrs {
		if cidr != nil && cidr.Contains(parsed) {
			return true
		}
	}
	return false
}

func matchAppEngineCron(req *http.Request, action Action) *Decision {
	if req == nil {
		return nil
	}
	val := req.Header.Get("X-Appengine-Cron")
	if val == "" {
		return nil
	}
	if strings.EqualFold(strings.TrimSpace(val), "true") || strings.EqualFold(strings.TrimSpace(val), "1") {
		decision := &Decision{
			Matched:         true,
			Protocol:        ProtocolHTTP,
			Action:          action,
			EffectiveAction: action,
			Reason:          ReasonAppEngineCron,
			Rule:            "X-Appengine-Cron",
			markOnly:        action != ActionDrop,
		}
		return decision
	}
	return nil
}

func matchAppEnginePath(req *http.Request, suppress bool) *Decision {
	if !suppress || req == nil {
		return nil
	}
	if strings.HasPrefix(req.URL.Path, "/_ah/") {
		return &Decision{
			Matched:         true,
			Protocol:        ProtocolHTTP,
			Action:          ActionDrop,
			EffectiveAction: ActionDrop,
			Reason:          ReasonAppEngineAH,
			Rule:            req.URL.Path,
		}
	}
	return nil
}

func methodAllowed(method string, allowed map[string]struct{}, limited bool) bool {
	if !limited {
		return true
	}
	_, ok := allowed[strings.ToUpper(method)]
	return ok
}

func matchHTTPUserRules(req *http.Request, rt httpRuntime) (Reason, string, bool) {
	if req == nil {
		return ReasonUnknown, "", false
	}
	if _, ok := rt.paths[req.URL.Path]; ok {
		return ReasonPath, req.URL.Path, true
	}
	for _, prefix := range rt.prefixes {
		if strings.HasPrefix(req.URL.Path, prefix) {
			return ReasonPrefix, prefix, true
		}
	}
	for _, re := range rt.regex {
		if re.MatchString(req.URL.Path) {
			return ReasonRegex, re.String(), true
		}
	}
	if len(rt.headers) > 0 {
		for _, matcher := range rt.headers {
			if matcher.match(req.Header) {
				return ReasonHeader, matcher.String(), true
			}
		}
	}
	return ReasonUnknown, "", false
}

func (hm headerMatcher) match(header http.Header) bool {
	if header == nil {
		return false
	}
	values := header.Values(hm.headerKey())
	if len(values) == 0 {
		return false
	}
	if hm.values == nil {
		return true
	}
	for _, raw := range values {
		if _, ok := hm.values[strings.ToLower(strings.TrimSpace(raw))]; ok {
			return true
		}
	}
	return false
}

func (hm headerMatcher) headerKey() string {
	if hm.key == "" {
		return hm.key
	}
	return http.CanonicalHeaderKey(hm.key)
}

func (e *Engine) applySampling(decision *Decision, protocol Protocol) *Decision {
	if decision == nil || !decision.Matched {
		return decision
	}
	switch protocol {
	case ProtocolHTTP:
		if e.sampling.shouldKeepHTTP(e.now()) {
			decision.EffectiveAction = ActionMark
			decision.markOnly = true
			return decision
		}
	case ProtocolGRPC:
		if e.sampling.shouldKeepGRPC(e.now()) {
			decision.EffectiveAction = ActionMark
			decision.markOnly = true
			return decision
		}
	}
	return decision
}

// FinalizeHTTP applies safety rails and updates metrics using the provided
// status and latency.
func (e *Engine) FinalizeHTTP(decision *Decision, status int, latency time.Duration) {
	if decision == nil {
		return
	}
	if decision.Matched {
		e.metrics.IncFiltered(decision.Protocol, decision.Reason, decision.ActiveAction())
	}

	if !decision.Matched || !e.cfg.AlwaysLogErrors {
		goto latencyCheck
	}
	if status >= 400 {
		*decision = decision.WithSafetyRail(SafetyRailError)
		e.metrics.IncForced(decision.Protocol, SafetyRailError)
	}

latencyCheck:
	if decision.Matched && e.cfg.LatencyThreshold > 0 && latency >= e.cfg.LatencyThreshold {
		*decision = decision.WithSafetyRail(SafetyRailLatency)
		decision.forcedSeverityHint = "WARN"
		e.metrics.IncForced(decision.Protocol, SafetyRailLatency)
	}
}

// EvaluateGRPC analyses the incoming RPC metadata to decide if chatter rules
// apply. peerAddr should be the raw peer address (typically ip:port).
func (e *Engine) EvaluateGRPC(method string, metadata map[string][]string, peerAddr string) *Decision {
	decision := &Decision{
		Protocol: ProtocolGRPC,
		Action:   e.cfg.Action,
	}
	if !e.GRPCEnabled() || method == "" {
		return decision
	}

	clientIP, trustOK := e.extractClientIP(peerAddr, metadata[e.proxy.headerLower()])
	if !trustOK {
		e.metrics.IncProxyTrustFailure()
	}
	if _, ok := e.grpcMatch.methods[method]; ok {
		decision.Matched = true
		decision.Reason = ReasonGRPCHealth
		decision.Rule = method
		decision.ignoreStatusOKOnly = e.grpcMatch.ignoreOKOnly
	} else if matchCIDR(clientIP, e.httpMatch.cidrs) {
		decision.Matched = true
		decision.Reason = ReasonLBIP
		decision.Rule = clientIP
	}

	if !decision.Matched {
		return decision
	}
	decision.EffectiveAction = decision.Action
	e.ensureSupportedAction(decision)
	return e.applySampling(decision, ProtocolGRPC)
}

func (e *Engine) ensureSupportedAction(decision *Decision) {
	if decision == nil || !decision.Matched {
		return
	}
	if decision.Action == actionUnknown {
		decision.Action = e.cfg.Action
	}
	if decision.EffectiveAction == actionUnknown {
		decision.EffectiveAction = decision.Action
	}
	if decision.ActiveAction() == ActionRoute {
		decision.warnRouteFallback = true
		decision.EffectiveAction = ActionMark
		e.routeWarnOnce.Do(func() {
			e.recordWarning("chatter action=route not implemented; falling back to mark")
		})
	}
}

func (p proxyRuntime) headerLower() string {
	if p.header == "" {
		return "x-forwarded-for"
	}
	return strings.ToLower(p.header)
}

// FinalizeGRPC applies safety rails based on the final status code and latency.
func (e *Engine) FinalizeGRPC(decision *Decision, code string, latency time.Duration, ok bool) {
	if decision == nil {
		return
	}
	forcedError := false
	if decision.Matched && decision.ignoreStatusOKOnly && !ok {
		*decision = decision.WithSafetyRail(SafetyRailError)
		forcedError = true
	}
	if decision.Matched {
		e.metrics.IncFiltered(decision.Protocol, decision.Reason, decision.ActiveAction())
	}
	if decision.Matched && e.cfg.AlwaysLogErrors && !ok {
		if !forcedError {
			*decision = decision.WithSafetyRail(SafetyRailError)
			forcedError = true
		}
	}
	if forcedError {
		*decision = decision.WithSafetyRail(SafetyRailError)
		e.metrics.IncForced(decision.Protocol, SafetyRailError)
	}
	if decision.Matched && e.cfg.LatencyThreshold > 0 && latency >= e.cfg.LatencyThreshold {
		*decision = decision.WithSafetyRail(SafetyRailLatency)
		decision.forcedSeverityHint = "WARN"
		e.metrics.IncForced(decision.Protocol, SafetyRailLatency)
	}
}

// RecordGRPCWatchStatus tracks transitions emitted by the gRPC health check
// Watch method. It returns a structured event indicating whether callers should
// log the update after applying the configured de-duplication interval.
func (e *Engine) RecordGRPCWatchStatus(peer, service, status string, ts time.Time) WatchEvent {
	event := WatchEvent{
		Peer:    strings.TrimSpace(peer),
		Service: strings.TrimSpace(service),
	}
	canonicalStatus := strings.ToUpper(strings.TrimSpace(status))
	event.Status = canonicalStatus

	if canonicalStatus == "" {
		return event
	}
	event.Level = watchStatusLevel(canonicalStatus)
	if ts.IsZero() {
		ts = e.now()
	}
	event.Timestamp = ts

	e.watchMu.Lock()
	defer e.watchMu.Unlock()

	if e.watchState == nil {
		e.watchState = make(map[watchKey]watchEntry)
	}

	key := watchKey{peer: event.Peer, service: event.Service}
	entry := e.watchState[key]
	event.PreviousStatus = entry.lastStatus

	if canonicalStatus == entry.lastStatus {
		e.watchState[key] = entry
		return event
	}

	event.Transition = true
	entry.lastStatus = canonicalStatus

	dedup := e.grpcMatch.watchDedup
	shouldLog := true
	if dedup > 0 && entry.lastEmittedStatus == canonicalStatus && !entry.lastEmittedAt.IsZero() && ts.Sub(entry.lastEmittedAt) < dedup {
		shouldLog = false
		event.Suppressed = true
	}

	if shouldLog {
		entry.lastEmittedStatus = canonicalStatus
		entry.lastEmittedAt = ts
		event.ShouldLog = true
		event.Level = watchStatusLevel(canonicalStatus)
		e.metrics.IncWatchTransition(canonicalStatus)
	}

	e.watchState[key] = entry
	return event
}

func watchStatusLevel(status string) string {
	switch status {
	case "NOT_SERVING", "SERVICE_UNKNOWN", "UNKNOWN":
		return "WARN"
	default:
		return "INFO"
	}
}

func (s *sampler) shouldKeepHTTP(now time.Time) bool {
	if s.httpKeepEvery > 0 {
		n := s.httpCount.Add(1)
		if n%uint64(s.httpKeepEvery) == 0 {
			return s.reserveHTTP(now)
		}
		return false
	}
	if s.httpRate > 0 {
		if s.randomFloat() < s.httpRate {
			return s.reserveHTTP(now)
		}
	}
	return false
}

func (s *sampler) shouldKeepGRPC(now time.Time) bool {
	if s.grpcKeepEvery > 0 {
		n := s.grpcCount.Add(1)
		if n%uint64(s.grpcKeepEvery) == 0 {
			return s.reserveGRPC(now)
		}
		return false
	}
	if s.grpcRate > 0 {
		if s.randomFloat() < s.grpcRate {
			return s.reserveGRPC(now)
		}
	}
	return false
}

func (s *sampler) randomFloat() float64 {
	s.randomMu.Lock()
	defer s.randomMu.Unlock()
	return s.random.Float64()
}

func (s *sampler) reserveHTTP(now time.Time) bool {
	if s.httpMaxPerMinute <= 0 {
		return true
	}
	s.httpQuotaMu.Lock()
	defer s.httpQuotaMu.Unlock()
	if s.httpQuotaWindow.IsZero() || now.Sub(s.httpQuotaWindow) >= time.Minute {
		s.httpQuotaWindow = now
		s.httpQuotaCurrent = 0
	}
	if s.httpQuotaCurrent >= s.httpMaxPerMinute {
		return false
	}
	s.httpQuotaCurrent++
	return true
}

func (s *sampler) reserveGRPC(now time.Time) bool {
	if s.grpcMaxPerMinute <= 0 {
		return true
	}
	s.grpcQuotaMu.Lock()
	defer s.grpcQuotaMu.Unlock()
	if s.grpcQuotaWindow.IsZero() || now.Sub(s.grpcQuotaWindow) >= time.Minute {
		s.grpcQuotaWindow = now
		s.grpcQuotaCurrent = 0
	}
	if s.grpcQuotaCurrent >= s.grpcMaxPerMinute {
		return false
	}
	s.grpcQuotaCurrent++
	return true
}

func parseIP(input string) string {
	if input == "" {
		return ""
	}
	if strings.HasPrefix(input, "[") {
		if idx := strings.Index(input, "]"); idx > 0 {
			candidate := input[1:idx]
			if net.ParseIP(candidate) != nil {
				return candidate
			}
		}
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(input))
	if err == nil {
		if net.ParseIP(host) != nil {
			return host
		}
	}
	if net.ParseIP(input) != nil {
		return input
	}
	return input
}

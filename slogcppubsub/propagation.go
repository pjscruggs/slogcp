// Copyright 2025 Patrick J. Scruggs
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package slogcppubsub

import (
	"context"
	"strings"

	"cloud.google.com/go/pubsub"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const googclientPrefix = "googclient_"

// Inject injects trace context from ctx into msg.Attributes, creating the
// attributes map when necessary.
func Inject(ctx context.Context, msg *pubsub.Message, opts ...Option) {
	if msg == nil {
		return
	}
	msg.Attributes = InjectAttributes(ctx, msg.Attributes, opts...)
}

// InjectAttributes injects trace context from ctx into attrs and returns the
// map. The configured propagator determines what keys are emitted (for example,
// W3C Trace Context and baggage). When no propagation data is present on ctx,
// attrs is returned unchanged.
func InjectAttributes(ctx context.Context, attrs map[string]string, opts ...Option) map[string]string {
	cfg := applyOptions(opts)
	return injectAttributes(ctx, attrs, cfg)
}

func injectAttributes(ctx context.Context, attrs map[string]string, cfg *config) map[string]string {
	if cfg != nil && !cfg.propagateTrace {
		return attrs
	}
	if ctx == nil {
		return attrs
	}

	if cfg != nil && cfg.injectOnlyIfSpanPresent && !trace.SpanContextFromContext(ctx).IsValid() {
		return attrs
	}

	propagator := cfg.propagators
	if propagator == nil {
		propagator = otel.GetTextMapPropagator()
	}
	if propagator != nil {
		propagator.Inject(ctx, lazyCarrier{attrs: &attrs, allowBaggage: cfg == nil || cfg.propagateBaggage})
	}

	if cfg != nil && cfg.googClientInjection {
		propagation.TraceContext{}.Inject(ctx, lazyCarrier{attrs: &attrs, prefix: googclientPrefix, allowBaggage: cfg == nil || cfg.propagateBaggage})
	}

	return attrs
}

// Extract extracts trace context from msg.Attributes into ctx and returns the
// updated context plus the discovered span context (if any).
func Extract(ctx context.Context, msg *pubsub.Message, opts ...Option) (context.Context, trace.SpanContext) {
	if msg == nil {
		return ctx, trace.SpanContextFromContext(ctx)
	}
	return ExtractAttributes(ctx, msg.Attributes, opts...)
}

// ExtractAttributes extracts trace context from attrs into ctx and returns the
// updated context plus the discovered span context (if any).
func ExtractAttributes(ctx context.Context, attrs map[string]string, opts ...Option) (context.Context, trace.SpanContext) {
	cfg := applyOptions(opts)
	return ensureSpanContext(ctx, attrs, cfg, true)
}

func ensureSpanContext(ctx context.Context, attrs map[string]string, cfg *config, forceExtract bool) (context.Context, trace.SpanContext) {
	if cfg != nil && !cfg.propagateTrace {
		return ctx, trace.SpanContextFromContext(ctx)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	current := trace.SpanContextFromContext(ctx)
	if current.IsValid() && !forceExtract {
		return ctx, current
	}
	if len(attrs) == 0 {
		return ctx, current
	}

	propagator := cfg.propagators
	if propagator == nil {
		propagator = otel.GetTextMapPropagator()
	}
	if propagator != nil {
		var carrier propagation.TextMapCarrier
		if cfg != nil && cfg.caseInsensitiveExtraction {
			carrier = &caseInsensitiveCarrier{attrs: attrs, allowBaggage: cfg.propagateBaggage}
		} else {
			carrier = strictCarrier{attrs: attrs, allowBaggage: cfg == nil || cfg.propagateBaggage}
		}
		extracted := propagator.Extract(ctx, carrier)
		sc := trace.SpanContextFromContext(extracted)
		if sc.IsValid() {
			return extracted, sc
		}
	}

	if cfg != nil && cfg.googClientExtraction {
		var carrier propagation.TextMapCarrier
		if cfg.caseInsensitiveExtraction {
			carrier = &caseInsensitiveCarrier{attrs: attrs, prefix: googclientPrefix}
		} else {
			carrier = strictCarrier{attrs: attrs, prefix: googclientPrefix}
		}
		extracted := propagation.TraceContext{}.Extract(ctx, carrier)
		sc := trace.SpanContextFromContext(extracted)
		if sc.IsValid() {
			return extracted, sc
		}
	}

	return ctx, trace.SpanContextFromContext(ctx)
}

type lazyCarrier struct {
	attrs        *map[string]string
	prefix       string
	allowBaggage bool
}

func (c lazyCarrier) Get(key string) string {
	if c.attrs == nil || *c.attrs == nil {
		return ""
	}
	m := *c.attrs
	key = strings.ToLower(key)
	if !c.allowBaggage && key == "baggage" {
		return ""
	}
	if c.prefix != "" {
		key = strings.ToLower(c.prefix) + key
	}
	return m[key]
}

func (c lazyCarrier) Set(key, value string) {
	if c.attrs == nil {
		return
	}
	m := *c.attrs
	key = strings.ToLower(key)
	if !c.allowBaggage && key == "baggage" {
		return
	}
	if m == nil {
		m = make(map[string]string)
		*c.attrs = m
	}
	if c.prefix != "" {
		key = strings.ToLower(c.prefix) + key
	}
	m[key] = value
}

func (c lazyCarrier) Keys() []string {
	if c.attrs == nil || len(*c.attrs) == 0 {
		return nil
	}
	m := *c.attrs
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

type caseInsensitiveCarrier struct {
	attrs        map[string]string
	prefix       string
	allowBaggage bool
	lower        map[string]string
}

func (c *caseInsensitiveCarrier) Get(key string) string {
	if len(c.attrs) == 0 {
		return ""
	}

	lowerKey := strings.ToLower(key)
	if !c.allowBaggage && lowerKey == "baggage" {
		return ""
	}
	if c.prefix == "" {
		if value, ok := c.attrs[key]; ok {
			return value
		}
		if value, ok := c.attrs[lowerKey]; ok {
			return value
		}
		return c.lookupLower(lowerKey)
	}

	prefix := strings.ToLower(c.prefix)
	prefixedKey := prefix + lowerKey
	if value, ok := c.attrs[prefixedKey]; ok {
		return value
	}
	return c.lookupLower(prefixedKey)
}

func (c *caseInsensitiveCarrier) Set(key, value string) {
	if c.attrs == nil {
		return
	}
	lowerKey := strings.ToLower(key)
	if !c.allowBaggage && lowerKey == "baggage" {
		return
	}
	if c.prefix != "" {
		lowerKey = strings.ToLower(c.prefix) + lowerKey
	}
	c.attrs[lowerKey] = value
}

func (c *caseInsensitiveCarrier) Keys() []string {
	if len(c.attrs) == 0 {
		return nil
	}
	keys := make([]string, 0, len(c.attrs))
	for k := range c.attrs {
		keys = append(keys, k)
	}
	return keys
}

func (c *caseInsensitiveCarrier) lookupLower(key string) string {
	if c.lower == nil {
		c.lower = make(map[string]string, len(c.attrs))
		for k, v := range c.attrs {
			c.lower[strings.ToLower(k)] = v
		}
	}
	return c.lower[key]
}

type strictCarrier struct {
	attrs        map[string]string
	prefix       string
	allowBaggage bool
}

func (c strictCarrier) Get(key string) string {
	if len(c.attrs) == 0 {
		return ""
	}
	if !c.allowBaggage && strings.EqualFold(key, "baggage") {
		return ""
	}
	if c.prefix != "" {
		key = c.prefix + key
	}
	return c.attrs[key]
}

func (c strictCarrier) Set(key, value string) {
	if c.attrs == nil {
		return
	}
	if !c.allowBaggage && strings.EqualFold(key, "baggage") {
		return
	}
	if c.prefix != "" {
		key = c.prefix + key
	}
	c.attrs[key] = value
}

func (c strictCarrier) Keys() []string {
	if len(c.attrs) == 0 {
		return nil
	}
	keys := make([]string, 0, len(c.attrs))
	for k := range c.attrs {
		keys = append(keys, k)
	}
	return keys
}

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

package gcp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

const maxProtoStructDepth = 32

// payloadToStruct attempts to convert the provided payload map into a Struct proto.
// Returns false when an unsupported value is encountered so callers can fall back
// to the map representation (which will be JSON-marshalled by the upstream client).
func payloadToStruct(payload map[string]any) (*structpb.Struct, bool) {
	fields := make(map[string]*structpb.Value, len(payload))
	for key, value := range payload {
		v, ok := anyToProtoValue(value, 0)
		if !ok {
			return nil, false
		}
		fields[key] = v
	}
	return &structpb.Struct{Fields: fields}, true
}

func slogValueToProto(v slog.Value) (*structpb.Value, bool) {
	return slogValueToProtoWithDepth(v, 0)
}

func slogValueToProtoWithDepth(v slog.Value, depth int) (*structpb.Value, bool) {
	if depth > maxProtoStructDepth {
		return nil, false
	}
	resolved := v.Resolve()
	switch resolved.Kind() {
	case slog.KindBool:
		return structpb.NewBoolValue(resolved.Bool()), true
	case slog.KindDuration:
		return structpb.NewStringValue(resolved.Duration().String()), true
	case slog.KindFloat64:
		return structpb.NewNumberValue(resolved.Float64()), true
	case slog.KindInt64:
		return structpb.NewNumberValue(float64(resolved.Int64())), true
	case slog.KindString:
		return structpb.NewStringValue(resolved.String()), true
	case slog.KindTime:
		return structpb.NewStringValue(resolved.Time().UTC().Format(time.RFC3339Nano)), true
	case slog.KindUint64:
		return structpb.NewNumberValue(float64(resolved.Uint64())), true
	case slog.KindAny:
		return anyToProtoValue(resolved.Any(), depth+1)
	case slog.KindGroup:
		children := resolved.Group()
		if len(children) == 0 {
			return structpb.NewStructValue(&structpb.Struct{Fields: map[string]*structpb.Value{}}), true
		}
		fields := make(map[string]*structpb.Value, len(children))
		for i := range children {
			childAttr := children[i]
			if childAttr.Key == "" {
				continue
			}
			childVal, ok := slogValueToProtoWithDepth(childAttr.Value, depth+1)
			if !ok {
				return nil, false
			}
			fields[childAttr.Key] = childVal
		}
		return structpb.NewStructValue(&structpb.Struct{Fields: fields}), true
	default:
		return nil, false
	}
}

func anyToProtoValue(value any, depth int) (*structpb.Value, bool) {
	if depth > maxProtoStructDepth {
		return nil, false
	}

	switch v := value.(type) {
	case nil:
		return structpb.NewNullValue(), true
	case *structpb.Value:
		return v, true
	case structpb.Value:
		cloned := proto.Clone(&v)
		return cloned.(*structpb.Value), true
	case *structpb.Struct:
		return structpb.NewStructValue(v), true
	case structpb.Struct:
		cloned := proto.Clone(&v)
		return structpb.NewStructValue(cloned.(*structpb.Struct)), true
	case *structpb.ListValue:
		return structpb.NewListValue(v), true
	case structpb.ListValue:
		cloned := proto.Clone(&v)
		return structpb.NewListValue(cloned.(*structpb.ListValue)), true
	case bool:
		return structpb.NewBoolValue(v), true
	case string:
		if !utf8.ValidString(v) {
			v = fixUTF8(v)
		}
		return structpb.NewStringValue(v), true
	case []byte:
		return structpb.NewStringValue(base64.StdEncoding.EncodeToString(v)), true
	case error:
		return structpb.NewStringValue(v.Error()), true
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return nil, false
		}
		return structpb.NewNumberValue(f), true
	case float32:
		return structpb.NewNumberValue(float64(v)), true
	case float64:
		return structpb.NewNumberValue(v), true
	case int:
		return structpb.NewNumberValue(float64(v)), true
	case int8:
		return structpb.NewNumberValue(float64(v)), true
	case int16:
		return structpb.NewNumberValue(float64(v)), true
	case int32:
		return structpb.NewNumberValue(float64(v)), true
	case int64:
		return structpb.NewNumberValue(float64(v)), true
	case uint:
		return structpb.NewNumberValue(float64(v)), true
	case uint8:
		return structpb.NewNumberValue(float64(v)), true
	case uint16:
		return structpb.NewNumberValue(float64(v)), true
	case uint32:
		return structpb.NewNumberValue(float64(v)), true
	case uint64:
		return structpb.NewNumberValue(float64(v)), true
	case time.Duration:
		return structpb.NewStringValue(v.String()), true
	case time.Time:
		return structpb.NewStringValue(v.UTC().Format(time.RFC3339Nano)), true
	case fmt.Stringer:
		return structpb.NewStringValue(v.String()), true
	case map[string]any:
		s, ok := payloadToStruct(v)
		if !ok {
			return nil, false
		}
		return structpb.NewStructValue(s), true
	case map[string]string:
		m := make(map[string]any, len(v))
		for key, val := range v {
			m[key] = val
		}
		s, ok := payloadToStruct(m)
		if !ok {
			return nil, false
		}
		return structpb.NewStructValue(s), true
	case []any:
		return listToProtoValue(v, depth)
	case []string:
		list := make([]any, len(v))
		for i := range v {
			list[i] = v[i]
		}
		return listToProtoValue(list, depth)
	}

	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return structpb.NewNullValue(), true
	}

	switch rv.Kind() {
	case reflect.Pointer:
		if rv.IsNil() {
			return structpb.NewNullValue(), true
		}
		return anyToProtoValue(rv.Elem().Interface(), depth+1)
	case reflect.Slice, reflect.Array:
		length := rv.Len()
		list := make([]any, 0, length)
		for i := 0; i < length; i++ {
			list = append(list, rv.Index(i).Interface())
		}
		return listToProtoValue(list, depth)
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return nil, false
		}
		result := make(map[string]any, rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			result[iter.Key().String()] = iter.Value().Interface()
		}
		s, ok := payloadToStruct(result)
		if !ok {
			return nil, false
		}
		return structpb.NewStructValue(s), true
	}

	// Fallback: treat as string.
	return structpb.NewStringValue(fmt.Sprint(value)), true
}

func listToProtoValue(items []any, depth int) (*structpb.Value, bool) {
	values := make([]*structpb.Value, 0, len(items))
	for _, item := range items {
		elem, ok := anyToProtoValue(item, depth+1)
		if !ok {
			return nil, false
		}
		values = append(values, elem)
	}
	return structpb.NewListValue(&structpb.ListValue{Values: values}), true
}

func fixUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}

	buf := make([]rune, 0, len(s))
	for _, r := range s {
		if utf8.ValidRune(r) {
			buf = append(buf, r)
		} else {
			buf = append(buf, '\uFFFD')
		}
	}
	return string(buf)
}

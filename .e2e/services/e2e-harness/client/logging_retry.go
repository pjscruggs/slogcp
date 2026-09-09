// Copyright 2025-2026 Patrick J. Scruggs
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

package client

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"time"

	"google.golang.org/api/googleapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// retryLogQuota retries a throttled read, never an assertion or permission error.
// The caller's deadline includes these waits. The attempt cap also bounds callers
// without deadlines; jitter prevents concurrent harnesses retrying in lockstep.
func retryLogQuota(ctx context.Context, read func() error) error {
	for attempt := range 4 {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := read()
		var apiErr *googleapi.Error
		throttled := status.Code(err) == codes.ResourceExhausted ||
			(errors.As(err, &apiErr) && apiErr.Code == http.StatusTooManyRequests)
		if !throttled || attempt == 3 {
			return err
		}
		delay := min(30*time.Second<<attempt, time.Minute) + time.Duration(rand.Int64N(int64(5*time.Second)))
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("log quota retry stopped (%v): %w", err, ctx.Err())
		case <-timer.C:
		}
	}
	panic("unreachable")
}

// retryLogQuotaRPC retries the same entries.list page, not the whole iterator.
func retryLogQuotaRPC(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoke grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	read := func() error { return invoke(ctx, method, req, reply, cc, opts...) }
	if method != "/google.logging.v2.LoggingServiceV2/ListLogEntries" {
		return read()
	}
	return retryLogQuota(ctx, read)
}

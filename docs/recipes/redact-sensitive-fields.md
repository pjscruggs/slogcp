# Redact sensitive structured fields

Use this recipe when your application logs domain objects or selected request
attributes. Give domain types an explicit logging representation, then use a
handler attribute replacer for the sensitive leaf keys your application uses.
Go's [structured logging introduction][slog-blog] describes `LogValuer` as a way
to control a type's logged representation.

## Select safe fields and replace known sensitive keys

This source-file excerpt constructs a logger and demonstrates both mechanisms.
Call `newRedactedLogger` at startup with your writer and trace project ID.
Handle the error and retain the returned handler for shutdown after logging
producers stop. `WithRedirectWriter` leaves writer ownership with the
application. Check the [startup
guide](../USAGE.md#create-the-handler-at-application-startup) for the rest of
the lifecycle.

```go
package app

import (
	"context"
	"io"
	"log/slog"
	"strings"

	"github.com/pjscruggs/slogcp"
)

type Customer struct {
	ID          string
	Plan        string
	Email       string
	AccessToken string
}

func (c Customer) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("id", c.ID),
		slog.String("plan", c.Plan),
	)
}

func redactAttr(_ []string, attr slog.Attr) slog.Attr {
	switch strings.ToLower(attr.Key) {
	case "authorization", "cookie", "set-cookie", "password", "access_token":
		return slog.String(attr.Key, "[REDACTED]")
	default:
		return attr
	}
}

func newRedactedLogger(
	out io.Writer, projectID string,
) (*slog.Logger, *slogcp.Handler, error) {
	handler, err := slogcp.NewHandler(out,
		slogcp.WithRedirectWriter(out),
		slogcp.WithTraceProjectID(projectID),
		slogcp.WithReplaceAttr(redactAttr),
	)
	if err != nil {
		return nil, nil, err
	}
	return slog.New(handler).With("component", "checkout"), handler, nil
}

func logCheckout(ctx context.Context, logger *slog.Logger, customer Customer) {
	logger.InfoContext(ctx, "checkout started",
		slog.Any("customer", customer),
		slog.Group("request", slog.String("Authorization", "Bearer example-secret")),
	)
}
```

The `customer` object contains only `id` and `plan` in the emitted JSON. The
`request.Authorization` value becomes `[REDACTED]`. The replacer also applies to
leaf attributes bound with `logger.With` and nested with `WithGroup`. It leaves
Cloud Logging's correlation fields intact.

The [slog attribute-replacement contract][replace] walks slog groups, not
arbitrary Go objects. A map or struct supplied as an `Any` value is a single
leaf unless it implements `LogValuer`. For example, a map under a key named
`request` can still contain an unredacted `Authorization` entry. Use explicit
slog groups or an allowlisted `LogValue` representation for those objects.
Extend the key policy to match your application's actual attribute names.

Keep secrets out of message strings, URLs, and error text at the call site; this
key-based policy does not scrub their contents. Converting `Customer` to another
type or formatting it into a string also bypasses its `LogValue` method. A
handler replacer applies to this JSON output path; separately added handlers or
other log destinations need their own policy. See
[`WithReplaceAttr`](../../handler.go) and the [configuration
reference](../CONFIGURATION.md#handler-setup).

If your application already has a replacer or handler wrapper, compose this
policy into that existing setup and preserve its filtering and destinations. The
pure callback above is safe to call concurrently and retains no group slice or
mutable request data.

## Verify the serialized output

Use distinctive synthetic secrets and decode the emitted JSON. Check a direct
attribute, a bound attribute, a nested slog group, and a domain object. Assert
that secret values are absent from the complete serialized output and the
intended safe fields remain. Pass a valid local span context and verify the
Cloud trace/span fields still match it.

Include a map-valued attribute in your application tests to make the policy's
boundary explicit: either replace the whole map's key or convert the map to an
approved representation before logging. Test errors and any additional log sinks
against their own policies as well.

[slog-blog]: https://go.dev/blog/slog
[replace]: https://pkg.go.dev/log/slog#HandlerOptions

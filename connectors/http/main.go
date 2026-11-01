// Command mandate-http holds API tokens for HTTP services and lends them to
// agents without handing them over. Reads go through an allowlisted
// passthrough at /forward/{service}/{path}; writes are the "http" connector's
// "request" op and run only with the owner's approval. Tokens never leave
// this process: they are added to outgoing requests only, and scrubbed from
// everything that comes back.
package main

import (
	"context"

	"github.com/adeci/mandate/connector"
)

func main() {
	connector.Main(func(_ context.Context, env *connector.Env, cfg Config) (connector.Connector, error) {
		return New(cfg, env.Secret, env.Log)
	})
}

// Command mandate-gmessages ingests SMS and RCS messages through Google
// Messages for Web, paired with the owner's Android phone, and serves the
// gmessages connector.
//
// It sends the writes the owner approved with a passkey, as verified by
// connector.Server, and nothing else: no read receipts, no typing.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/util"
	"go.mau.fi/util/exhttp"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

func main() {
	connector.Main(start)
}

func start(ctx context.Context, env *connector.Env, _ struct{}) (connector.Connector, error) {
	st, err := store.Open(ctx, env.DataPath())
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	// Shown under Device pairing on the phone.
	util.BrowserDetailsMessage.OS = "mandate"
	log := zerolog.New(os.Stderr).Level(zerolog.InfoLevel).With().Timestamp().Str("module", "libgm").Logger()
	newClient := func(auth *libgm.AuthData, onEvent func(any)) gmClient {
		cli := libgm.NewClient(auth, nil, log, exhttp.SensibleClientSettings)
		cli.SetPingInterval(pingInterval)
		cli.SetEventHandler(onEvent)
		return libgmClient{cli}
	}
	newPairing := func(auth *libgm.AuthData, paired func()) pairClient {
		cli := libgm.NewClient(auth, nil, log, exhttp.SensibleClientSettings)
		cli.SetEventHandler(func(any) {})
		cb := func(*gmproto.PairedData) { paired() }
		cli.PairCallback.Store(&cb)
		return libgmClient{cli}
	}
	c := newConnector(ctx, st, env.Log, env.Secret("session.json"), newClient, newPairing)
	if err := c.resume(); err != nil {
		st.Close()
		return nil, err
	}
	return &program{gmConnector: c, st: st}, nil
}

// program disconnects once serving has stopped, so an in-flight send can
// finish, then closes the store.
type program struct {
	*gmConnector
	st *store.Store
}

func (p *program) Close() error {
	p.disconnect()
	return p.st.Close()
}

// libgmClient adds the account of the fetched config to *libgm.Client.
type libgmClient struct{ *libgm.Client }

func (c libgmClient) Account() string { return c.Config.GetDeviceInfo().GetEmail() }

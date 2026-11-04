# Bundled connectors

Each one holds one account, links from the app under Connections, and speaks the [connector protocol](spec/connector.md). None is official: they use the services' own apps' protocols and may break when a service changes.

| Connector | Links as | Linking |
|---|---|---|
| `whatsapp` | a linked device | scan a QR code with WhatsApp, or type a pairing code for your number |
| `signal` | a linked device | scan a QR code with Signal |
| `gmessages` | a paired computer | scan a QR code with Google Messages, or confirm emoji with your Google account |
| `matrix` | a session of your account | homeserver, user and password or token; verify with your recovery key or by comparing emoji |
| `discord` | your account | its token |
| `fastmail` | an API token | a token with mail access |

Each runs as its own throwaway user and keeps its session in a directory only it can read. mandate reads what it stores through your grants.

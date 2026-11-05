# Signed records

The owner's devices sign four kinds of record:

- **key**: entries of the key log;
- **identity**: an agent identity;
- **grant**: what an identity may read;
- **write**: an approved write.

The gateway verifies identities and grants. Connectors verify writes. The key log says which keys are trusted.

Other changes need only the device token and are not signed, because they only shrink access: Close, Deny, Stop all, deleting an identity, and never-share edits. Never-share removal is the exception, but grants still limit what can be read. What the owner side syncs to the gateway decides which identities and grants are live.

## Device keys

A device key is a P-256 key that signs only after the owner authenticates on the device. Pairing is the root of trust; keys carry no attestation.

| type | lives in | signs |
|---|---|---|
| `p256` | the platform's hardware store: StrongBox or TEE, Secure Enclave, a TPM | the challenge, with ECDSA and SHA-256 |
| `fido2` | a FIDO2 authenticator over CTAP2, e.g. a YubiKey | a FIDO2 assertion over the challenge |

```json
{"id":"b903…d31e","name":"Pixel 9","public_key":"BN4CBo…kcE","type":"p256"}
```

- `public_key` is the 65-byte uncompressed point.
- `id` is its SHA-256.
- `credential_id` is present for `fido2` keys only.

## Encoding

Record bytes are JSON in RFC 8785 (JCS) form:

- no whitespace;
- object keys sorted, all ASCII;
- integers only, at most 2^53 − 1 in magnitude;
- no `null`.

Optional fields are omitted, never empty.

| value | form |
|---|---|
| time | RFC 3339, UTC, whole seconds: `2026-11-03T09:00:00Z` |
| duration | integer seconds |
| bytes | base64url without padding |
| hash, key ID | lowercase hex |
| record ID | 26 base32 characters (RFC 4648, no padding): 128 random bits |

Verifiers MUST reject record bytes that:

- are not canonical (re-encode them and compare);
- have unknown fields;
- lack a required field.

## Envelope

```json
{"kind": "grant", "record": "<bytes>", "nonce": "<16+ random bytes>", "issued_at": "2026-11-03T09:15:00Z",
 "key": "<key id>", "authenticator_data": "<fido2 only>", "sig": "<64 bytes>"}
```

`H(p1, …, pn)` is SHA-256 over each part as an 8-byte big-endian length, then the part:

```
challenge = H("mandate-record-v1", kind, record, nonce, issued_at)
```

- **`p256`:** `sig` = ECDSA-P256 over SHA-256(`challenge`).
- **`fido2`:** the authenticator gets `challenge` as `clientDataHash`, rp ID `mandate`, user verification required.
  - `sig` = ECDSA-P256 over SHA-256(`authenticator_data || challenge`).
  - Verifiers check that `authenticator_data` starts with SHA-256(`"mandate"`) and has flags UP (0x01) and UV (0x04).
  - The sign count is not checked, because nonces and spent IDs already stop replays.
- **Signature form:** `sig` is r||s, 32 bytes each. Signers convert from DER.

## Verifying

1. `kind` is `key`, `identity`, `grant` or `write`.
2. `record` decodes for its kind, as under Encoding.
3. `key` is trusted by the key log, and for a key entry or a write not retired. For a key entry, use the log as it stood before that entry.
4. `nonce` is at least 16 bytes.
5. `sig` verifies over the challenge.
6. The kind's rules below hold.

Removing a key invalidates the identities and grants it signed; retiring it does not.

## Kinds

### key

| field | |
|---|---|
| `seq` | 0, 1, 2, … |
| `op` | `add`, `retire` or `remove` |
| `key` | the key |
| `prev` | SHA-256 of the previous entry's record bytes; `""` at seq 0 |
| `at` | for display |

- **Seq 0** adds the first device's key and is signed by that key. Every later entry is signed by a key trusted after the entry before it.
- **`add`** names a key not yet trusted.
- **`retire`** names a trusted key exactly as it was added, not yet retired, and is signed by another key. A retired key signs no key entries or writes, but the identities and grants it signed stay valid. A device that approves in several ways holds a key for each; it adds a new one with a key it already has, and retires one with another.
- **`remove`** names a trusted key exactly as it was added, for a key that may be compromised.
- Neither `retire` nor `remove` may leave no key that is not retired.
- **Name:** a device's display name is unsigned metadata, like an identity's; the key's `name` is its default.
- **Order:** a log is valid only as a whole, in order.
- **First log:** a connector trusts the first log the owner side pushes to it (trust on first use). Only the owner side's uid can push, and root starts both. `mandate reset` empties every log, so a fresh seq 0 is accepted again.

### identity

| field | |
|---|---|
| `id` | record ID |
| `token_sha256` | SHA-256 of the identity's token text |
| `created_at` | |

- **Token:** made and hashed by the new party, as in [agent-api.md](agent-api.md#code-exchange).
- **Name:** the identity's name is unsigned metadata, and renaming signs nothing.

### grant

| field | |
|---|---|
| `id` | record ID |
| `identity` | identity `id` |
| `connector` | |
| `chats` | chat IDs; omitted means every chat |
| `horizon` | seconds back from each read; `0` means all history |
| `from`, `until` | a one-off window |
| `schedule` | a routine, instead of `from` and `until` |
| `note` | |
| `created_at` | |

`schedule` has `days` (0 = Sunday to 6, ascending; omitted means every day), `start` (`HH:MM`), `length` (1 to 86400) and `zone` (IANA).

A grant has either `from` < `until` or `schedule`, not both. What a grant allows is in `agent-api.md`.

### write

| field | |
|---|---|
| `id` | the proposal ID |
| `connector`, `op` | |
| `args` | object of strings, with the owner's edits applied |
| `display` | `[{label, value, param?}]`, rendered by the connector from `args` |
| `note` | |
| `created_at`, `expires_at` | |

The connector:

- requires `issued_at` no more than 5 minutes before now or 1 minute after it, and now before `expires_at`;
- spends the nonce and `id`, so each runs at most once;
- re-renders `display` from `args` and refuses if it differs.

## Test vectors

These come from a throwaway Go program whose `H` was checked against the `approval` package. Each signature uses `k` = SHA-256("mandate test nonce " || message) mod n, so that the vectors reproduce; real signers use a fresh `k` or RFC 6979. Record bytes are ASCII and shown as text, with no trailing newline.

```
H("a", "bc") input  00000000000000016100000000000000026263
H("a", "bc")        3fafa1cf2f19a7c1129beb20cf0983f73a489a221fc0dd2f16d1be292d089205
```

Keys (test-only private scalars):

```
phone   p256   d   ba96728848f1a1f6ac7fc120780235401cd5d9350d44cbcdead0a8e0c305546a
               pub 04de02068f9cbf6518b2fed2db60a6c1ba0ec40567421fef00871e787189ff04cac5e7d7ec27cb08fbfd53da46b438d1cece993e7dfd44ead6ebb15cab68a591c1
               id  b9035233be144c52017fbb52541aab66faa7f6c93eef6e76968ba564c477d31e
laptop  p256   d   c69bdb52d170ed9e901cbf4fe4836886a4d06687c4579712445702724aebd2c3
               pub 047eea7e092dfa9c00412e4c9d362b6e29557b9af54b791dbb39d154b539b219ccc0fdd13ce45209d8e07fded9fee69d466f1b00a5dc174e7b2f7463041ac46bbc
               id  32bcdd4973e2c874262f3c979066b64a5e3a1355c7d65ea61545e476abaa6669
yubikey fido2  d   66428e5ece7985847c3e4cc7f278980a80639a13364cbcbdee44ac0e9270b043
               pub 04accf53998f3b353f47d05e30739f1455f53ad02a589e1474e9502309654412275e270faac971c50bc9ea1db463c1c5b1a366cd250edf4702d3b94a4771f6f5f1
               id  f94142c81924df1ae7145b6b42b5b6f8e8d84051ee618ead2aa4749cd4577289
               credential_id oKGio6SlpqeoqaqrrK2urw
```

Each vector below gives the record, then its nonce, `issued_at`, signer, challenge and sig.

**key, seq 0: the phone adds itself**

```
{"at":"2026-11-03T09:00:00Z","key":{"id":"b9035233be144c52017fbb52541aab66faa7f6c93eef6e76968ba564c477d31e","name":"Pixel 9","public_key":"BN4CBo-cv2UYsv7S22CmwboOxAVnQh_vAIceeHGJ_wTKxefX7CfLCPv9U9pGtDjRzs6ZPn39ROrW67Fcq2ilkcE","type":"p256"},"op":"add","prev":"","seq":0}
nonce 101112131415161718191a1b1c1d1e1f  issued_at 2026-11-03T09:00:00Z  key phone
challenge a0fa023f9aa6c5a527c4a82952ca0365dbddad3b7561622f99ee63db3599ff9e
sig       a18f08607cb28eaed509a4365863bfc87148c542360d1fa5421cbef6e4e09bafe2faa1a2c8e9e513d580f6c45f9251e0c86873dd91dde0739f0f75dcde3e71fb
```

As an envelope:

```json
{
  "kind": "key",
  "record": "eyJhdCI6IjIwMjYtMTEtMDNUMDk6MDA6MDBaIiwia2V5Ijp7ImlkIjoiYjkwMzUyMzNiZTE0NGM1MjAxN2ZiYjUyNTQxYWFiNjZmYWE3ZjZjOTNlZWY2ZTc2OTY4YmE1NjRjNDc3ZDMxZSIsIm5hbWUiOiJQaXhlbCA5IiwicHVibGljX2tleSI6IkJONENCby1jdjJVWXN2N1MyMkNtd2JvT3hBVm5RaF92QUljZWVIR0pfd1RLeGVmWDdDZkxDUHY5VTlwR3REalJ6czZaUG4zOVJPclc2N0ZjcTJpbGtjRSIsInR5cGUiOiJwMjU2In0sIm9wIjoiYWRkIiwicHJldiI6IiIsInNlcSI6MH0",
  "nonce": "EBESExQVFhcYGRobHB0eHw",
  "issued_at": "2026-11-03T09:00:00Z",
  "key": "b9035233be144c52017fbb52541aab66faa7f6c93eef6e76968ba564c477d31e",
  "sig": "oY8IYHyyjq7VCaQ2WGO_yHFIxUI2DR-lQhy-9uTgm6_i-qGiyOnlE9WA9sRfklHgyGhz3ZHd4HOfD3Xc3j5x-w"
}
```

**key, seq 1: the phone adds the laptop**

```
{"at":"2026-11-03T09:05:00Z","key":{"id":"32bcdd4973e2c874262f3c979066b64a5e3a1355c7d65ea61545e476abaa6669","name":"MacBook","public_key":"BH7qfgkt-pwAQS5MnTYrbilVe5r1S3kduznRVLU5shnMwP3RPORSCdjgf97Z_uadRm8bAKXcF057L3RjBBrEa7w","type":"p256"},"op":"add","prev":"b0386bffa97186b1fb7d2e64a1da19a390a76c0c849099e987fefb7a9e5a22e8","seq":1}
nonce 202122232425262728292a2b2c2d2e2f  issued_at 2026-11-03T09:05:00Z  key phone
challenge f73a9e7b0f031147bb0caea20e6be829fc6e3037d731708a2cd591e3045a0668
sig       8d29ec256c36fd59fc28ea5ed7b4f4c111805cb0d4a4f5e747188b6575d7a42b11e36490859af5c34d5b248e12015f84207927967837e01de2f57fdd7d21fac2
```

**key, seq 2: the phone adds a YubiKey**

```
{"at":"2026-11-03T09:07:00Z","key":{"credential_id":"oKGio6SlpqeoqaqrrK2urw","id":"f94142c81924df1ae7145b6b42b5b6f8e8d84051ee618ead2aa4749cd4577289","name":"YubiKey 5","public_key":"BKzPU5mPOzU_R9BeMHOfFFX1OtAqWJ4UdOlQIwllRBInXicPqslxxQvJ6h20Y8HFsaNmzSUO30cC07lKR3H29fE","type":"fido2"},"op":"add","prev":"9d9286509c407a41f644ade1cb6f44af8ec3d60343064902201ffb28caf207ce","seq":2}
nonce 28292a2b2c2d2e2f3031323334353637  issued_at 2026-11-03T09:07:00Z  key phone
challenge 68f36f5cc56cbbfa23a409f5384822545a4215e9780b272acf94c559eb1360ff
sig       24c582d6503b9fefd3e46e2bb88b025163d9663263d46bba51442809ed85ec85d5175e391573051dfcf61e0d3ee843fbf60aa855d350762a0a34432451674863
```

**identity**

```
{"created_at":"2026-11-03T09:10:00Z","id":"QX3M7E2KQ4F5PN6ZJ7LDV2Y3AU","token_sha256":"9ca6a79517f50695032879c8e495efe27d51afac90bd81578e3f57701e7a5732"}
nonce 303132333435363738393a3b3c3d3e3f  issued_at 2026-11-03T09:10:00Z  key phone
challenge b221969333db6f727996ae7a3a162901370fa90b4afd20e85b8d9a70a93f40f7
sig       bb476ff7133be63650d660f2c2c4586b709ca6515c370742697b125273634135f7fe9350769112ff2eb23ecfe3a87359f51f9bd9d3401cbdc56dfe13f70d05f2
```

**grant, one-off**

```
{"chats":["group.6f1b2c"],"connector":"signal","created_at":"2026-11-03T09:15:00Z","from":"2026-11-03T09:15:00Z","horizon":604800,"id":"7RJ4P2W5ZK3HXQ6N5M2C4YB7DE","identity":"QX3M7E2KQ4F5PN6ZJ7LDV2Y3AU","until":"2026-11-03T10:15:00Z"}
nonce 404142434445464748494a4b4c4d4e4f  issued_at 2026-11-03T09:15:00Z  key phone
challenge 23efdcc37f7bff647e2ea3d956652dc21c2d7f63b4aad03aa6a2c78303e04410
sig       a5dbc6cae740a94412b5160aac2c850ef8ab02c6ce3f99f38a331ebebebf0ec98bd68701c835468296a72a5ba7f87e884a2b429e3bac4ff31da4ed7bee59300d
```

**grant, routine**

```
{"chats":["inbox"],"connector":"fastmail","created_at":"2026-11-03T09:20:00Z","horizon":86400,"id":"M4D2X7KQ5P3ZL6WJ5R2N4YB4TA","identity":"QX3M7E2KQ4F5PN6ZJ7LDV2Y3AU","note":"morning summary","schedule":{"days":[1,2,3,4,5],"length":1200,"start":"07:00","zone":"Europe/Berlin"}}
nonce 505152535455565758595a5b5c5d5e5f  issued_at 2026-11-03T09:20:00Z  key laptop
challenge df66a320e16ab732eeedfff1ab0efbd8e90771775bbd3d9eacfccf39af82acd2
sig       8ac71b9fb1fb4d882a194878688ccf4727206b35c5d636652ad5f553542d345dca229717cbb1e142e0ad3e23a07f60d527838925e6f5e713a7cf7b390ae1064d
```

**write**

```
{"args":{"chat":"group.6f1b2c","text":"See you at 8"},"connector":"signal","created_at":"2026-11-03T09:30:00Z","display":[{"label":"To","value":"~Family"},{"label":"Text","param":"text","value":"See you at 8"}],"expires_at":"2026-11-04T09:30:00Z","id":"Z2K7Q4M3P6X5W2N7D4R3J6YB5C","op":"send"}
nonce 606162636465666768696a6b6c6d6e6f  issued_at 2026-11-03T09:31:00Z  key phone
challenge e151d04c076e9f3e71895aacd8b82f36ee4b5523c898ae285b24bed14f843420
sig       d210091ba6f38e8302b8a51d13600d1c1c7b1a8c968a3798b835ad1e865940cd7825a9a8f7bf235ed2c00d1796fc16f14cbe6272bb0493602386cfcd7464d1ec
```

**write, approved with the YubiKey**

`authenticator_data` is SHA-256(`"mandate"`), then flags `05`, then sign count `00000007`.

```
{"args":{"body":"Signed copy attached.","subject":"Lease","to":"dana@example.com"},"connector":"fastmail","created_at":"2026-11-03T09:40:00Z","display":[{"label":"To","value":"dana@example.com"},{"label":"Subject","param":"subject","value":"Lease"},{"label":"Text","param":"body","value":"Signed copy attached."}],"expires_at":"2026-11-04T09:40:00Z","id":"H5N2Q7K4X3M6P2W5Z7R4D3YB6J","op":"send"}
nonce 707172737475767778797a7b7c7d7e7f  issued_at 2026-11-03T09:41:00Z  key yubikey
authenticator_data d959b638663f9a7de663a1be37e39bc5d820e3dbb3a31b0af1bad43e0c39fc690500000007
challenge 85a66d819ef8d18b0cfaf7cd1b6fce2cc50ea99769be2c684a2681fde2a13016
sig       c0bd3f0afc7c0a080b4f85fe6afe1f9737966049ce3a87d4273b9b41e2f185cefec064417a326cd8b62a5f430b836c1e48c98998995774ca745ef844b3f8ba32
```

**key, seq 3: the laptop retires the phone**

```
{"at":"2026-11-03T09:50:00Z","key":{"id":"b9035233be144c52017fbb52541aab66faa7f6c93eef6e76968ba564c477d31e","name":"Pixel 9","public_key":"BN4CBo-cv2UYsv7S22CmwboOxAVnQh_vAIceeHGJ_wTKxefX7CfLCPv9U9pGtDjRzs6ZPn39ROrW67Fcq2ilkcE","type":"p256"},"op":"retire","prev":"0af42c2f10d3d8193f43273ef3c8b5b79f798fd7edf8542e9c0e99de385f9116","seq":3}
nonce 808182838485868788898a8b8c8d8e8f  issued_at 2026-11-03T09:50:00Z  key laptop
challenge 44f6d7cc661f0fb4aeb0e1ccc4d6d632f3e40ec954a774e908396a01a292438c
sig       ae221c4433b7f68a3b27446c75fd9a0ac38e51ff7abcab46d0ea49e876dc514e480107f1798eeff1245bce5d5c47b6274bcd12e6a21e7040f820f08f21c1c8dd
```

After it, the identity and the one-off grant, which the phone signed, still verify.

**Must fail**

- The write, verified after the phone is retired: a retired key signs no writes.
- The one-off grant's envelope with `kind` set to `identity`. The bytes do not decode as an identity, and the challenge becomes `fa68aefd9d143f4937810198130e07ad1816c529dd278258c0c776bd4f552ef8`, so `sig` fails too.
- The one-off grant with a space before `"horizon"`. It is not canonical, so it is rejected before the signature is checked.

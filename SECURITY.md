# Security

hubbub holds every credential the channels it delivers to require, and issues
the keys that reach them. That makes it worth reporting bugs in carefully.

## Reporting a vulnerability

Use GitHub's private reporting:
**[Report a vulnerability](https://github.com/ryanlewis/hubbub/security/advisories/new)**.

Please don't open a public issue for anything with a working path to impact.

This is a personal project maintained in spare time. You will get an
acknowledgement, and a fix when there is one, but there is no response-time
commitment and no bounty. If a report is a real vulnerability I will credit you
in the advisory unless you'd rather I didn't.

A useful report says what an attacker gets and how they get there. A scanner
finding with no path to impact is not something I can act on.

## Supported versions

There are no releases. `main` is the only supported version, fixes land there,
and nothing is backported. Config shapes are still changing — the README says
to expect that before a tagged release, and it means it.

## What hubbub assumes

hubbub is one component of a deployment, and four of its security properties
are things it *cannot* enforce for itself. If your deployment breaks one, the
resulting exposure is a misconfiguration rather than a vulnerability in this
code — which does not make it any less your incident.

1. **TLS terminates in front of it.** There is no certificate handling in the
   binary at all; both listeners are plain HTTP. Without a proxy in front,
   every bearer key crosses the network in the clear.

2. **The ops port is unreachable from the internet.** It carries no
   authentication of any kind, and `POST /test/{channel}` on it sends a real
   notification. Where you bind it *is* the access control, which is why the
   `[ops]` table is presence-enabled: delete it and no ops listener exists.

3. **Admin identity comes from a proxy that strips client-supplied copies of
   its own headers.** With `auth = "exe-dev"`, `/admin` trusts
   `X-ExeDev-Email`. That is sound behind the exe.dev proxy, which removes any
   copy a client sends, and worthless anywhere else — reached directly, the
   header is whatever the caller typed, and so is the identity. hubbub warns
   about this on every start rather than letting it be discovered later.

4. **The config files are readable only by the service user.** `channels.toml`
   holds SMTP passwords and ntfy tokens; `keys.toml` is nothing but
   credentials. `0600`, owned by the service user. File permissions are the
   only thing protecting them at rest — hubbub does not encrypt them.

## Deliberate, and not a vulnerability

Reports of the following are already-made decisions rather than findings. Each
is documented where an operator will meet it.

- **Unauthenticated `/`, `/llms.txt`, `/favicon.svg`, `/health`,
  `/openapi.json` and `/docs`.** An agent handed a base URL has to be able to
  discover the contract before it holds a key. The first three describe the API
  and never the deployment behind it. `/openapi.json` and `/docs` do name this
  instance's channel ids: that is shape, not secrets, and it is the point of
  serving them.

- **No authentication on the ops port.** See assumption 2 — placement is the
  control.

- **`X-ExeDev-Email` is trusted.** See assumption 3. A demonstration that the
  header can be forged *through a correctly configured exe.dev proxy* is a real
  finding and very much wanted. A demonstration that it can be forged against a
  hub reached directly is the documented behaviour.

- **The `html` request field is passed through as written**, with control
  characters stripped and nothing else. Posting it requires a key the operator
  issued to a machine the operator runs — the same trust that already lets a
  caller put arbitrary text in front of the operator — and the receiving mail
  client is itself a sanitiser. Sanitising properly means parsing HTML, which
  the standard library cannot do and which is not worth a dependency for
  content that is already trusted. A key grants markup in the operator's inbox;
  the README says so rather than leaving it implied.

- **No per-key rate limits.** There is one global cap, and it is a blast-radius
  guard rather than accounting: a compromised key can exhaust it for every
  other caller. Per-key caps are on the roadmap, and the trade is stated.

## In scope

Roughly in the order I would worry about them:

- Authenticating without a valid key, or any way a key reaches a channel it was
  not granted. A key's `channels` list is its permission boundary; a request
  may narrow that set and must never widen it.
- Bypassing `/admin` where assumption 3 actually holds.
- Credential disclosure: a masking gap in the `/admin` confirmation diff, or a
  secret reaching the delivery log, a metrics label, an error body or a page.
  Metrics labels are channel ids and outcomes only, deliberately.
- Escaping the spool directory through a channel id, or any other path
  traversal driven by config or request data.
- Writing attacker-controlled structure into `keys.toml` or `channels.toml`
  through a value the dashboard splices in — a caller id that closes its own
  table and opens another, and anything of that shape.
- Breaking either outbox invariant: a terminal outcome settled twice or not at
  all, or a transient failure that deletes a message instead of retrying it. A
  `202` is a promise to settle later in the delivery log, and losing that is a
  notification that silently never arrives.

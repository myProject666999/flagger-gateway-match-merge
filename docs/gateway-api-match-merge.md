# Gateway API match merging: rules, conflicts and session affinity

This note records the semantics that were previously implicit in
`pkg/router/gateway_api.go` and `pkg/router/gateway_api_v1beta1.go`, and the
behaviour that is now guaranteed for both `gatewayapi/v1` and
`gatewayapi/v1beta1`.

## Where the code lives now

The translation and merging used to exist twice, once per API version, with
slightly different behaviour. They are now implemented once in
`pkg/router/gateway_api_match.go` on version-agnostic structures
(`gatewayMatch`, `gatewayPathMatch`, `gatewayHeaderMatch`, `gatewayQueryMatch`)
and only mapped to the concrete API types by `toV1RouteMatches` /
`toV1Beta1RouteMatches`. Both routers therefore use the same logic; the tests in
`pkg/router/gateway_api_parity_test.go` run one table against both
implementations and assert that the resulting `HTTPRoute` rules are equal field
by field.

## Merge rule table

`mergeGatewayMatches(analysis, service)` produces the cartesian product: one
merged match per (analysis match, service match) pair, in analysis-major order.
For each pair:

| Dimension | Analysis has it | Service has it | Both have it |
|---|---|---|---|
| Path | taken from analysis | taken from service | must be identical (`Type` **and** `Value`), otherwise unsatisfiable |
| Method | taken from analysis | taken from service | must be identical, otherwise unsatisfiable |
| Headers | unioned by name | unioned by name | unioned by name; same name with same type+value collapses to one; different type or value is a conflict |
| Query params | unioned by name | unioned by name | unioned by name; same name with same type+value collapses to one; different type or value is a conflict |

Header names are compared case-insensitively (the HTTP semantic); the spelling
that survives is the one from the service match, which is the base of the merge.
Query parameter names are compared exactly. `Service.Match` remains the base, so
properties it defines (path, method, filters) are preserved unless the analysis
side explicitly requires something incompatible.

This fixes the three previously observed gaps:

1. **Header replacement:** previously an analysis header list replaced the
   service header list entirely. With `service.match: x-tenant=acme` and
   `analysis.match: x-canary=always`, the A/B rule now requires *both* headers;
   previously it required only `x-canary` and let other tenants into the canary.
2. **Missing analysis path:** previously an analysis path/prefix was never
   carried into the merged match, so an analysis-only `prefix: /beta` produced
   an A/B rule that matched every path. It is now part of the merged match.
3. **Silent path/method conflicts:** an analysis `exact: /api` combined with a
   service `prefix: /api` used to be silently ignored. It is now reported.

## Conflict handling

Both conflicts and unsupported selectors are **rejected**, not warned about and
ignored. Rationale: silently widening the match is the failure mode that leaks
non-test traffic into the canary, so a rejected configuration is safer.

* `ErrUnsupportedMatch` — a selector that Gateway API cannot express: header
  prefix/suffix matches, empty (presence-only) header/query matches, and a
  method matcher that is not `exact`.
* `ErrConflictingMatch` — the same header/query name required with two different
  values or types, or two different paths/methods in one merged match.

Errors are wrappers around one of those sentinels and are returned from
`Reconcile`/`SetRoutes`; the controller turns them into a warning event and does
not write a route (`scheduler.go` already records `%v` of the router error), so
the previously generated `HTTPRoute` is left untouched. This is what "reject the
configuration" means here: the Canary is not progressed and no match is
silently widened.

## The two swallowed error sites

Both are fixed:

1. `analysisMatches, _ := gwr.mapRouteMatches(canary.GetAnalysis().Match)` in
   `Reconcile` of both files — the error is now propagated.
2. `svcMatches, _ := gwr.mapRouteMatches(canary.Spec.Service.Match)` in the
   session-affinity cleanup path (`canaryWeight == 0`) of both files — now
   propagated.

Additionally, the analysis translation errors are now reported (previously the
`analysisMatches` call above was the only one that was ignored; it is the same
call site, now checked).

The method-match error message used to quote `requestMatch.Headers`; it now
quotes `requestMatch.Method`.

## Determinism

Map iteration order used to leak into the generated slices. Header and query
param conditions are now sorted by name (headers case-insensitively, then by
spelling, type and value; query params by name, type and value), and duplicate
names are collapsed before the slices are built. The merge preserves that order,
so two reconciles of the same spec produce byte-identical matches. See
`TestGatewayAPIRouters_ReconcileIsIdempotent`, which asserts that the second
`Reconcile` performs no `update` call on the `HTTPRoute`.

## Was PrimaryCookieName optional?

It is now optional, which is what the router code always assumed
(`getSessionAffinityRouteRules` only supports the primary cookie when the name is
non-empty). The validator previously compared the two names without checking for
emptiness, so `cookieName: canary` with no `primaryCookieName` compared
`"canary"` against `""` (allowed) — but a config with *both* empty was rejected
because `"" == ""`. In practice that made `primaryCookieName` effectively
mandatory whenever `sessionAffinity` was set with an empty primary name.

`verifySessionAffinity` now only rejects when **both names are non-empty and
identical**:

```go
if cookieName != "" && primaryCookieName != "" && cookieName == primaryCookieName { ... }
```

An empty `primaryCookieName` means "canary stickiness only", which is the
documented behaviour ("unset means no primary stickiness"). A fully empty cookie
name remains allowed by validation, but the router cannot generate a useful
cookie in that case; that is unchanged from before and out of scope here.

## Cookie matching expression

The old expression was `.*<name>.*<value>.*` built from
`strings.Split(cookie, "=")`. It matched unrelated cookies
(`xcanary=1; other=abc` for `canary=abc`, because the name and value appear in
different pairs), matched prefixes/suffixes (`canary=abcd` for `canary=abc`), did
not escape regex metacharacters in the name, and mis-split values containing `=`.

The generated expression is now built from the exact cookie pair, with the name
and value escaped via `regexp.QuoteMeta`, and anchored to a cookie boundary:

```
.*(^|;)[[:space:]]*<name>=<value>[[:space:]]*(;|$).*
```

* `(^|;)` and `(;|$)` bound the pair to the start/end of the header or to a
  semicolon, so `xcanary=abc` does not match `canary=abc`, and `canary=abcd`
  does not match `canary=abc`.
* `[[:space:]]*` tolerates the optional whitespace after `;` and before `;`.
* The leading and trailing `.*` mean the expression still matches the whole
  `Cookie` header value, which works for both full-match (RE2, as used by Envoy)
  and search-based regex engines. Only RE2/PCRE/POSIX-ERE constructs are used
  (no lookarounds, no backreferences), so Gateway API implementations that
  accept POSIX ERE or RE2 accept it. If an implementation rejects `[[:space:]]`
  it can be written as `[ \t]*`, which is equivalent for Cookie headers.

The regex is produced by `sessionAffinityCookieRegex`, and the test
`TestSessionAffinityCookieRegex` covers the start/middle/end positions, the
`xcanary=abc` and `canary=abcd` counter-examples, metacharacters in the name and
in the value, and values containing `=`.

## Timeout / cookie lifetime

* `MaxAge` still defaults to 86400 seconds via `SessionAffinity.GetMaxAge()`.
* The primary cookie's `Max-Age` is still the analysis interval, parsed with
  `time.ParseDuration`.
* **Cleanup was missing and is now implemented.** When the analysis ends
  (`canaryWeight == 0`), the router already emitted a rule that expires the
  previous canary cookie. It now also expires the primary cookie:
  `Set-Cookie: <cookie>; Max-Age=-1` is emitted for both cookies (on the
  `Cookie`-matching sticky rule, so only clients that actually hold the cookie
  receive the deletion). `SessionAffinityCookie` and
  `PrimarySessionAffinityCookie` are cleared, `PreviousSessionAffinityCookie`
  keeps its existing meaning ("the canary cookie of the run that just ended").
  If the primary cookie was present but no previous canary cookie exists, the
  sticky rule is still generated for the primary cookie so it can be expired.

This happens on both promotion and rollback because both paths call
`SetRoutes(..., canaryWeight=0, ...)` (`scheduler.go` `rollback`,
`runPromotionTrafficShift`, `handleFailedPromotion`).

## Impact on existing HTTPRoutes

* **First reconcile after the upgrade:** every Canary whose analysis match
  previously lost the service conditions will see a one-time `update` of its
  `HTTPRoute`: the A/B rule gains the merged path/method/headers/query
  conditions. Canaries that only use `service.match` (no `analysis.match`) are
  unchanged unless their conditions were previously emitted in map order — the
  first reconcile sorts them, which can also cause a single update. After that,
  reconciles are stable.
* **Ordering:** matches are emitted in analysis-major, service-minor order with
  sorted headers/query params. A previously stored route with a different order
  is updated once and then compares equal (`cmp.Diff` sees equal slices).
* **Session affinity routes:** the cookie regular expression changes, so the
  first `SetRoutes` after the upgrade rewrites the sticky rules once. These rules
  are deliberately ignored by `Reconcile`'s comparison (they derive from status,
  not spec), so `Reconcile` itself does not update them.
* **A/B + session affinity:** `SetRoutes` previously wrote the merged A/B
  conditions into `Rules[0]`, which — with session affinity enabled — is the
  first *sticky* rule, destroying its `Cookie` match. The weighted rule is now
  addressed by position (`len(rules)-1` when affinity is on) so the sticky rules
  keep their cookie match plus the service conditions. Covered by
  `TestGatewayAPIRouters_ABWithSessionAffinity`.
* No CRD/schema change was needed: the new cookie regex is still a
  `RegularExpression` header match, and the status fields keep their meaning.

## Tests

All offline, using the fake Gateway API clientset from the existing fixtures.

* `pkg/router/gateway_api_match_test.go`
  * `TestTranslateRouteMatches` — exact/prefix/regex path, method, headers,
    query params, sorting, case-insensitive dedupe, case conflict, unsupported
    method/header/query selectors (including that the method error names the
    method), empty match ignored.
  * `TestMergeGatewayMatches` — header and query union, conflicts, identical
    conditions collapsing, analysis-only path prefix and method, path conflicts
    (exact vs prefix), method conflicts, cartesian product order and
    determinism.
  * `TestSessionAffinityCookieRegex` — the table of header/cookie combinations
    above.
  * `TestSessionAffinityCookieMatch` — `name=value` parsing and rejection of an
    empty cookie.
* `pkg/router/gateway_api_parity_test.go` — one scenario table run against both
  API versions, asserting equivalence: A/B merge, rule layout, session affinity
  cookie rules (with/without primary cookie), cleanup after the analysis, A/B +
  session affinity, and reconcile idempotency (no update on an unchanged route).
* `pkg/controller/controller_test.go` — `verifySessionAffinity`: both names equal
  (error), primary name empty (ok), both names empty (ok), distinct names (ok).

Run with `go test ./pkg/router/ ./pkg/controller/`.

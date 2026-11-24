/*
Copyright 2025 The Flux authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package router

import (
	"context"
	"fmt"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stesting "k8s.io/client-go/testing"

	flaggerv1 "github.com/fluxcd/flagger/pkg/apis/flagger/v1beta1"
	v1 "github.com/fluxcd/flagger/pkg/apis/gatewayapi/v1"
	"github.com/fluxcd/flagger/pkg/apis/gatewayapi/v1beta1"
	istioapiv1alpha1 "github.com/fluxcd/flagger/pkg/apis/istio/common/v1alpha1"
	istiov1beta1 "github.com/fluxcd/flagger/pkg/apis/istio/v1beta1"
	clientset "github.com/fluxcd/flagger/pkg/client/clientset/versioned"
)

// gatewayRouterUnderTest exposes the operations of both Gateway API router
// implementations through the same interface, so a single table can assert that
// they produce equivalent results.
type gatewayRouterUnderTest struct {
	name      string
	reconcile func(canary *flaggerv1.Canary) error
	setRoutes func(canary *flaggerv1.Canary, primaryWeight, canaryWeight int) error
	getRules  func(t *testing.T, canary *flaggerv1.Canary) []gatewayRuleUnderTest
	// routeWrites returns how many create/update calls were made against HTTPRoutes.
	routeWrites func() int
}

type gatewayRuleUnderTest struct {
	Matches     []gatewayMatch
	BackendRefs []string
	SetCookies  []string
}

func newGatewayRoutersUnderTest(canary *flaggerv1.Canary) []gatewayRouterUnderTest {
	// Each router gets its own fake clients so that writes performed by one
	// implementation are not observed by the other.
	v1Mocks := newFixture(canary)
	v1beta1Mocks := newFixture(canary)

	v1Router := &GatewayAPIRouter{
		gatewayAPIClient: v1Mocks.meshClient,
		kubeClient:       v1Mocks.kubeClient,
		logger:           v1Mocks.logger,
	}
	v1beta1Router := &GatewayAPIV1Beta1Router{
		gatewayAPIClient: v1beta1Mocks.meshClient,
		kubeClient:       v1beta1Mocks.kubeClient,
		logger:           v1beta1Mocks.logger,
	}

	return []gatewayRouterUnderTest{
		{
			name:      "v1",
			reconcile: v1Router.Reconcile,
			setRoutes: func(canary *flaggerv1.Canary, primaryWeight, canaryWeight int) error {
				return v1Router.SetRoutes(canary, primaryWeight, canaryWeight, false)
			},
			routeWrites: func() int {
				return countRouteWrites(v1Mocks.meshClient, "httproutes")
			},
			getRules: func(t *testing.T, canary *flaggerv1.Canary) []gatewayRuleUnderTest {
				route, err := v1Router.gatewayAPIClient.GatewayapiV1().HTTPRoutes(canary.Namespace).
					Get(context.TODO(), "podinfo", metav1.GetOptions{})
				require.NoError(t, err)
				rules := make([]gatewayRuleUnderTest, 0, len(route.Spec.Rules))
				for _, rule := range route.Spec.Rules {
					rules = append(rules, gatewayRuleUnderTest{
						Matches:     toGatewayMatchesV1(rule.Matches),
						BackendRefs: v1BackendRefNames(rule),
						SetCookies:  v1SetCookies(rule),
					})
				}
				return rules
			},
		},
		{
			name:      "v1beta1",
			reconcile: v1beta1Router.Reconcile,
			setRoutes: func(canary *flaggerv1.Canary, primaryWeight, canaryWeight int) error {
				return v1beta1Router.SetRoutes(canary, primaryWeight, canaryWeight, false)
			},
			routeWrites: func() int {
				return countRouteWrites(v1beta1Mocks.meshClient, "httproutes")
			},
			getRules: func(t *testing.T, canary *flaggerv1.Canary) []gatewayRuleUnderTest {
				route, err := v1beta1Router.gatewayAPIClient.GatewayapiV1beta1().HTTPRoutes(canary.Namespace).
					Get(context.TODO(), "podinfo", metav1.GetOptions{})
				require.NoError(t, err)
				rules := make([]gatewayRuleUnderTest, 0, len(route.Spec.Rules))
				for _, rule := range route.Spec.Rules {
					rules = append(rules, gatewayRuleUnderTest{
						Matches:     toGatewayMatchesV1Beta1(rule.Matches),
						BackendRefs: v1beta1BackendRefNames(rule),
						SetCookies:  v1beta1SetCookies(rule),
					})
				}
				return rules
			},
		},
	}
}

// countRouteWrites counts create/update calls made against the given resource.
// It relies on the fake clientset exposing its recorded actions.
func countRouteWrites(client clientset.Interface, resource string) int {
	testingClient, ok := client.(interface{ Actions() []k8stesting.Action })
	if !ok {
		return -1
	}
	writes := 0
	for _, action := range testingClient.Actions() {
		if (action.GetVerb() == "create" || action.GetVerb() == "update") &&
			action.GetResource().Resource == resource {
			writes++
		}
	}
	return writes
}

func toGatewayMatchesV1(matches []v1.HTTPRouteMatch) []gatewayMatch {
	result := make([]gatewayMatch, 0, len(matches))
	for _, match := range matches {
		out := gatewayMatch{}
		if match.Path != nil {
			out.Path = &gatewayPathMatch{}
			if match.Path.Type != nil {
				out.Path.Type = string(*match.Path.Type)
			}
			if match.Path.Value != nil {
				out.Path.Value = *match.Path.Value
			}
		}
		if match.Method != nil {
			out.Method = string(*match.Method)
		}
		for _, header := range match.Headers {
			h := gatewayHeaderMatch{Name: string(header.Name), Value: header.Value}
			if header.Type != nil {
				h.Type = gatewayMatchType(*header.Type)
			}
			out.Headers = append(out.Headers, h)
		}
		for _, param := range match.QueryParams {
			q := gatewayQueryMatch{Name: string(param.Name), Value: param.Value}
			if param.Type != nil {
				q.Type = gatewayMatchType(*param.Type)
			}
			out.QueryParams = append(out.QueryParams, q)
		}
		result = append(result, out)
	}
	return result
}

func toGatewayMatchesV1Beta1(matches []v1beta1.HTTPRouteMatch) []gatewayMatch {
	result := make([]gatewayMatch, 0, len(matches))
	for _, match := range matches {
		out := gatewayMatch{}
		if match.Path != nil {
			out.Path = &gatewayPathMatch{}
			if match.Path.Type != nil {
				out.Path.Type = string(*match.Path.Type)
			}
			if match.Path.Value != nil {
				out.Path.Value = *match.Path.Value
			}
		}
		if match.Method != nil {
			out.Method = string(*match.Method)
		}
		for _, header := range match.Headers {
			h := gatewayHeaderMatch{Name: string(header.Name), Value: header.Value}
			if header.Type != nil {
				h.Type = gatewayMatchType(*header.Type)
			}
			out.Headers = append(out.Headers, h)
		}
		for _, param := range match.QueryParams {
			q := gatewayQueryMatch{Name: param.Name, Value: param.Value}
			if param.Type != nil {
				q.Type = gatewayMatchType(*param.Type)
			}
			out.QueryParams = append(out.QueryParams, q)
		}
		result = append(result, out)
	}
	return result
}

func v1BackendRefNames(rule v1.HTTPRouteRule) []string {
	names := make([]string, 0, len(rule.BackendRefs))
	for _, ref := range rule.BackendRefs {
		names = append(names, string(ref.Name))
	}
	return names
}

func v1beta1BackendRefNames(rule v1beta1.HTTPRouteRule) []string {
	names := make([]string, 0, len(rule.BackendRefs))
	for _, ref := range rule.BackendRefs {
		names = append(names, string(ref.Name))
	}
	return names
}

// v1SetCookies returns the Set-Cookie values set by a rule, from both rule-level
// filters and backend-specific filters.
func v1SetCookies(rule v1.HTTPRouteRule) []string {
	var cookies []string
	collectV1Filters := func(filters []v1.HTTPRouteFilter) {
		for _, filter := range filters {
			if filter.ResponseHeaderModifier == nil {
				continue
			}
			for _, header := range filter.ResponseHeaderModifier.Add {
				if string(header.Name) == setCookieHeader {
					cookies = append(cookies, header.Value)
				}
			}
		}
	}
	collectV1Filters(rule.Filters)
	for _, ref := range rule.BackendRefs {
		collectV1Filters(ref.Filters)
	}
	return cookies
}

func v1beta1SetCookies(rule v1beta1.HTTPRouteRule) []string {
	var cookies []string
	collectFilters := func(filters []v1beta1.HTTPRouteFilter) {
		for _, filter := range filters {
			if filter.ResponseHeaderModifier == nil {
				continue
			}
			for _, header := range filter.ResponseHeaderModifier.Add {
				if string(header.Name) == setCookieHeader {
					cookies = append(cookies, header.Value)
				}
			}
		}
	}
	collectFilters(rule.Filters)
	for _, ref := range rule.BackendRefs {
		collectFilters(ref.Filters)
	}
	return cookies
}

// randomCookieValue matches the random value generated by randSeq when building
// session affinity cookies, so that the random part can be normalised before
// comparing the output of the two router implementations.
var randomCookieValue = regexp.MustCompile(`=[A-Za-z]{10}`)

// normalizeGatewayRules replaces the random session affinity cookie values, which
// differ between runs, with a stable placeholder.
func normalizeGatewayRules(rules []gatewayRuleUnderTest) []gatewayRuleUnderTest {
	normalize := func(s string) string {
		return randomCookieValue.ReplaceAllString(s, "=<value>")
	}
	out := make([]gatewayRuleUnderTest, 0, len(rules))
	for _, rule := range rules {
		normalized := gatewayRuleUnderTest{
			BackendRefs: rule.BackendRefs,
			SetCookies:  make([]string, 0, len(rule.SetCookies)),
		}
		for _, cookie := range rule.SetCookies {
			normalized.SetCookies = append(normalized.SetCookies, normalize(cookie))
		}
		for _, match := range rule.Matches {
			normalizedMatch := match.copy()
			for i := range normalizedMatch.Headers {
				normalizedMatch.Headers[i].Value = normalize(normalizedMatch.Headers[i].Value)
			}
			normalized.Matches = append(normalized.Matches, normalizedMatch)
		}
		out = append(out, normalized)
	}
	return out
}

func headerMatchRequests(name, value string) []istiov1beta1.HTTPMatchRequest {
	return []istiov1beta1.HTTPMatchRequest{{
		Headers: map[string]istioapiv1alpha1.StringMatch{name: {Exact: value}},
	}}
}

// newABTestGatewayCanary returns a canary fixture with A/B match conditions and
// optional service match conditions and session affinity.
func newABTestGatewayCanary(serviceMatch, analysisMatch []istiov1beta1.HTTPMatchRequest,
	sessionAffinity *flaggerv1.SessionAffinity) *flaggerv1.Canary {
	canary := newTestGatewayAPICanary()
	canary.Spec.Service.Match = serviceMatch
	canary.Spec.Analysis.Match = analysisMatch
	canary.Spec.Analysis.Iterations = 10
	canary.Spec.Analysis.SessionAffinity = sessionAffinity
	return canary
}

// forEachGatewayRouter runs the test body once per router implementation with a
// fresh fixture, and asserts that both implementations produce equal rules.
func forEachGatewayRouter(t *testing.T,
	setup func(canary *flaggerv1.Canary),
	body func(t *testing.T, router gatewayRouterUnderTest, canary *flaggerv1.Canary) []gatewayRuleUnderTest) {
	t.Helper()

	var results = map[string][]gatewayRuleUnderTest{}
	for _, router := range newGatewayRoutersUnderTest(nil) {
		canary := newTestGatewayAPICanary()
		if setup != nil {
			setup(canary)
		}
		results[router.name] = normalizeGatewayRules(body(t, router, canary))
	}

	require.Contains(t, results, "v1")
	require.Contains(t, results, "v1beta1")
	assert.Equal(t, results["v1"], results["v1beta1"],
		"v1 and v1beta1 must produce equivalent HTTPRoute rules")
}

func TestGatewayAPIRouters_ABMatchMerge(t *testing.T) {
	tests := []struct {
		name          string
		serviceMatch  []istiov1beta1.HTTPMatchRequest
		analysisMatch []istiov1beta1.HTTPMatchRequest
		wantMatches   []gatewayMatch
		wantErr       []string
	}{
		{
			name:          "headers from the service and the analysis are unioned",
			serviceMatch:  headerMatchRequests("x-tenant", "acme"),
			analysisMatch: headerMatchRequests("x-canary", "always"),
			wantMatches: []gatewayMatch{{
				Headers: []gatewayHeaderMatch{
					headerMatchHeader("x-canary", "always"),
					headerMatchHeader("x-tenant", "acme"),
				},
			}},
		},
		{
			name:          "analysis-only path prefix is carried into the merged match",
			serviceMatch:  headerMatchRequests("x-tenant", "acme"),
			analysisMatch: []istiov1beta1.HTTPMatchRequest{{Uri: &istioapiv1alpha1.StringMatch{Prefix: "/beta"}}},
			wantMatches: []gatewayMatch{{
				Path:    &gatewayPathMatch{Type: pathMatchPrefixType, Value: "/beta"},
				Headers: []gatewayHeaderMatch{headerMatchHeader("x-tenant", "acme")},
			}},
		},
		{
			name:          "analysis path prefix with no service conditions",
			analysisMatch: []istiov1beta1.HTTPMatchRequest{{Uri: &istioapiv1alpha1.StringMatch{Prefix: "/beta"}}},
			wantMatches: []gatewayMatch{{
				Path: &gatewayPathMatch{Type: pathMatchPrefixType, Value: "/beta"},
			}},
		},
		{
			name:         "analysis method and query params",
			serviceMatch: headerMatchRequests("x-tenant", "acme"),
			analysisMatch: []istiov1beta1.HTTPMatchRequest{{
				Method:      &istioapiv1alpha1.StringMatch{Exact: "POST"},
				QueryParams: map[string]istioapiv1alpha1.StringMatch{"debug": {Exact: "1"}},
			}},
			wantMatches: []gatewayMatch{{
				Method:      "POST",
				Headers:     []gatewayHeaderMatch{headerMatchHeader("x-tenant", "acme")},
				QueryParams: []gatewayQueryMatch{{Name: "debug", Value: "1", Type: gatewayMatchExact}},
			}},
		},
		{
			name:          "conflicting header values are rejected",
			serviceMatch:  headerMatchRequests("x-env", "prod"),
			analysisMatch: headerMatchRequests("x-env", "staging"),
			wantErr:       []string{"x-env", "prod", "staging"},
		},
		{
			name: "conflicting path match types are rejected",
			serviceMatch: []istiov1beta1.HTTPMatchRequest{{
				Uri: &istioapiv1alpha1.StringMatch{Prefix: "/api"},
			}},
			analysisMatch: []istiov1beta1.HTTPMatchRequest{{
				Uri: &istioapiv1alpha1.StringMatch{Exact: "/api"},
			}},
			wantErr: []string{"path match"},
		},
		{
			name:          "header names are compared case-insensitively",
			serviceMatch:  headerMatchRequests("X-Tenant", "acme"),
			analysisMatch: headerMatchRequests("x-tenant", "acme"),
			// The service spelling is kept as the base of the merge.
			wantMatches: []gatewayMatch{{
				Headers: []gatewayHeaderMatch{headerMatchHeader("X-Tenant", "acme")},
			}},
		},
		{
			name:          "unsupported analysis selector is reported",
			analysisMatch: []istiov1beta1.HTTPMatchRequest{{Uri: &istioapiv1alpha1.StringMatch{}}},
			wantErr:       []string{"path matching selector"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			forEachGatewayRouter(t, func(canary *flaggerv1.Canary) {
				canary.Spec.Service.Match = test.serviceMatch
				canary.Spec.Analysis.Match = test.analysisMatch
				canary.Spec.Analysis.Iterations = 10
			}, func(t *testing.T, router gatewayRouterUnderTest, canary *flaggerv1.Canary) []gatewayRuleUnderTest {
				err := router.reconcile(canary)
				if len(test.wantErr) > 0 {
					require.Error(t, err, "router %s should reject the configuration", router.name)
					for _, want := range test.wantErr {
						assert.Contains(t, err.Error(), want, "router %s", router.name)
					}
					return nil
				}

				require.NoError(t, err, "router %s", router.name)
				rules := router.getRules(t, canary)
				require.NotEmpty(t, rules)
				assert.Equal(t, test.wantMatches, rules[0].Matches, "router %s", router.name)
				return rules
			})
		})
	}
}

func TestGatewayAPIRouters_ABMatchRuleLayout(t *testing.T) {
	// The A/B rule routes weighted traffic and the second rule sends everything
	// else to primary only. Both must be present and consistent across versions.
	forEachGatewayRouter(t, func(canary *flaggerv1.Canary) {
		canary.Spec.Service.Match = headerMatchRequests("x-tenant", "acme")
		canary.Spec.Analysis.Match = headerMatchRequests("x-canary", "always")
		canary.Spec.Analysis.Iterations = 10
	}, func(t *testing.T, router gatewayRouterUnderTest, canary *flaggerv1.Canary) []gatewayRuleUnderTest {
		require.NoError(t, router.reconcile(canary))
		rules := router.getRules(t, canary)
		require.Len(t, rules, 2)

		assert.Equal(t, []string{"podinfo-primary", "podinfo-canary"}, rules[0].BackendRefs)
		assert.Equal(t, []string{"podinfo-primary"}, rules[1].BackendRefs)
		assert.Equal(t, []gatewayMatch{{
			Headers: []gatewayHeaderMatch{
				headerMatchHeader("x-canary", "always"),
				headerMatchHeader("x-tenant", "acme"),
			},
		}}, rules[0].Matches)

		// The non-A/B rule keeps the unmodified service match conditions.
		assert.Equal(t, []gatewayMatch{{
			Headers: []gatewayHeaderMatch{headerMatchHeader("x-tenant", "acme")},
		}}, rules[1].Matches)
		return rules
	})
}

func TestGatewayAPIRouters_SessionAffinityCookies(t *testing.T) {
	cases := []struct {
		name              string
		sessionAffinity   func(interval string) *flaggerv1.SessionAffinity
		wantRuleCount     int
		wantRegexName     string
		wantRegexValueRe  string
		wantPrimaryCookie bool
	}{
		{
			name: "without primary cookie",
			sessionAffinity: func(string) *flaggerv1.SessionAffinity {
				return &flaggerv1.SessionAffinity{CookieName: "flagger-cookie", MaxAge: 300}
			},
			wantRuleCount:     2,
			wantRegexName:     "flagger-cookie",
			wantPrimaryCookie: false,
		},
		{
			name: "with primary cookie",
			sessionAffinity: func(string) *flaggerv1.SessionAffinity {
				return &flaggerv1.SessionAffinity{
					CookieName:        "canary-cookie",
					PrimaryCookieName: "primary-cookie",
					MaxAge:            300,
				}
			},
			wantRuleCount:     3,
			wantRegexName:     "canary-cookie",
			wantPrimaryCookie: true,
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			forEachGatewayRouter(t, func(canary *flaggerv1.Canary) {
				canary.Spec.Analysis.Interval = "15s"
				canary.Spec.Analysis.SessionAffinity = test.sessionAffinity("15s")
			}, func(t *testing.T, router gatewayRouterUnderTest, canary *flaggerv1.Canary) []gatewayRuleUnderTest {
				require.NoError(t, router.reconcile(canary))
				require.NoError(t, router.setRoutes(canary, 90, 10))
				rules := router.getRules(t, canary)
				require.Len(t, rules, test.wantRuleCount)

				// The sticky canary rule matches on a Cookie header regex that contains the
				// exact "name=" pair of the cookie recorded in the status.
				stickyCanary := rules[0]
				require.NotEmpty(t, stickyCanary.Matches)
				require.NotEmpty(t, stickyCanary.Matches[0].Headers)
				cookieHeaderMatch := stickyCanary.Matches[0].Headers[0]
				assert.Equal(t, cookieHeader, cookieHeaderMatch.Name)
				assert.Equal(t, gatewayMatchRegex, cookieHeaderMatch.Type)
				assert.Contains(t, cookieHeaderMatch.Value, test.wantRegexName+"=")
				assert.Equal(t, []string{"podinfo-primary", "podinfo-canary"}, stickyCanary.BackendRefs)

				// The weighted rule sets the canary cookie, and the primary cookie when
				// a primary cookie name is configured.
				weighted := rules[len(rules)-1]
				if !test.wantPrimaryCookie {
					require.Len(t, weighted.SetCookies, 1)
					assert.Regexp(t, `^`+regexp.QuoteMeta(test.wantRegexName)+`=[A-Za-z]+; Max-Age=300`, weighted.SetCookies[0])
				} else {
					require.Len(t, weighted.SetCookies, 2)
					joined := weighted.SetCookies[0] + "|" + weighted.SetCookies[1]
					assert.Regexp(t, `canary-cookie=[A-Za-z]+; Max-Age=300`, joined)
					// The primary cookie max age is the analysis interval.
					assert.Regexp(t, `primary-cookie=[A-Za-z]+; Max-Age=15`, joined)

					primarySticky := rules[1]
					require.NotEmpty(t, primarySticky.Matches[0].Headers)
					assert.Contains(t, primarySticky.Matches[0].Headers[0].Value, "primary-cookie=")
					assert.Equal(t, []string{"podinfo-primary", "podinfo-canary"}, primarySticky.BackendRefs)
				}
				return rules
			})
		})
	}
}

func TestGatewayAPIRouters_SessionAffinityCleanup(t *testing.T) {
	// After the analysis finishes, both cookies must be invalidated so clients that
	// were pinned to the canary do not keep an obsolete cookie.
	forEachGatewayRouter(t, func(canary *flaggerv1.Canary) {
		canary.Spec.Analysis.Interval = "15s"
		canary.Spec.Analysis.SessionAffinity = &flaggerv1.SessionAffinity{
			CookieName:        "canary-cookie",
			PrimaryCookieName: "primary-cookie",
			MaxAge:            300,
		}
	}, func(t *testing.T, router gatewayRouterUnderTest, canary *flaggerv1.Canary) []gatewayRuleUnderTest {
		require.NoError(t, router.reconcile(canary))
		require.NoError(t, router.setRoutes(canary, 90, 10))

		canaryCookie := canary.Status.SessionAffinityCookie
		primaryCookie := canary.Status.PrimarySessionAffinityCookie
		require.NotEmpty(t, canaryCookie)
		require.NotEmpty(t, primaryCookie)

		// Promotion: weight goes back to primary, the analysis is over.
		require.NoError(t, router.setRoutes(canary, 100, 0))

		assert.Empty(t, canary.Status.SessionAffinityCookie)
		assert.Empty(t, canary.Status.PrimarySessionAffinityCookie)
		assert.Equal(t, canaryCookie, canary.Status.PreviousSessionAffinityCookie)

		rules := router.getRules(t, canary)
		require.NotEmpty(t, rules)

		// The sticky rule matches the previous cookie and expires both cookies.
		sticky := rules[0]
		require.NotEmpty(t, sticky.Matches[0].Headers)
		assert.Contains(t, sticky.Matches[0].Headers[0].Value, "canary-cookie=")

		expirations := sticky.SetCookies
		assert.Contains(t, expirations, fmt.Sprintf("%s; %s=%d", canaryCookie, maxAgeAttr, -1))
		assert.Contains(t, expirations, fmt.Sprintf("%s; %s=%d", primaryCookie, maxAgeAttr, -1))
		return rules
	})
}

func TestGatewayAPIRouters_ReconcileIsIdempotent(t *testing.T) {
	// The generated conditions are sorted, so reconciling twice must not update the
	// HTTPRoute the second time. This guards against spurious updates caused by a
	// non-deterministic ordering of header/query conditions.
	forEachGatewayRouter(t, func(canary *flaggerv1.Canary) {
		canary.Spec.Service.Match = []istiov1beta1.HTTPMatchRequest{{
			Uri: &istioapiv1alpha1.StringMatch{Prefix: "/api"},
			Headers: map[string]istioapiv1alpha1.StringMatch{
				"x-tenant": {Exact: "acme"},
				"X-Region": {Exact: "eu"},
			},
			QueryParams: map[string]istioapiv1alpha1.StringMatch{
				"debug": {Exact: "1"},
				"beta":  {Exact: "true"},
			},
		}}
		canary.Spec.Analysis.Match = []istiov1beta1.HTTPMatchRequest{{
			Headers: map[string]istioapiv1alpha1.StringMatch{
				"x-canary": {Exact: "always"},
				"x-slot":   {Regex: ".*-blue"},
			},
			Method: &istioapiv1alpha1.StringMatch{Exact: "GET"},
		}}
		canary.Spec.Analysis.Iterations = 10
	}, func(t *testing.T, router gatewayRouterUnderTest, canary *flaggerv1.Canary) []gatewayRuleUnderTest {
		require.NoError(t, router.reconcile(canary))
		before := router.getRules(t, canary)
		writesAfterCreate := router.routeWrites()
		require.Equal(t, 1, writesAfterCreate, "router %s should create the HTTPRoute once", router.name)

		require.NoError(t, router.reconcile(canary))
		after := router.getRules(t, canary)

		assert.Equal(t, before, after, "reconciling twice must produce identical rules")
		assert.Equal(t, writesAfterCreate, router.routeWrites(),
			"router %s must not update an unchanged HTTPRoute", router.name)
		return after
	})
}

func TestGatewayAPIRouters_ABWithSessionAffinity(t *testing.T) {
	// When both A/B match conditions and session affinity are configured, the merged
	// A/B conditions belong to the weighted routing rule, not to the sticky cookie
	// rules. Overwriting the first rule would silently disable the sticky routing.
	forEachGatewayRouter(t, func(canary *flaggerv1.Canary) {
		canary.Spec.Analysis.Interval = "15s"
		canary.Spec.Analysis.Iterations = 10
		canary.Spec.Service.Match = headerMatchRequests("x-tenant", "acme")
		canary.Spec.Analysis.Match = headerMatchRequests("x-canary", "always")
		canary.Spec.Analysis.SessionAffinity = &flaggerv1.SessionAffinity{
			CookieName:        "canary-cookie",
			PrimaryCookieName: "primary-cookie",
		}
	}, func(t *testing.T, router gatewayRouterUnderTest, canary *flaggerv1.Canary) []gatewayRuleUnderTest {
		require.NoError(t, router.reconcile(canary))
		require.NoError(t, router.setRoutes(canary, 90, 10))

		rules := router.getRules(t, canary)
		// sticky canary, sticky primary, weighted, primary-only A/B fallback
		require.Len(t, rules, 4)

		// Both sticky rules must keep their Cookie matcher and the service conditions.
		for idx, cookieName := range []string{"canary-cookie", "primary-cookie"} {
			rule := rules[idx]
			require.Len(t, rule.Matches, 1, "sticky rule %d", idx)
			headers := rule.Matches[0].Headers
			require.Len(t, headers, 2, "sticky rule %d must match the Cookie header plus the service headers", idx)
			assert.Equal(t, cookieHeader, headers[0].Name)
			assert.Contains(t, headers[0].Value, cookieName+"=")
			assert.Equal(t, headerMatchHeader("x-tenant", "acme"), headers[1])
		}

		// The weighted rule carries the merged A/B conditions.
		weighted := rules[2]
		assert.Equal(t, []gatewayMatch{{
			Headers: []gatewayHeaderMatch{
				headerMatchHeader("x-canary", "always"),
				headerMatchHeader("x-tenant", "acme"),
			},
		}}, weighted.Matches)

		// The fallback rule sends everything else to primary.
		assert.Equal(t, []string{"podinfo-primary"}, rules[3].BackendRefs)
		return rules
	})
}

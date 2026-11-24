/*
Copyright 2022 The Flux authors

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
	"slices"
	"strings"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"

	flaggerv1 "github.com/fluxcd/flagger/pkg/apis/flagger/v1beta1"
	"github.com/fluxcd/flagger/pkg/apis/gatewayapi/v1beta1"
	clientset "github.com/fluxcd/flagger/pkg/client/clientset/versioned"
)

var (
	v1beta1PathMatchType    = v1beta1.PathMatchPathPrefix
	v1beta1HeaderMatchRegex = v1beta1.HeaderMatchRegularExpression
)

type GatewayAPIV1Beta1Router struct {
	gatewayAPIClient clientset.Interface
	kubeClient       kubernetes.Interface
	logger           *zap.SugaredLogger
	setOwnerRefs     bool
}

func (gwr *GatewayAPIV1Beta1Router) Reconcile(canary *flaggerv1.Canary) error {
	if len(canary.Spec.Service.GatewayRefs) == 0 {
		return fmt.Errorf("GatewayRefs must be specified when using Gateway API as a provider.")
	}

	apexSvcName, primarySvcName, canarySvcName := canary.GetServiceNames()

	hrNamespace := canary.Namespace

	var hostNames []v1beta1.Hostname
	for _, host := range canary.Spec.Service.Hosts {
		hostNames = append(hostNames, v1beta1.Hostname(host))
	}
	serviceMatches, err := translateRouteMatches(canary.Spec.Service.Match)
	if err != nil {
		return fmt.Errorf("Invalid request matching selectors: %w", err)
	}
	mergeBase := serviceMatches
	matches := toV1Beta1RouteMatches(serviceMatches)
	if len(matches) == 0 {
		matches = append(matches, v1beta1.HTTPRouteMatch{
			Path: &v1beta1.HTTPPathMatch{
				Type:  &v1beta1PathMatchType,
				Value: &pathMatchValue,
			},
		})
	}

	httpRouteSpec := v1beta1.HTTPRouteSpec{
		CommonRouteSpec: v1beta1.CommonRouteSpec{
			ParentRefs: canary.Spec.Service.GatewayRefs,
		},
		Hostnames: hostNames,
		Rules: []v1beta1.HTTPRouteRule{
			{
				Matches: matches,
				Filters: gwr.makeFilters(canary),
				BackendRefs: []v1beta1.HTTPBackendRef{
					{
						BackendRef: gwr.makeBackendRef(primarySvcName, initialPrimaryWeight, canary.Spec.Service.Port),
					},
					{
						BackendRef: gwr.makeBackendRef(canarySvcName, initialCanaryWeight, canary.Spec.Service.Port),
					},
				},
			},
		},
	}

	// A/B testing
	if len(canary.GetAnalysis().Match) > 0 {
		analysisMatches, err := translateRouteMatches(canary.GetAnalysis().Match)
		if err != nil {
			return fmt.Errorf("Invalid analysis matching selectors: %w", err)
		}
		mergedMatches, err := mergeGatewayMatches(analysisMatches, mergeBase)
		if err != nil {
			return fmt.Errorf("Invalid analysis matching selectors: %w", err)
		}
		httpRouteSpec.Rules[0].Matches = toV1Beta1RouteMatches(mergedMatches)
		httpRouteSpec.Rules = append(httpRouteSpec.Rules, v1beta1.HTTPRouteRule{
			Matches: matches,
			Filters: gwr.makeFilters(canary),
			BackendRefs: []v1beta1.HTTPBackendRef{
				{
					BackendRef: gwr.makeBackendRef(primarySvcName, initialPrimaryWeight, canary.Spec.Service.Port),
				},
			},
		})
	}

	httpRoute, err := gwr.gatewayAPIClient.GatewayapiV1beta1().HTTPRoutes(hrNamespace).Get(
		context.TODO(), apexSvcName, metav1.GetOptions{},
	)

	newMetadata := canary.Spec.Service.Apex
	if newMetadata == nil {
		newMetadata = &flaggerv1.CustomMetadata{}
	}
	if newMetadata.Labels == nil {
		newMetadata.Labels = make(map[string]string)
	}
	if newMetadata.Annotations == nil {
		newMetadata.Annotations = make(map[string]string)
	}
	newMetadata.Annotations = filterMetadata(newMetadata.Annotations)

	if errors.IsNotFound(err) {
		route := &v1beta1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{
				Name:        apexSvcName,
				Namespace:   hrNamespace,
				Labels:      newMetadata.Labels,
				Annotations: newMetadata.Annotations,
			},
			Spec: httpRouteSpec,
		}

		if gwr.setOwnerRefs {
			route.OwnerReferences = []metav1.OwnerReference{
				*metav1.NewControllerRef(canary, schema.GroupVersionKind{
					Group:   flaggerv1.SchemeGroupVersion.Group,
					Version: flaggerv1.SchemeGroupVersion.Version,
					Kind:    flaggerv1.CanaryKind,
				}),
			}
		}

		_, err := gwr.gatewayAPIClient.GatewayapiV1beta1().HTTPRoutes(hrNamespace).
			Create(context.TODO(), route, metav1.CreateOptions{})

		if err != nil {
			return fmt.Errorf("HTTPRoute %s.%s create error: %w", apexSvcName, hrNamespace, err)
		}
		gwr.logger.With("canary", fmt.Sprintf("%s.%s", canary.Name, canary.Namespace)).
			Infof("HTTPRoute %s.%s created", route.GetName(), hrNamespace)
		return nil
	} else if err != nil {
		return fmt.Errorf("HTTPRoute %s.%s get error: %w", apexSvcName, hrNamespace, err)
	}

	ignoreCmpOptions := []cmp.Option{
		cmpopts.IgnoreFields(v1beta1.BackendRef{}, "Weight"),
		cmpopts.EquateEmpty(),
	}
	if canary.Spec.Analysis.SessionAffinity != nil {
		// Ignore the rules that do sticky routing, i.e. rules whose match contains the `Cookie`
		// header with the regular expression generated for one of the session affinity cookies
		// tracked in the Canary status. Those rules are produced by SetRoutes and cannot be
		// derived from the spec alone, so they must not be compared with the desired state.
		cookieRegexes := sessionAffinityCookieRegexes(canary)
		ignoreCookieRoutes := cmpopts.IgnoreSliceElements(func(r v1beta1.HTTPRouteRule) bool {
			for _, match := range r.Matches {
				for _, headerMatch := range match.Headers {
					if headerMatch.Type != nil && *headerMatch.Type == v1beta1HeaderMatchRegex &&
						headerMatch.Name == cookieHeader && cookieRegexes[headerMatch.Value] {
						return true
					}
				}
			}
			return false
		})

		ignoreCmpOptions = append(ignoreCmpOptions, ignoreCookieRoutes)
		// Ignore backend specific filters, since we use that to insert the `Set-Cookie` header in responses.
		ignoreCmpOptions = append(ignoreCmpOptions, cmpopts.IgnoreFields(v1beta1.HTTPBackendRef{}, "Filters"))
	}

	if canary.GetAnalysis().Mirror {
		// If a Canary run is in progress, the HTTPRoute rule will have an extra filter of type RequestMirror
		// which needs to be ignored so that the requests are mirrored to the canary deployment.
		inProgress := canary.Status.Phase == flaggerv1.CanaryPhaseWaiting || canary.Status.Phase == flaggerv1.CanaryPhaseProgressing ||
			canary.Status.Phase == flaggerv1.CanaryPhaseWaitingPromotion
		if inProgress {
			ignoreCmpOptions = append(ignoreCmpOptions, cmpopts.IgnoreFields(v1beta1.HTTPRouteRule{}, "Filters"))
		}
	}

	if httpRoute != nil {
		// Preserve the existing annotations added by other controllers such as AWS Gateway API Controller.
		mergedAnnotations := newMetadata.Annotations
		for key, val := range httpRoute.Annotations {
			if _, ok := mergedAnnotations[key]; !ok {
				mergedAnnotations[key] = val
			}
		}

		// Compare the existing HTTPRoute spec and metadata with the desired state.
		// If there are differences, update the HTTPRoute object.
		specDiff := cmp.Diff(
			httpRoute.Spec, httpRouteSpec,
			ignoreCmpOptions...,
		)
		labelsDiff := cmp.Diff(newMetadata.Labels, httpRoute.Labels, cmpopts.EquateEmpty())
		annotationsDiff := cmp.Diff(mergedAnnotations, httpRoute.Annotations, cmpopts.EquateEmpty())
		if (specDiff != "" && httpRoute.Name != "") || labelsDiff != "" || annotationsDiff != "" {
			hrClone := httpRoute.DeepCopy()
			hrClone.Spec = httpRouteSpec
			hrClone.ObjectMeta.Annotations = mergedAnnotations
			hrClone.ObjectMeta.Labels = newMetadata.Labels
			_, err := gwr.gatewayAPIClient.GatewayapiV1beta1().HTTPRoutes(hrNamespace).
				Update(context.TODO(), hrClone, metav1.UpdateOptions{})
			if err != nil {
				return fmt.Errorf("HTTPRoute %s.%s update error: %w while reconciling", hrClone.GetName(), hrNamespace, err)
			}
			gwr.logger.With("canary", fmt.Sprintf("%s.%s", canary.Name, canary.Namespace)).
				Infof("HTTPRoute %s.%s updated", hrClone.GetName(), hrNamespace)
		}
	}

	return nil
}

func (gwr *GatewayAPIV1Beta1Router) GetRoutes(canary *flaggerv1.Canary) (
	primaryWeight int,
	canaryWeight int,
	mirrored bool,
	err error,
) {
	apexSvcName, primarySvcName, canarySvcName := canary.GetServiceNames()
	hrNamespace := canary.Namespace
	httpRoute, err := gwr.gatewayAPIClient.GatewayapiV1beta1().HTTPRoutes(hrNamespace).Get(context.TODO(), apexSvcName, metav1.GetOptions{})
	if err != nil {
		err = fmt.Errorf("HTTPRoute %s.%s get error: %w", apexSvcName, hrNamespace, err)
		return
	}
	var weightedRule *v1beta1.HTTPRouteRule
	for _, rule := range httpRoute.Spec.Rules {
		// If session affinity is enabled, then we are only interested in the rule
		// that has backend-specific filters, as that's the rule that does weighted
		// routing.
		if canary.Spec.Analysis.SessionAffinity != nil {
			for _, backendRef := range rule.BackendRefs {
				if len(backendRef.Filters) > 0 {
					weightedRule = &rule
				}
			}
		}

		// A/B testing: Avoid reading the rule with only for backendRef.
		if len(rule.BackendRefs) == 2 {
			for _, backendRef := range rule.BackendRefs {
				if backendRef.Name == v1beta1.ObjectName(primarySvcName) {
					primaryWeight = int(*backendRef.Weight)
				}
				if backendRef.Name == v1beta1.ObjectName(canarySvcName) {
					canaryWeight = int(*backendRef.Weight)
				}
			}
		}
		for _, filter := range rule.Filters {
			if filter.Type == v1beta1.HTTPRouteFilterRequestMirror && filter.RequestMirror != nil &&
				string(filter.RequestMirror.BackendRef.Name) == canarySvcName {
				mirrored = true
			}
		}
	}

	if weightedRule != nil {
		for _, backendRef := range weightedRule.BackendRefs {
			if backendRef.Name == v1beta1.ObjectName(primarySvcName) {
				primaryWeight = int(*backendRef.Weight)
			}
			if backendRef.Name == v1beta1.ObjectName(canarySvcName) {
				canaryWeight = int(*backendRef.Weight)
			}
		}
	}
	return
}

func (gwr *GatewayAPIV1Beta1Router) SetRoutes(
	canary *flaggerv1.Canary,
	primaryWeight int,
	canaryWeight int,
	mirrored bool,
) error {
	pWeight := int32(primaryWeight)
	cWeight := int32(canaryWeight)
	apexSvcName, primarySvcName, canarySvcName := canary.GetServiceNames()
	hrNamespace := canary.Namespace
	httpRoute, err := gwr.gatewayAPIClient.GatewayapiV1beta1().HTTPRoutes(hrNamespace).Get(context.TODO(), apexSvcName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("HTTPRoute %s.%s get error: %w", apexSvcName, hrNamespace, err)
	}
	hrClone := httpRoute.DeepCopy()
	hostNames := []v1beta1.Hostname{}
	for _, host := range canary.Spec.Service.Hosts {
		hostNames = append(hostNames, v1beta1.Hostname(host))
	}
	serviceMatches, err := translateRouteMatches(canary.Spec.Service.Match)
	if err != nil {
		return fmt.Errorf("Invalid request matching selectors: %w", err)
	}
	mergeBase := serviceMatches
	matches := toV1Beta1RouteMatches(serviceMatches)
	if len(matches) == 0 {
		matches = append(matches, v1beta1.HTTPRouteMatch{
			Path: &v1beta1.HTTPPathMatch{
				Type:  &v1beta1PathMatchType,
				Value: &pathMatchValue,
			},
		})
	}
	weightedRouteRule := &v1beta1.HTTPRouteRule{
		Matches: matches,
		Filters: gwr.makeFilters(canary),
		BackendRefs: []v1beta1.HTTPBackendRef{
			{
				BackendRef: gwr.makeBackendRef(primarySvcName, pWeight, canary.Spec.Service.Port),
			},
			{
				BackendRef: gwr.makeBackendRef(canarySvcName, cWeight, canary.Spec.Service.Port),
			},
		},
	}

	// If B/G mirroring is enabled, then add a route filter which mirrors the traffic
	// to the canary service.
	if mirrored && canary.GetAnalysis().Iterations > 0 {
		weightedRouteRule.Filters = append(weightedRouteRule.Filters, v1beta1.HTTPRouteFilter{
			Type: v1beta1.HTTPRouteFilterRequestMirror,
			RequestMirror: &v1beta1.HTTPRequestMirrorFilter{
				BackendRef: v1beta1.BackendObjectReference{
					Group: (*v1beta1.Group)(&backendRefGroup),
					Kind:  (*v1beta1.Kind)(&backendRefKind),
					Name:  v1beta1.ObjectName(canarySvcName),
					Port:  (*v1beta1.PortNumber)(&canary.Spec.Service.Port),
				},
			},
		})
	}

	httpRouteSpec := v1beta1.HTTPRouteSpec{
		CommonRouteSpec: v1beta1.CommonRouteSpec{
			ParentRefs: canary.Spec.Service.GatewayRefs,
		},
		Hostnames: hostNames,
		Rules: []v1beta1.HTTPRouteRule{
			*weightedRouteRule,
		},
	}

	if canary.Spec.Analysis.SessionAffinity != nil {
		rules, err := gwr.getSessionAffinityRouteRules(canary, canaryWeight, weightedRouteRule)
		if err != nil {
			return err
		}
		httpRouteSpec.Rules = rules
	}

	hrClone.Spec = httpRouteSpec

	// The weighted routing rule is the one that carries the A/B match conditions. With
	// session affinity the sticky rules come first and the weighted rule is last, so it
	// cannot be addressed by a fixed index.
	weightedRuleIndex := 0
	if canary.Spec.Analysis.SessionAffinity != nil {
		weightedRuleIndex = len(hrClone.Spec.Rules) - 1
	}

	// A/B testing
	if len(canary.GetAnalysis().Match) > 0 {
		analysisMatches, err := translateRouteMatches(canary.GetAnalysis().Match)
		if err != nil {
			return fmt.Errorf("Invalid analysis matching selectors: %w", err)
		}
		mergedMatches, err := mergeGatewayMatches(analysisMatches, mergeBase)
		if err != nil {
			return fmt.Errorf("Invalid analysis matching selectors: %w", err)
		}
		hrClone.Spec.Rules[weightedRuleIndex].Matches = toV1Beta1RouteMatches(mergedMatches)

		primaryOnlyRule := v1beta1.HTTPRouteRule{
			Matches: matches,
			Filters: gwr.makeFilters(canary),
			BackendRefs: []v1beta1.HTTPBackendRef{
				{
					BackendRef: gwr.makeBackendRef(primarySvcName, initialPrimaryWeight, canary.Spec.Service.Port),
				},
			},
		}
		hrClone.Spec.Rules = append(hrClone.Spec.Rules, primaryOnlyRule)
	}

	_, err = gwr.gatewayAPIClient.GatewayapiV1beta1().HTTPRoutes(hrNamespace).Update(context.TODO(), hrClone, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("HTTPRoute %s.%s update error: %w while setting weights", hrClone.GetName(), hrNamespace, err)
	}

	return nil
}

func (gwr *GatewayAPIV1Beta1Router) Finalize(_ *flaggerv1.Canary) error {
	return nil
}

// getSessionAffinityRouteRules returns the HTTPRouteRule objects required to perform
// session affinity based Canary releases.
func (gwr *GatewayAPIV1Beta1Router) getSessionAffinityRouteRules(canary *flaggerv1.Canary, canaryWeight int,
	weightedRouteRule *v1beta1.HTTPRouteRule) ([]v1beta1.HTTPRouteRule, error) {
	_, primarySvcName, canarySvcName := canary.GetServiceNames()
	stickyRouteRule := *weightedRouteRule

	// If a canary run is active, we want all responses corresponding to requests hitting the canary deployment
	// (due to weighted routing) to include a `Set-Cookie` header. All requests that have the `Cookie` header
	// and match the value of the `Set-Cookie` header will be routed to the canary deployment.
	if canaryWeight != 0 {
		if canary.Status.SessionAffinityCookie == "" {
			canary.Status.SessionAffinityCookie = fmt.Sprintf("%s=%s", canary.Spec.Analysis.SessionAffinity.CookieName, randSeq())
		}
		if canary.Spec.Analysis.SessionAffinity.PrimaryCookieName != "" &&
			canary.Status.PrimarySessionAffinityCookie == "" {
			canary.Status.PrimarySessionAffinityCookie = fmt.Sprintf("%s=%s", canary.Spec.Analysis.SessionAffinity.PrimaryCookieName, randSeq())
		}

		// Add `Set-Cookie` header modifier to the canary backend in the weighted routing rule.
		for i, backendRef := range weightedRouteRule.BackendRefs {
			if string(backendRef.BackendObjectReference.Name) == canarySvcName {
				backendRef.Filters = append(backendRef.Filters, v1beta1.HTTPRouteFilter{
					Type: v1beta1.HTTPRouteFilterResponseHeaderModifier,
					ResponseHeaderModifier: &v1beta1.HTTPHeaderFilter{
						Add: []v1beta1.HTTPHeader{
							{
								Name:  setCookieHeader,
								Value: canary.Spec.Analysis.SessionAffinity.BuildCookie(canary.Status.SessionAffinityCookie, canary.Spec.Analysis.SessionAffinity.GetMaxAge()),
							},
						},
					},
				})
			}
			weightedRouteRule.BackendRefs[i] = backendRef
		}

		// Add the primary cookie to the primary backend in the weighted routing rule, only if a
		// primary cookie name has been specified.
		if canary.Spec.Analysis.SessionAffinity.PrimaryCookieName != "" {
			interval, err := time.ParseDuration(canary.Spec.Analysis.Interval)
			if err != nil {
				return nil, fmt.Errorf("failed to parse canary interval: %w", err)
			}
			for i, backendRef := range weightedRouteRule.BackendRefs {
				if string(backendRef.BackendObjectReference.Name) == primarySvcName {
					backendRef.Filters = append(backendRef.Filters, v1beta1.HTTPRouteFilter{
						Type: v1beta1.HTTPRouteFilterResponseHeaderModifier,
						ResponseHeaderModifier: &v1beta1.HTTPHeaderFilter{
							Add: []v1beta1.HTTPHeader{
								{
									Name:  setCookieHeader,
									Value: canary.Spec.Analysis.SessionAffinity.BuildCookie(canary.Status.PrimarySessionAffinityCookie, int(interval.Seconds())),
								},
							},
						},
					})
				}
				weightedRouteRule.BackendRefs[i] = backendRef
			}
		}

		svcMatches, err := translateRouteMatches(canary.Spec.Service.Match)
		if err != nil {
			return nil, err
		}

		// Add `Cookie` header matcher to the sticky routing rule.
		cookieMatch, err := sessionAffinityCookieMatch(canary.Status.SessionAffinityCookie)
		if err != nil {
			return nil, err
		}
		mergedMatches, err := mergeGatewayMatches([]gatewayMatch{cookieMatch}, svcMatches)
		if err != nil {
			return nil, err
		}
		stickyCanaryRouteRule := *weightedRouteRule
		stickyCanaryRouteRule.Matches = toV1Beta1RouteMatches(mergedMatches)
		stickyCanaryRouteRule.BackendRefs = []v1beta1.HTTPBackendRef{
			{
				BackendRef: gwr.makeBackendRef(primarySvcName, 0, canary.Spec.Service.Port),
			},
			{
				BackendRef: gwr.makeBackendRef(canarySvcName, 100, canary.Spec.Service.Port),
			},
		}

		// Configure a sticky rule for the primary cookie, only if a primary cookie name has been
		// specified.
		if canary.Spec.Analysis.SessionAffinity.PrimaryCookieName != "" {
			primaryCookieMatch, err := sessionAffinityCookieMatch(canary.Status.PrimarySessionAffinityCookie)
			if err != nil {
				return nil, err
			}
			mergedMatches, err := mergeGatewayMatches([]gatewayMatch{primaryCookieMatch}, svcMatches)
			if err != nil {
				return nil, err
			}
			stickyPrimaryRouteRule := *weightedRouteRule
			stickyPrimaryRouteRule.Matches = toV1Beta1RouteMatches(mergedMatches)
			stickyPrimaryRouteRule.BackendRefs = []v1beta1.HTTPBackendRef{
				{
					BackendRef: gwr.makeBackendRef(primarySvcName, 100, canary.Spec.Service.Port),
				},
				{
					BackendRef: gwr.makeBackendRef(canarySvcName, 0, canary.Spec.Service.Port),
				},
			}
			return []v1beta1.HTTPRouteRule{stickyCanaryRouteRule, stickyPrimaryRouteRule, *weightedRouteRule}, nil
		}

		return []v1beta1.HTTPRouteRule{stickyCanaryRouteRule, *weightedRouteRule}, nil
	} else {
		// If canary weight is 0 and SessionAffinityCookie is non-blank, then it belongs to a previous canary run.
		if canary.Status.SessionAffinityCookie != "" {
			canary.Status.PreviousSessionAffinityCookie = canary.Status.SessionAffinityCookie
		}
		previousCookie := canary.Status.PreviousSessionAffinityCookie

		// The analysis has finished (either promoted or rolled back): both cookies must be
		// invalidated, otherwise clients that were pinned to the canary keep sending a cookie
		// for which no sticky rule exists anymore.
		cookiesToExpire := []string{}
		if previousCookie != "" {
			cookiesToExpire = append(cookiesToExpire, previousCookie)
		}
		if canary.Status.PrimarySessionAffinityCookie != "" {
			cookiesToExpire = append(cookiesToExpire, canary.Status.PrimarySessionAffinityCookie)
		}

		// Match against the previous session cookie and delete that cookie
		if previousCookie != "" {
			cookieMatch, err := sessionAffinityCookieMatch(previousCookie)
			if err != nil {
				return nil, err
			}
			svcMatches, err := translateRouteMatches(canary.Spec.Service.Match)
			if err != nil {
				return nil, err
			}
			mergedMatches, err := mergeGatewayMatches([]gatewayMatch{cookieMatch}, svcMatches)
			if err != nil {
				return nil, err
			}
			stickyRouteRule.Matches = toV1Beta1RouteMatches(mergedMatches)

			expireHeaders := make([]v1beta1.HTTPHeader, 0, len(cookiesToExpire))
			for _, cookie := range cookiesToExpire {
				expireHeaders = append(expireHeaders, v1beta1.HTTPHeader{
					Name:  setCookieHeader,
					Value: fmt.Sprintf("%s; %s=%d", cookie, maxAgeAttr, -1),
				})
			}
			stickyRouteRule.Filters = append(stickyRouteRule.Filters, v1beta1.HTTPRouteFilter{
				Type: v1beta1.HTTPRouteFilterResponseHeaderModifier,
				ResponseHeaderModifier: &v1beta1.HTTPHeaderFilter{
					Add: expireHeaders,
				},
			})
		}

		canary.Status.SessionAffinityCookie = ""
		canary.Status.PrimarySessionAffinityCookie = ""
	}

	return []v1beta1.HTTPRouteRule{stickyRouteRule, *weightedRouteRule}, nil
}

func (gwr *GatewayAPIV1Beta1Router) makeBackendRef(svcName string, weight, port int32) v1beta1.BackendRef {
	return v1beta1.BackendRef{
		BackendObjectReference: v1beta1.BackendObjectReference{
			Group: (*v1beta1.Group)(&backendRefGroup),
			Kind:  (*v1beta1.Kind)(&backendRefKind),
			Name:  v1beta1.ObjectName(svcName),
			Port:  (*v1beta1.PortNumber)(&port),
		},
		Weight: &weight,
	}
}

func sortFiltersV1beta1(headers []v1beta1.HTTPHeader) {

	if headers != nil {
		slices.SortFunc(headers, func(a, b v1beta1.HTTPHeader) int {
			if a.Name == b.Name {
				return strings.Compare(a.Value, b.Value)
			}
			return strings.Compare(string(a.Name), string(b.Name))
		})
	}
}

func (gwr *GatewayAPIV1Beta1Router) makeFilters(canary *flaggerv1.Canary) []v1beta1.HTTPRouteFilter {
	var filters []v1beta1.HTTPRouteFilter

	if canary.Spec.Service.Headers != nil {
		if canary.Spec.Service.Headers.Request != nil {
			requestHeaderFilter := v1beta1.HTTPRouteFilter{
				Type:                  v1beta1.HTTPRouteFilterRequestHeaderModifier,
				RequestHeaderModifier: &v1beta1.HTTPHeaderFilter{},
			}

			for name, val := range canary.Spec.Service.Headers.Request.Add {
				requestHeaderFilter.RequestHeaderModifier.Add = append(requestHeaderFilter.RequestHeaderModifier.Add, v1beta1.HTTPHeader{
					Name:  v1beta1.HTTPHeaderName(name),
					Value: val,
				})
			}
			sortFiltersV1beta1(requestHeaderFilter.RequestHeaderModifier.Add)
			for name, val := range canary.Spec.Service.Headers.Request.Set {
				requestHeaderFilter.RequestHeaderModifier.Set = append(requestHeaderFilter.RequestHeaderModifier.Set, v1beta1.HTTPHeader{
					Name:  v1beta1.HTTPHeaderName(name),
					Value: val,
				})
			}
			sortFiltersV1beta1(requestHeaderFilter.RequestHeaderModifier.Set)

			for _, name := range canary.Spec.Service.Headers.Request.Remove {
				requestHeaderFilter.RequestHeaderModifier.Remove = append(requestHeaderFilter.RequestHeaderModifier.Remove, name)
			}

			filters = append(filters, requestHeaderFilter)
		}
		if canary.Spec.Service.Headers.Response != nil {
			responseHeaderFilter := v1beta1.HTTPRouteFilter{
				Type:                   v1beta1.HTTPRouteFilterResponseHeaderModifier,
				ResponseHeaderModifier: &v1beta1.HTTPHeaderFilter{},
			}

			for name, val := range canary.Spec.Service.Headers.Response.Add {
				responseHeaderFilter.ResponseHeaderModifier.Add = append(responseHeaderFilter.ResponseHeaderModifier.Add, v1beta1.HTTPHeader{
					Name:  v1beta1.HTTPHeaderName(name),
					Value: val,
				})
			}
			sortFiltersV1beta1(responseHeaderFilter.ResponseHeaderModifier.Add)
			for name, val := range canary.Spec.Service.Headers.Response.Set {
				responseHeaderFilter.ResponseHeaderModifier.Set = append(responseHeaderFilter.ResponseHeaderModifier.Set, v1beta1.HTTPHeader{
					Name:  v1beta1.HTTPHeaderName(name),
					Value: val,
				})
			}
			sortFiltersV1beta1(responseHeaderFilter.ResponseHeaderModifier.Set)

			for _, name := range canary.Spec.Service.Headers.Response.Remove {
				responseHeaderFilter.ResponseHeaderModifier.Remove = append(responseHeaderFilter.ResponseHeaderModifier.Remove, name)
			}

			filters = append(filters, responseHeaderFilter)
		}
	}

	if canary.Spec.Service.Rewrite != nil {
		rewriteFilter := v1beta1.HTTPRouteFilter{
			Type:       v1beta1.HTTPRouteFilterURLRewrite,
			URLRewrite: &v1beta1.HTTPURLRewriteFilter{},
		}
		if canary.Spec.Service.Rewrite.Authority != "" {
			hostname := v1beta1.PreciseHostname(canary.Spec.Service.Rewrite.Authority)
			rewriteFilter.URLRewrite.Hostname = &hostname
		}
		if canary.Spec.Service.Rewrite.Uri != "" {
			rewriteFilter.URLRewrite.Path = &v1beta1.HTTPPathModifier{
				Type: v1beta1.HTTPPathModifierType(canary.Spec.Service.Rewrite.GetType()),
			}
			if rewriteFilter.URLRewrite.Path.Type == v1beta1.FullPathHTTPPathModifier {
				rewriteFilter.URLRewrite.Path.ReplaceFullPath = &canary.Spec.Service.Rewrite.Uri
			} else {
				rewriteFilter.URLRewrite.Path.ReplacePrefixMatch = &canary.Spec.Service.Rewrite.Uri
			}
		}

		filters = append(filters, rewriteFilter)
	}

	for _, mirror := range canary.Spec.Service.Mirror {
		mirror := mirror
		mirrorFilter := v1beta1.HTTPRouteFilter{
			Type:          v1beta1.HTTPRouteFilterRequestMirror,
			RequestMirror: &mirror,
		}
		filters = append(filters, mirrorFilter)
	}

	return filters
}

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
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	flaggerv1 "github.com/fluxcd/flagger/pkg/apis/flagger/v1beta1"
	v1 "github.com/fluxcd/flagger/pkg/apis/gatewayapi/v1"
	"github.com/fluxcd/flagger/pkg/apis/gatewayapi/v1beta1"
	istioapiv1alpha1 "github.com/fluxcd/flagger/pkg/apis/istio/common/v1alpha1"
	istiov1beta1 "github.com/fluxcd/flagger/pkg/apis/istio/v1beta1"
)

// The Gateway API provider exists in two flavours (gatewayapi/v1 and
// gatewayapi/v1beta1) which are almost identical, but whose Go types are
// distinct. To guarantee that both produce the same HTTPRoute matches for the
// same Canary spec, the translation of Istio-style HTTPMatchRequest objects and
// the merging of A/B match conditions with service match conditions are done on
// the API-version-agnostic structures below and are only mapped to the concrete
// API types at the very end.

// gatewayMatchType mirrors the only two match types that Gateway API supports
// for header and query parameter matches.
type gatewayMatchType string

const (
	gatewayMatchExact gatewayMatchType = "Exact"
	gatewayMatchRegex gatewayMatchType = "RegularExpression"
)

// Gateway API path match types. They happen to share the "Exact" spelling with
// the header/query match types but are a distinct enum.
const (
	pathMatchExactType  = "Exact"
	pathMatchPrefixType = "PathPrefix"
	pathMatchRegexType  = "RegularExpression"
)

// gatewayPathMatch is the version-agnostic representation of a path matcher.
type gatewayPathMatch struct {
	Type  string
	Value string
}

// gatewayHeaderMatch is the version-agnostic representation of a header
// matcher. Name matching is case-insensitive, which is the HTTP semantic.
type gatewayHeaderMatch struct {
	Name  string
	Value string
	Type  gatewayMatchType
}

// gatewayQueryMatch is the version-agnostic representation of a query
// parameter matcher. Query parameter names are case-sensitive.
type gatewayQueryMatch struct {
	Name  string
	Value string
	Type  gatewayMatchType
}

// gatewayMatch is the version-agnostic representation of a single HTTPRoute
// match. All fields present on a match must be satisfied by a request (logical
// AND). The elements of Headers and QueryParams are sorted by name so that the
// generated objects are stable across reconciliations.
type gatewayMatch struct {
	Path        *gatewayPathMatch
	Method      string
	Headers     []gatewayHeaderMatch
	QueryParams []gatewayQueryMatch
}

// ErrUnsupportedMatch is returned when an Istio-style match selector cannot be
// expressed with Gateway API matches. It is never silently dropped.
var ErrUnsupportedMatch = errors.New("unsupported match condition")

// ErrConflictingMatch is returned when the A/B match conditions and the service
// match conditions cannot be satisfied by the same request, or when a single
// match requires two different values for the same dimension.
var ErrConflictingMatch = errors.New("conflicting match conditions")

func (m gatewayMatch) copy() gatewayMatch {
	out := m
	if m.Path != nil {
		path := *m.Path
		out.Path = &path
	}
	out.Headers = append([]gatewayHeaderMatch(nil), m.Headers...)
	out.QueryParams = append([]gatewayQueryMatch(nil), m.QueryParams...)
	return out
}

func (m gatewayMatch) isEmpty() bool {
	return m.Path == nil && m.Method == "" && len(m.Headers) == 0 && len(m.QueryParams) == 0
}

// translateRouteMatches converts Istio-style HTTPMatchRequest selectors into the
// version-agnostic representation. Map keys are converted to sorted slices and
// unsupported selectors (header prefix/suffix matches, empty presence matches,
// non-exact methods) are reported as errors instead of being dropped.
func translateRouteMatches(requestMatches []istiov1beta1.HTTPMatchRequest) ([]gatewayMatch, error) {
	matches := make([]gatewayMatch, 0, len(requestMatches))
	for _, requestMatch := range requestMatches {
		match, err := translateRouteMatch(requestMatch)
		if err != nil {
			return nil, err
		}
		// An entirely empty request match carries no conditions; keep the
		// previous behaviour of ignoring it.
		if match.isEmpty() {
			continue
		}
		matches = append(matches, match)
	}
	return matches, nil
}

func translateRouteMatch(requestMatch istiov1beta1.HTTPMatchRequest) (gatewayMatch, error) {
	match := gatewayMatch{}

	if requestMatch.Uri != nil {
		switch {
		case requestMatch.Uri.Regex != "":
			match.Path = &gatewayPathMatch{Type: pathMatchRegexType, Value: requestMatch.Uri.Regex}
		case requestMatch.Uri.Exact != "":
			match.Path = &gatewayPathMatch{Type: pathMatchExactType, Value: requestMatch.Uri.Exact}
		case requestMatch.Uri.Prefix != "":
			match.Path = &gatewayPathMatch{Type: pathMatchPrefixType, Value: requestMatch.Uri.Prefix}
		default:
			return gatewayMatch{}, fmt.Errorf(
				"%w: Gateway API doesn't support the specified path matching selector: %+v",
				ErrUnsupportedMatch, requestMatch.Uri)
		}
	}

	if requestMatch.Method != nil {
		if requestMatch.Method.Exact == "" {
			return gatewayMatch{}, fmt.Errorf(
				"%w: Gateway API doesn't support the specified method matching selector: %+v",
				ErrUnsupportedMatch, requestMatch.Method)
		}
		match.Method = requestMatch.Method.Exact
	}

	headers, err := translateHeaderMatches(requestMatch.Headers)
	if err != nil {
		return gatewayMatch{}, err
	}
	match.Headers = headers

	queryParams, err := translateQueryMatches(requestMatch.QueryParams)
	if err != nil {
		return gatewayMatch{}, err
	}
	match.QueryParams = queryParams

	return match, nil
}

// translateHeaderMatches converts a header match map into a sorted slice with
// case-insensitively unique names.
func translateHeaderMatches(headers map[string]istioapiv1alpha1.StringMatch) ([]gatewayHeaderMatch, error) {
	if len(headers) == 0 {
		return nil, nil
	}

	matches := make([]gatewayHeaderMatch, 0, len(headers))
	for key, val := range headers {
		switch {
		case val.Exact != "":
			matches = append(matches, gatewayHeaderMatch{Name: key, Value: val.Exact, Type: gatewayMatchExact})
		case val.Regex != "":
			matches = append(matches, gatewayHeaderMatch{Name: key, Value: val.Regex, Type: gatewayMatchRegex})
		default:
			// This includes prefix/suffix matches and empty value presence
			// matches, neither of which Gateway API can express.
			return nil, fmt.Errorf(
				"%w: Gateway API doesn't support the specified header matching selector: %s: %+v",
				ErrUnsupportedMatch, key, val)
		}
	}

	sortGatewayHeaders(matches)
	return deduplicateGatewayHeaders(matches)
}

// translateQueryMatches converts a query parameter match map into a sorted
// slice with exact (case-sensitive) names.
func translateQueryMatches(queryParams map[string]istioapiv1alpha1.StringMatch) ([]gatewayQueryMatch, error) {
	if len(queryParams) == 0 {
		return nil, nil
	}

	matches := make([]gatewayQueryMatch, 0, len(queryParams))
	for key, val := range queryParams {
		switch {
		case val.Exact != "":
			matches = append(matches, gatewayQueryMatch{Name: key, Value: val.Exact, Type: gatewayMatchExact})
		case val.Regex != "":
			matches = append(matches, gatewayQueryMatch{Name: key, Value: val.Regex, Type: gatewayMatchRegex})
		default:
			return nil, fmt.Errorf(
				"%w: Gateway API doesn't support the specified query matching selector: %s: %+v",
				ErrUnsupportedMatch, key, val)
		}
	}

	sortGatewayQueryParams(matches)
	return deduplicateGatewayQueryParams(matches)
}

func sortGatewayHeaders(headers []gatewayHeaderMatch) {
	slices.SortFunc(headers, func(a, b gatewayHeaderMatch) int {
		if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		if c := strings.Compare(string(a.Type), string(b.Type)); c != 0 {
			return c
		}
		return strings.Compare(a.Value, b.Value)
	})
}

func sortGatewayQueryParams(params []gatewayQueryMatch) {
	slices.SortFunc(params, func(a, b gatewayQueryMatch) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		if c := strings.Compare(string(a.Type), string(b.Type)); c != 0 {
			return c
		}
		return strings.Compare(a.Value, b.Value)
	})
}

// headerConflictName renders a header name for a conflict message, showing both
// spellings when the two matchers use different casing of the same name.
func headerConflictName(a, b string) string {
	if a == b {
		return a
	}
	return a + "/" + b
}

// deduplicateGatewayHeaders keeps exactly one matcher per header name, treating
// names case-insensitively. Two matchers for the same name that require
// different values or match types cannot both be satisfied and are reported as a
// conflict.
func deduplicateGatewayHeaders(headers []gatewayHeaderMatch) ([]gatewayHeaderMatch, error) {
	unique := make([]gatewayHeaderMatch, 0, len(headers))
	for _, header := range headers {
		if len(unique) > 0 && strings.EqualFold(unique[len(unique)-1].Name, header.Name) {
			previous := unique[len(unique)-1]
			if previous.Type != header.Type || previous.Value != header.Value {
				return nil, fmt.Errorf(
					"%w: header %q is required to be both %s %q and %s %q",
					ErrConflictingMatch, headerConflictName(previous.Name, header.Name),
					previous.Type, previous.Value, header.Type, header.Value)
			}
			continue
		}
		unique = append(unique, header)
	}
	return unique, nil
}

func deduplicateGatewayQueryParams(params []gatewayQueryMatch) ([]gatewayQueryMatch, error) {
	unique := make([]gatewayQueryMatch, 0, len(params))
	for _, param := range params {
		if len(unique) > 0 && unique[len(unique)-1].Name == param.Name {
			previous := unique[len(unique)-1]
			if previous.Type != param.Type || previous.Value != param.Value {
				return nil, fmt.Errorf(
					"%w: query parameter %q is required to be both %s %q and %s %q",
					ErrConflictingMatch, param.Name,
					previous.Type, previous.Value, param.Type, param.Value)
			}
			continue
		}
		unique = append(unique, param)
	}
	return unique, nil
}

// mergeGatewayMatches combines the A/B analysis conditions with the service
// conditions. Every analysis condition is combined with every service
// condition, so the result keeps the cartesian product semantics of the Istio
// provider. For each pair:
//
//   - headers and query parameters are unioned by name (header names are
//     compared case-insensitively). The same name with the same match type and
//     value collapses into one condition; the same name with a different value
//     or type is a conflict and is reported as an error.
//   - path and method are taken from whichever side defines them. If both sides
//     define them, they must be identical; otherwise the combination is
//     unsatisfiable and is reported as an error.
//
// Inputs are expected to already be sorted and free of duplicates (as produced
// by translateRouteMatches), which makes the order of the result deterministic.
func mergeGatewayMatches(analysis, service []gatewayMatch) ([]gatewayMatch, error) {
	if len(analysis) == 0 {
		return service, nil
	}
	if len(service) == 0 {
		return analysis, nil
	}

	merged := make([]gatewayMatch, 0, len(analysis)*len(service))
	for _, a := range analysis {
		for _, s := range service {
			match, err := mergeGatewayMatch(a, s)
			if err != nil {
				return nil, err
			}
			merged = append(merged, match)
		}
	}
	return merged, nil
}

func mergeGatewayMatch(analysis, service gatewayMatch) (gatewayMatch, error) {
	// The service match is the base: it defines the scope of the canary.
	merged := service.copy()

	switch {
	case analysis.Path == nil:
		// keep the service path
	case service.Path == nil:
		merged.Path = analysis.copy().Path
	case service.Path.Type != analysis.Path.Type || service.Path.Value != analysis.Path.Value:
		return gatewayMatch{}, fmt.Errorf(
			"%w: analysis path match %s %q cannot be satisfied together with service path match %s %q",
			ErrConflictingMatch, analysis.Path.Type, analysis.Path.Value, service.Path.Type, service.Path.Value)
	}

	switch {
	case analysis.Method == "":
		// keep the service method
	case service.Method == "":
		merged.Method = analysis.Method
	case service.Method != analysis.Method:
		return gatewayMatch{}, fmt.Errorf(
			"%w: analysis method match %q cannot be satisfied together with service method match %q",
			ErrConflictingMatch, analysis.Method, service.Method)
	}

	headers, err := unionGatewayHeaders(service.Headers, analysis.Headers)
	if err != nil {
		return gatewayMatch{}, err
	}
	merged.Headers = headers

	queryParams, err := unionGatewayQueryParams(service.QueryParams, analysis.QueryParams)
	if err != nil {
		return gatewayMatch{}, err
	}
	merged.QueryParams = queryParams

	return merged, nil
}

func unionGatewayHeaders(base, extra []gatewayHeaderMatch) ([]gatewayHeaderMatch, error) {
	if len(extra) == 0 {
		return base, nil
	}
	union := append([]gatewayHeaderMatch(nil), base...)
	for _, candidate := range extra {
		found := false
		for _, existing := range union {
			if !strings.EqualFold(existing.Name, candidate.Name) {
				continue
			}
			found = true
			if existing.Type != candidate.Type || existing.Value != candidate.Value {
				return nil, fmt.Errorf(
					"%w: header %q is required to be both %s %q and %s %q",
					ErrConflictingMatch, headerConflictName(existing.Name, candidate.Name),
					existing.Type, existing.Value, candidate.Type, candidate.Value)
			}
			break
		}
		if !found {
			union = append(union, candidate)
		}
	}
	sortGatewayHeaders(union)
	return union, nil
}

func unionGatewayQueryParams(base, extra []gatewayQueryMatch) ([]gatewayQueryMatch, error) {
	if len(extra) == 0 {
		return base, nil
	}
	union := append([]gatewayQueryMatch(nil), base...)
	for _, candidate := range extra {
		found := false
		for _, existing := range union {
			if existing.Name != candidate.Name {
				continue
			}
			found = true
			if existing.Type != candidate.Type || existing.Value != candidate.Value {
				return nil, fmt.Errorf(
					"%w: query parameter %q is required to be both %s %q and %s %q",
					ErrConflictingMatch, candidate.Name,
					existing.Type, existing.Value, candidate.Type, candidate.Value)
			}
			break
		}
		if !found {
			union = append(union, candidate)
		}
	}
	sortGatewayQueryParams(union)
	return union, nil
}

// sessionAffinityCookieMatch builds the match condition for a session affinity
// cookie. statusCookie is the "name=value" pair stored in the Canary status.
func sessionAffinityCookieMatch(statusCookie string) (gatewayMatch, error) {
	name, value, ok := strings.Cut(statusCookie, "=")
	if !ok || name == "" {
		return gatewayMatch{}, fmt.Errorf("invalid session affinity cookie in status: %q", statusCookie)
	}
	return gatewayMatch{
		Headers: []gatewayHeaderMatch{
			{
				Name:  cookieHeader,
				Type:  gatewayMatchRegex,
				Value: sessionAffinityCookieRegex(name, value),
			},
		},
	}, nil
}

// sessionAffinityCookieRegex returns a regular expression that matches a Cookie
// header (a semicolon separated list of name=value pairs) containing the exact
// pair name=value. The name and the value are escaped and must appear as a
// complete pair, so that e.g. `xcanary=abc` does not match `canary=abc` and
// `canary=abcd` does not match `canary=abc`.
//
// The expression is wrapped in `.*` and uses only constructs available in RE2,
// PCRE and POSIX ERE, so it works both with regular expressions that must match
// the whole header value (e.g. Envoy's RE2 based matchers) and with
// implementations that search within the value:
//
//	.*(^|;)[[:space:]]*<name>=<value>[[:space:]]*(;|$).*
func sessionAffinityCookieRegex(name, value string) string {
	return fmt.Sprintf(".*(^|;)[[:space:]]*%s=%s[[:space:]]*(;|$).*",
		regexp.QuoteMeta(name), regexp.QuoteMeta(value))
}

// sessionAffinityCookieRegexes returns the set of Cookie header regular
// expressions that the router itself generates for the cookies recorded in the
// Canary status. It is used to recognise the sticky routing rules that are
// maintained by SetRoutes rather than by the spec.
func sessionAffinityCookieRegexes(canary *flaggerv1.Canary) map[string]bool {
	if canary.Spec.Analysis.SessionAffinity == nil {
		return nil
	}
	regexes := map[string]bool{}
	cookies := []string{
		canary.Status.SessionAffinityCookie,
		canary.Status.PrimarySessionAffinityCookie,
		canary.Status.PreviousSessionAffinityCookie,
	}
	for _, cookie := range cookies {
		name, value, ok := strings.Cut(cookie, "=")
		if !ok || name == "" {
			continue
		}
		regexes[sessionAffinityCookieRegex(name, value)] = true
	}
	return regexes
}

// toV1RouteMatches maps the version-agnostic matches to gatewayapi/v1 types.
func toV1RouteMatches(matches []gatewayMatch) []v1.HTTPRouteMatch {
	if len(matches) == 0 {
		return nil
	}
	routeMatches := make([]v1.HTTPRouteMatch, 0, len(matches))
	for _, match := range matches {
		routeMatch := v1.HTTPRouteMatch{}

		if match.Path != nil {
			pathType := v1.PathMatchType(match.Path.Type)
			pathValue := match.Path.Value
			routeMatch.Path = &v1.HTTPPathMatch{Type: &pathType, Value: &pathValue}
		}
		if match.Method != "" {
			method := v1.HTTPMethod(match.Method)
			routeMatch.Method = &method
		}
		for _, header := range match.Headers {
			headerType := v1.HeaderMatchType(header.Type)
			routeMatch.Headers = append(routeMatch.Headers, v1.HTTPHeaderMatch{
				Type:  &headerType,
				Name:  v1.HTTPHeaderName(header.Name),
				Value: header.Value,
			})
		}
		for _, param := range match.QueryParams {
			paramType := v1.QueryParamMatchType(param.Type)
			routeMatch.QueryParams = append(routeMatch.QueryParams, v1.HTTPQueryParamMatch{
				Type:  &paramType,
				Name:  v1.HTTPHeaderName(param.Name),
				Value: param.Value,
			})
		}

		routeMatches = append(routeMatches, routeMatch)
	}
	return routeMatches
}

// toV1Beta1RouteMatches maps the version-agnostic matches to
// gatewayapi/v1beta1 types.
func toV1Beta1RouteMatches(matches []gatewayMatch) []v1beta1.HTTPRouteMatch {
	if len(matches) == 0 {
		return nil
	}
	routeMatches := make([]v1beta1.HTTPRouteMatch, 0, len(matches))
	for _, match := range matches {
		routeMatch := v1beta1.HTTPRouteMatch{}

		if match.Path != nil {
			pathType := v1beta1.PathMatchType(match.Path.Type)
			pathValue := match.Path.Value
			routeMatch.Path = &v1beta1.HTTPPathMatch{Type: &pathType, Value: &pathValue}
		}
		if match.Method != "" {
			method := v1beta1.HTTPMethod(match.Method)
			routeMatch.Method = &method
		}
		for _, header := range match.Headers {
			headerType := v1beta1.HeaderMatchType(header.Type)
			routeMatch.Headers = append(routeMatch.Headers, v1beta1.HTTPHeaderMatch{
				Type:  &headerType,
				Name:  v1beta1.HTTPHeaderName(header.Name),
				Value: header.Value,
			})
		}
		for _, param := range match.QueryParams {
			paramType := v1beta1.QueryParamMatchType(param.Type)
			routeMatch.QueryParams = append(routeMatch.QueryParams, v1beta1.HTTPQueryParamMatch{
				Type:  &paramType,
				Name:  param.Name,
				Value: param.Value,
			})
		}

		routeMatches = append(routeMatches, routeMatch)
	}
	return routeMatches
}

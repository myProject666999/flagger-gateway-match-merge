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
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	istioapiv1alpha1 "github.com/fluxcd/flagger/pkg/apis/istio/common/v1alpha1"
	istiov1beta1 "github.com/fluxcd/flagger/pkg/apis/istio/v1beta1"
)

func exactMatch(value string) istioapiv1alpha1.StringMatch {
	return istioapiv1alpha1.StringMatch{Exact: value}
}

func regexMatch(value string) istioapiv1alpha1.StringMatch {
	return istioapiv1alpha1.StringMatch{Regex: value}
}

func headerMatchHeader(name, value string) gatewayHeaderMatch {
	return gatewayHeaderMatch{Name: name, Value: value, Type: gatewayMatchExact}
}

func TestTranslateRouteMatches(t *testing.T) {
	tests := []struct {
		name        string
		request     []istiov1beta1.HTTPMatchRequest
		want        []gatewayMatch
		wantErr     bool
		errContains []string
	}{
		{
			name: "exact path, method, headers and query params",
			request: []istiov1beta1.HTTPMatchRequest{{
				Uri:         &istioapiv1alpha1.StringMatch{Exact: "/api"},
				Method:      &istioapiv1alpha1.StringMatch{Exact: "GET"},
				Headers:     map[string]istioapiv1alpha1.StringMatch{"X-Tenant": exactMatch("acme")},
				QueryParams: map[string]istioapiv1alpha1.StringMatch{"debug": exactMatch("1")},
			}},
			want: []gatewayMatch{{
				Path:        &gatewayPathMatch{Type: pathMatchExactType, Value: "/api"},
				Method:      "GET",
				Headers:     []gatewayHeaderMatch{headerMatchHeader("X-Tenant", "acme")},
				QueryParams: []gatewayQueryMatch{{Name: "debug", Value: "1", Type: gatewayMatchExact}},
			}},
		},
		{
			name: "prefix path",
			request: []istiov1beta1.HTTPMatchRequest{{
				Uri: &istioapiv1alpha1.StringMatch{Prefix: "/v2"},
			}},
			want: []gatewayMatch{{
				Path: &gatewayPathMatch{Type: pathMatchPrefixType, Value: "/v2"},
			}},
		},
		{
			name: "regex path",
			request: []istiov1beta1.HTTPMatchRequest{{
				Uri: &istioapiv1alpha1.StringMatch{Regex: "/v[0-9]+"},
			}},
			want: []gatewayMatch{{
				Path: &gatewayPathMatch{Type: pathMatchRegexType, Value: "/v[0-9]+"},
			}},
		},
		{
			name: "headers are sorted by name",
			request: []istiov1beta1.HTTPMatchRequest{{
				Headers: map[string]istioapiv1alpha1.StringMatch{
					"x-zeta":   exactMatch("1"),
					"x-alpha":  exactMatch("2"),
					"X-Middle": exactMatch("3"),
				},
			}},
			want: []gatewayMatch{{
				Headers: []gatewayHeaderMatch{
					headerMatchHeader("x-alpha", "2"),
					headerMatchHeader("X-Middle", "3"),
					headerMatchHeader("x-zeta", "1"),
				},
			}},
		},
		{
			name: "query params are sorted by name",
			request: []istiov1beta1.HTTPMatchRequest{{
				QueryParams: map[string]istioapiv1alpha1.StringMatch{
					"z": exactMatch("1"),
					"a": regexMatch("2"),
				},
			}},
			want: []gatewayMatch{{
				QueryParams: []gatewayQueryMatch{
					{Name: "a", Value: "2", Type: gatewayMatchRegex},
					{Name: "z", Value: "1", Type: gatewayMatchExact},
				},
			}},
		},
		{
			name: "header name duplicates differing only in case must agree",
			request: []istiov1beta1.HTTPMatchRequest{{
				Headers: map[string]istioapiv1alpha1.StringMatch{
					"x-tenant": exactMatch("acme"),
					"X-Tenant": exactMatch("acme"),
				},
			}},
			want: []gatewayMatch{{
				Headers: []gatewayHeaderMatch{headerMatchHeader("X-Tenant", "acme")},
			}},
		},
		{
			name: "header name duplicates differing only in case with different values conflict",
			request: []istiov1beta1.HTTPMatchRequest{{
				Headers: map[string]istioapiv1alpha1.StringMatch{
					"x-tenant": exactMatch("acme"),
					"X-Tenant": exactMatch("other"),
				},
			}},
			wantErr:     true,
			errContains: []string{"x-tenant", "X-Tenant", "acme", "other"},
		},
		{
			name: "unsupported method match returns an error naming the method",
			request: []istiov1beta1.HTTPMatchRequest{{
				Method: &istioapiv1alpha1.StringMatch{Prefix: "GE"},
			}},
			wantErr:     true,
			errContains: []string{"method matching selector"},
		},
		{
			name: "unsupported header prefix match returns an error",
			request: []istiov1beta1.HTTPMatchRequest{{
				Headers: map[string]istioapiv1alpha1.StringMatch{"x-tenant": {Prefix: "ac"}},
			}},
			wantErr:     true,
			errContains: []string{"header matching selector", "x-tenant"},
		},
		{
			name: "unsupported empty header match returns an error",
			request: []istiov1beta1.HTTPMatchRequest{{
				Headers: map[string]istioapiv1alpha1.StringMatch{"x-tenant": {}},
			}},
			wantErr:     true,
			errContains: []string{"header matching selector", "x-tenant"},
		},
		{
			name: "unsupported query match returns an error naming the query parameter",
			request: []istiov1beta1.HTTPMatchRequest{{
				QueryParams: map[string]istioapiv1alpha1.StringMatch{"debug": {Suffix: "1"}},
			}},
			wantErr:     true,
			errContains: []string{"query matching selector", "debug"},
		},
		{
			name: "empty request match is ignored",
			request: []istiov1beta1.HTTPMatchRequest{
				{},
				{Headers: map[string]istioapiv1alpha1.StringMatch{"x-tenant": exactMatch("acme")}},
			},
			want: []gatewayMatch{{
				Headers: []gatewayHeaderMatch{headerMatchHeader("x-tenant", "acme")},
			}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := translateRouteMatches(test.request)
			if test.wantErr {
				require.Error(t, err)
				for _, want := range test.errContains {
					assert.Contains(t, err.Error(), want)
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestMergeGatewayMatches(t *testing.T) {
	service := []gatewayMatch{
		{Headers: []gatewayHeaderMatch{headerMatchHeader("x-tenant", "acme")}},
	}

	tests := []struct {
		name        string
		analysis    []gatewayMatch
		service     []gatewayMatch
		want        []gatewayMatch
		wantErr     bool
		errContains []string
	}{
		{
			name:     "no analysis conditions returns the service conditions",
			analysis: nil,
			service:  service,
			want:     service,
		},
		{
			name:     "no service conditions returns the analysis conditions",
			analysis: []gatewayMatch{{Headers: []gatewayHeaderMatch{headerMatchHeader("x-canary", "always")}}},
			service:  nil,
			want:     []gatewayMatch{{Headers: []gatewayHeaderMatch{headerMatchHeader("x-canary", "always")}}},
		},
		{
			name:     "header union keeps both the service and the analysis headers",
			analysis: []gatewayMatch{{Headers: []gatewayHeaderMatch{headerMatchHeader("x-canary", "always")}}},
			service:  service,
			want: []gatewayMatch{{
				Headers: []gatewayHeaderMatch{
					headerMatchHeader("x-canary", "always"),
					headerMatchHeader("x-tenant", "acme"),
				},
			}},
		},
		{
			name:     "query param union",
			analysis: []gatewayMatch{{QueryParams: []gatewayQueryMatch{{Name: "b", Value: "2", Type: gatewayMatchExact}}}},
			service:  []gatewayMatch{{QueryParams: []gatewayQueryMatch{{Name: "a", Value: "1", Type: gatewayMatchExact}}}},
			want: []gatewayMatch{{
				QueryParams: []gatewayQueryMatch{
					{Name: "a", Value: "1", Type: gatewayMatchExact},
					{Name: "b", Value: "2", Type: gatewayMatchExact},
				},
			}},
		},
		{
			name:        "conflicting header values are unsatisfiable",
			analysis:    []gatewayMatch{{Headers: []gatewayHeaderMatch{headerMatchHeader("x-env", "staging")}}},
			service:     []gatewayMatch{{Headers: []gatewayHeaderMatch{headerMatchHeader("x-env", "prod")}}},
			wantErr:     true,
			errContains: []string{"x-env", "staging", "prod"},
		},
		{
			name:        "header conflict is case-insensitive",
			analysis:    []gatewayMatch{{Headers: []gatewayHeaderMatch{headerMatchHeader("X-Env", "staging")}}},
			service:     []gatewayMatch{{Headers: []gatewayHeaderMatch{headerMatchHeader("x-env", "prod")}}},
			wantErr:     true,
			errContains: []string{"X-Env", "staging", "prod"},
		},
		{
			name:     "identical header from both sides collapses into one condition",
			analysis: []gatewayMatch{{Headers: []gatewayHeaderMatch{headerMatchHeader("x-tenant", "acme")}}},
			service:  service,
			want: []gatewayMatch{{
				Headers: []gatewayHeaderMatch{headerMatchHeader("x-tenant", "acme")},
			}},
		},
		{
			name:     "analysis only defines a path prefix, which is carried into the merged match",
			analysis: []gatewayMatch{{Path: &gatewayPathMatch{Type: pathMatchPrefixType, Value: "/beta"}}},
			service:  service,
			want: []gatewayMatch{{
				Path:    &gatewayPathMatch{Type: pathMatchPrefixType, Value: "/beta"},
				Headers: []gatewayHeaderMatch{headerMatchHeader("x-tenant", "acme")},
			}},
		},
		{
			name:     "analysis only defines a method, which is carried into the merged match",
			analysis: []gatewayMatch{{Method: "POST"}},
			service:  service,
			want: []gatewayMatch{{
				Method:  "POST",
				Headers: []gatewayHeaderMatch{headerMatchHeader("x-tenant", "acme")},
			}},
		},
		{
			name: "equal paths are accepted",
			analysis: []gatewayMatch{{
				Path: &gatewayPathMatch{Type: pathMatchPrefixType, Value: "/api"},
			}},
			service: []gatewayMatch{{
				Path: &gatewayPathMatch{Type: pathMatchPrefixType, Value: "/api"},
			}},
			want: []gatewayMatch{{
				Path: &gatewayPathMatch{Type: pathMatchPrefixType, Value: "/api"},
			}},
		},
		{
			name: "conflicting path exact vs prefix is unsatisfiable",
			analysis: []gatewayMatch{{
				Path: &gatewayPathMatch{Type: pathMatchExactType, Value: "/api"},
			}},
			service: []gatewayMatch{{
				Path: &gatewayPathMatch{Type: pathMatchPrefixType, Value: "/api"},
			}},
			wantErr:     true,
			errContains: []string{"path match", "Exact", "PathPrefix", "/api"},
		},
		{
			name:        "conflicting methods are unsatisfiable",
			analysis:    []gatewayMatch{{Method: "GET"}},
			service:     []gatewayMatch{{Method: "POST"}},
			wantErr:     true,
			errContains: []string{"method match", "GET", "POST"},
		},
		{
			name: "cartesian product keeps a stable order",
			analysis: []gatewayMatch{
				{Headers: []gatewayHeaderMatch{headerMatchHeader("x-canary", "a")}},
				{Headers: []gatewayHeaderMatch{headerMatchHeader("x-canary", "b")}},
			},
			service: []gatewayMatch{
				{Path: &gatewayPathMatch{Type: pathMatchPrefixType, Value: "/one"}},
				{Path: &gatewayPathMatch{Type: pathMatchPrefixType, Value: "/two"}},
			},
			want: []gatewayMatch{
				{
					Path:    &gatewayPathMatch{Type: pathMatchPrefixType, Value: "/one"},
					Headers: []gatewayHeaderMatch{headerMatchHeader("x-canary", "a")},
				},
				{
					Path:    &gatewayPathMatch{Type: pathMatchPrefixType, Value: "/two"},
					Headers: []gatewayHeaderMatch{headerMatchHeader("x-canary", "a")},
				},
				{
					Path:    &gatewayPathMatch{Type: pathMatchPrefixType, Value: "/one"},
					Headers: []gatewayHeaderMatch{headerMatchHeader("x-canary", "b")},
				},
				{
					Path:    &gatewayPathMatch{Type: pathMatchPrefixType, Value: "/two"},
					Headers: []gatewayHeaderMatch{headerMatchHeader("x-canary", "b")},
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := mergeGatewayMatches(test.analysis, test.service)
			if test.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, ErrConflictingMatch)
				for _, want := range test.errContains {
					assert.Contains(t, err.Error(), want)
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.want, got)

			// Merging must be deterministic: running it again yields the same result.
			again, err := mergeGatewayMatches(test.analysis, test.service)
			require.NoError(t, err)
			assert.Equal(t, got, again)
		})
	}
}

func TestSessionAffinityCookieRegex(t *testing.T) {
	tests := []struct {
		name       string
		cookieName string
		cookieVal  string
		header     string
		want       bool
	}{
		{
			name:       "cookie at the start of the header",
			cookieName: "canary",
			cookieVal:  "abc",
			header:     "canary=abc",
			want:       true,
		},
		{
			name:       "cookie at the start followed by other cookies",
			cookieName: "canary",
			cookieVal:  "abc",
			header:     "canary=abc; other=1",
			want:       true,
		},
		{
			name:       "cookie in the middle",
			cookieName: "canary",
			cookieVal:  "abc",
			header:     "first=1; canary=abc; last=2",
			want:       true,
		},
		{
			name:       "cookie at the end",
			cookieName: "canary",
			cookieVal:  "abc",
			header:     "first=1; canary=abc",
			want:       true,
		},
		{
			name:       "cookie at the end with trailing semicolon",
			cookieName: "canary",
			cookieVal:  "abc",
			header:     "first=1; canary=abc;",
			want:       true,
		},
		{
			name:       "cookie with surrounding whitespace",
			cookieName: "canary",
			cookieVal:  "abc",
			header:     "first=1;   canary=abc   ; last=2",
			want:       true,
		},
		{
			name:       "name and value spread over unrelated cookies must not match",
			cookieName: "canary",
			cookieVal:  "abc",
			header:     "xcanary=1; other=abc",
			want:       false,
		},
		{
			name:       "value that merely starts with the cookie value must not match",
			cookieName: "canary",
			cookieVal:  "abc",
			header:     "canary=xyz; note=abcdef",
			want:       false,
		},
		{
			name:       "value that extends the cookie value must not match",
			cookieName: "canary",
			cookieVal:  "abc",
			header:     "canary=abcd",
			want:       false,
		},
		{
			name:       "cookie name appearing as a value must not match",
			cookieName: "canary",
			cookieVal:  "abc",
			header:     "note=canary=abc",
			want:       false,
		},
		{
			name:       "cookie name with regex metacharacters is escaped",
			cookieName: "canary.test",
			cookieVal:  "abc",
			header:     "canaryXtest=abc",
			want:       false,
		},
		{
			name:       "cookie name with regex metacharacters matches itself",
			cookieName: "canary.test",
			cookieVal:  "abc",
			header:     "other=1; canary.test=abc",
			want:       true,
		},
		{
			name:       "cookie value containing an equals sign matches as a whole",
			cookieName: "canary",
			cookieVal:  "a=b",
			header:     "other=1; canary=a=b",
			want:       true,
		},
		{
			name:       "cookie value containing an equals sign does not match its prefix",
			cookieName: "canary",
			cookieVal:  "a=b",
			header:     "canary=a",
			want:       false,
		},
		{
			name:       "cookie value with regex metacharacters is escaped",
			cookieName: "canary",
			cookieVal:  "a+b",
			header:     "canary=aab",
			want:       false,
		},
		{
			name:       "cookie value with regex metacharacters matches itself",
			cookieName: "canary",
			cookieVal:  "a+b",
			header:     "canary=a+b",
			want:       true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			re, err := regexp.Compile(sessionAffinityCookieRegex(test.cookieName, test.cookieVal))
			require.NoError(t, err, "the generated expression must compile")
			assert.Equal(t, test.want, re.MatchString(test.header))
		})
	}
}

func TestSessionAffinityCookieMatch(t *testing.T) {
	match, err := sessionAffinityCookieMatch("canary=abc")
	require.NoError(t, err)
	require.Len(t, match.Headers, 1)
	assert.Equal(t, cookieHeader, match.Headers[0].Name)
	assert.Equal(t, gatewayMatchRegex, match.Headers[0].Type)

	_, err = sessionAffinityCookieMatch("")
	require.Error(t, err)
}

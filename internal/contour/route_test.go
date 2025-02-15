// Copyright © 2018 Heptio
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package contour

import (
	"sort"
	"testing"
	"time"

	v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	"github.com/envoyproxy/go-control-plane/envoy/api/v2/route"
	"github.com/gogo/protobuf/proto"
	"github.com/google/go-cmp/cmp"
	ingressroutev1 "github.com/heptio/contour/apis/contour/v1beta1"
	"github.com/heptio/contour/internal/dag"
	"github.com/heptio/contour/internal/envoy"
	"github.com/heptio/contour/internal/metrics"
	"github.com/prometheus/client_golang/prometheus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/api/extensions/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestRouteCacheContents(t *testing.T) {
	tests := map[string]struct {
		contents map[string]*v2.RouteConfiguration
		want     []proto.Message
	}{
		"empty": {
			contents: nil,
			want:     nil,
		},
		"simple": {
			contents: map[string]*v2.RouteConfiguration{
				"ingress_http": {
					Name: "ingress_http",
				},
				"ingress_https": {
					Name: "ingress_https",
				},
			},
			want: []proto.Message{
				&v2.RouteConfiguration{
					Name: "ingress_http",
				},
				&v2.RouteConfiguration{
					Name: "ingress_https",
				},
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var rc RouteCache
			rc.Update(tc.contents)
			got := rc.Contents()
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestRouteCacheQuery(t *testing.T) {
	tests := map[string]struct {
		contents map[string]*v2.RouteConfiguration
		query    []string
		want     []proto.Message
	}{
		"exact match": {
			contents: map[string]*v2.RouteConfiguration{
				"ingress_http": {
					Name: "ingress_http",
				},
			},
			query: []string{"ingress_http"},
			want: []proto.Message{
				&v2.RouteConfiguration{
					Name: "ingress_http",
				},
			},
		},
		"partial match": {
			contents: map[string]*v2.RouteConfiguration{
				"ingress_http": {
					Name: "ingress_http",
				},
			},
			query: []string{"stats-handler", "ingress_http"},
			want: []proto.Message{
				&v2.RouteConfiguration{
					Name: "ingress_http",
				},
				&v2.RouteConfiguration{
					Name: "stats-handler",
				},
			},
		},
		"no match": {
			contents: map[string]*v2.RouteConfiguration{
				"ingress_http": {
					Name: "ingress_http",
				},
			},
			query: []string{"stats-handler"},
			want: []proto.Message{
				&v2.RouteConfiguration{
					Name: "stats-handler",
				},
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var rc RouteCache
			rc.Update(tc.contents)
			got := rc.Query(tc.query)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestRouteVisit(t *testing.T) {
	tests := map[string]struct {
		objs []interface{}
		want map[string]*v2.RouteConfiguration
	}{
		"nothing": {
			objs: nil,
			want: map[string]*v2.RouteConfiguration{
				"ingress_http": {
					Name: "ingress_http",
				},
				"ingress_https": {
					Name: "ingress_https",
				},
			},
		},
		"one http only ingress with service": {
			objs: []interface{}{
				&v1beta1.Ingress{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kuard",
						Namespace: "default",
					},
					Spec: v1beta1.IngressSpec{
						Backend: &v1beta1.IngressBackend{
							ServiceName: "kuard",
							ServicePort: intstr.FromInt(8080),
						},
					},
				},
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kuard",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Protocol:   "TCP",
							Port:       8080,
							TargetPort: intstr.FromInt(8080),
						}},
					},
				},
			},
			want: map[string]*v2.RouteConfiguration{
				"ingress_http": {
					Name: "ingress_http",
					VirtualHosts: []route.VirtualHost{{
						Name:    "*",
						Domains: []string{"*"},
						Routes: []route.Route{{
							Match:               envoy.RoutePrefix("/"),
							Action:              routecluster("default/kuard/8080/da39a3ee5e"),
							RequestHeadersToAdd: envoy.RouteHeaders(),
						}},
					}},
				},
				"ingress_https": {
					Name: "ingress_https",
				},
			},
		},
		"one http only ingress with regex match": {
			objs: []interface{}{
				&v1beta1.Ingress{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kuard",
						Namespace: "default",
					},
					Spec: v1beta1.IngressSpec{
						Rules: []v1beta1.IngressRule{{
							IngressRuleValue: v1beta1.IngressRuleValue{
								HTTP: &v1beta1.HTTPIngressRuleValue{
									Paths: []v1beta1.HTTPIngressPath{{
										Path: "/[^/]+/invoices(/.*|/?)", // issue 1243
										Backend: v1beta1.IngressBackend{
											ServiceName: "kuard",
											ServicePort: intstr.FromInt(8080),
										},
									}},
								},
							},
						}},
					},
				},
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kuard",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Protocol:   "TCP",
							Port:       8080,
							TargetPort: intstr.FromInt(8080),
						}},
					},
				},
			},
			want: map[string]*v2.RouteConfiguration{
				"ingress_http": {
					Name: "ingress_http",
					VirtualHosts: []route.VirtualHost{{
						Name:    "*",
						Domains: []string{"*"},
						Routes: []route.Route{{
							Match:               envoy.RouteRegex("/[^/]+/invoices(/.*|/?)"),
							Action:              routecluster("default/kuard/8080/da39a3ee5e"),
							RequestHeadersToAdd: envoy.RouteHeaders(),
						}},
					}},
				},
				"ingress_https": {
					Name: "ingress_https",
				},
			},
		},

		"one http only ingressroute": {
			objs: []interface{}{
				&ingressroutev1.IngressRoute{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "simple",
						Namespace: "default",
					},
					Spec: ingressroutev1.IngressRouteSpec{
						VirtualHost: &ingressroutev1.VirtualHost{
							Fqdn: "www.example.com",
						},
						Routes: []ingressroutev1.Route{{
							Match: "/",
							Services: []ingressroutev1.Service{
								{
									Name: "backend",
									Port: 80,
								},
							},
						}},
					},
				},
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "backend",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Protocol:   "TCP",
							Port:       80,
							TargetPort: intstr.FromInt(8080),
						}},
					},
				},
			},
			want: map[string]*v2.RouteConfiguration{
				"ingress_http": {
					Name: "ingress_http",
					VirtualHosts: []route.VirtualHost{{
						Name:    "www.example.com",
						Domains: domains("www.example.com"),
						Routes: []route.Route{{
							Match:               envoy.RoutePrefix("/"),
							Action:              routecluster("default/backend/80/da39a3ee5e"),
							RequestHeadersToAdd: envoy.RouteHeaders(),
						}},
					}},
				},
				"ingress_https": {
					Name: "ingress_https",
				},
			},
		},
		"default backend ingress with secret": {
			objs: []interface{}{
				&v1beta1.Ingress{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "simple",
						Namespace: "default",
					},
					Spec: v1beta1.IngressSpec{
						TLS: []v1beta1.IngressTLS{{
							Hosts:      []string{"whatever.example.com"},
							SecretName: "secret",
						}},
						Backend: &v1beta1.IngressBackend{
							ServiceName: "kuard",
							ServicePort: intstr.FromInt(8080),
						},
					},
				},
				&v1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "secret",
						Namespace: "default",
					},
					Type: "kubernetes.io/tls",
					Data: secretdata("certificate", "key"),
				},
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kuard",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Protocol:   "TCP",
							Port:       8080,
							TargetPort: intstr.FromInt(8080),
						}},
					},
				},
			},
			want: map[string]*v2.RouteConfiguration{
				"ingress_http": {
					Name: "ingress_http",
					VirtualHosts: []route.VirtualHost{{
						Name:    "*", // default backend
						Domains: []string{"*"},
						Routes: []route.Route{{
							Match:               envoy.RoutePrefix("/"),
							Action:              routecluster("default/kuard/8080/da39a3ee5e"),
							RequestHeadersToAdd: envoy.RouteHeaders(),
						}},
					}},
				},
				"ingress_https": {
					Name: "ingress_https", // no https for default backend
				},
			},
		},
		"vhost ingress with secret": {
			objs: []interface{}{
				&v1beta1.Ingress{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "simple",
						Namespace: "default",
					},
					Spec: v1beta1.IngressSpec{
						TLS: []v1beta1.IngressTLS{{
							Hosts:      []string{"www.example.com"},
							SecretName: "secret",
						}},
						Rules: []v1beta1.IngressRule{{
							Host: "www.example.com",
							IngressRuleValue: v1beta1.IngressRuleValue{
								HTTP: &v1beta1.HTTPIngressRuleValue{
									Paths: []v1beta1.HTTPIngressPath{{
										Backend: v1beta1.IngressBackend{
											ServiceName: "kuard",
											ServicePort: intstr.FromString("www"),
										},
									}},
								},
							},
						}},
					},
				},
				&v1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "secret",
						Namespace: "default",
					},
					Type: "kubernetes.io/tls",
					Data: secretdata("certificate", "key"),
				},
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kuard",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Name:       "www",
							Protocol:   "TCP",
							Port:       8080,
							TargetPort: intstr.FromInt(8080),
						}},
					},
				},
			},
			want: map[string]*v2.RouteConfiguration{
				"ingress_http": {
					Name: "ingress_http",
					VirtualHosts: []route.VirtualHost{{
						Name:    "www.example.com",
						Domains: domains("www.example.com"),
						Routes: []route.Route{{
							Match:               envoy.RoutePrefix("/"),
							Action:              routecluster("default/kuard/8080/da39a3ee5e"),
							RequestHeadersToAdd: envoy.RouteHeaders(),
						}},
					}},
				},
				"ingress_https": {
					Name: "ingress_https",
					VirtualHosts: []route.VirtualHost{{
						Name:    "www.example.com",
						Domains: domains("www.example.com"),
						Routes: []route.Route{{
							Match:               envoy.RoutePrefix("/"),
							Action:              routecluster("default/kuard/8080/da39a3ee5e"),
							RequestHeadersToAdd: envoy.RouteHeaders(),
						}},
					}},
				},
			},
		},
		"simple ingressroute with secret": {
			objs: []interface{}{
				&ingressroutev1.IngressRoute{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "simple",
						Namespace: "default",
					},
					Spec: ingressroutev1.IngressRouteSpec{
						VirtualHost: &ingressroutev1.VirtualHost{
							Fqdn: "www.example.com",
							TLS: &ingressroutev1.TLS{
								SecretName: "secret",
							},
						},
						Routes: []ingressroutev1.Route{{
							Match: "/",
							Services: []ingressroutev1.Service{{
								Name: "backend",
								Port: 8080,
							},
							}},
						},
					},
				},
				&v1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "secret",
						Namespace: "default",
					},
					Type: "kubernetes.io/tls",
					Data: secretdata("certificate", "key"),
				},
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "backend",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Name:       "www",
							Protocol:   "TCP",
							Port:       8080,
							TargetPort: intstr.FromInt(8080),
						}},
					},
				},
			},
			want: map[string]*v2.RouteConfiguration{
				"ingress_http": {
					Name: "ingress_http",
					VirtualHosts: []route.VirtualHost{{
						Name:    "www.example.com",
						Domains: domains("www.example.com"),
						Routes: []route.Route{{
							Match: envoy.RoutePrefix("/"),
							Action: &route.Route_Redirect{
								Redirect: &route.RedirectAction{
									SchemeRewriteSpecifier: &route.RedirectAction_HttpsRedirect{
										HttpsRedirect: true,
									},
								},
							},
						}},
					}},
				},
				"ingress_https": {
					Name: "ingress_https",
					VirtualHosts: []route.VirtualHost{{
						Name:    "www.example.com",
						Domains: domains("www.example.com"),
						Routes: []route.Route{{
							Match:               envoy.RoutePrefix("/"),
							Action:              routecluster("default/backend/8080/da39a3ee5e"),
							RequestHeadersToAdd: envoy.RouteHeaders(),
						}},
					}},
				},
			},
		},
		"simple tls ingress with allow-http:false": {
			objs: []interface{}{
				&v1beta1.Ingress{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "simple",
						Namespace: "default",
						Annotations: map[string]string{
							"kubernetes.io/ingress.allow-http": "false",
						},
					},
					Spec: v1beta1.IngressSpec{
						TLS: []v1beta1.IngressTLS{{
							Hosts:      []string{"www.example.com"},
							SecretName: "secret",
						}},
						Rules: []v1beta1.IngressRule{{
							Host: "www.example.com",
							IngressRuleValue: v1beta1.IngressRuleValue{
								HTTP: &v1beta1.HTTPIngressRuleValue{
									Paths: []v1beta1.HTTPIngressPath{{
										Backend: v1beta1.IngressBackend{
											ServiceName: "kuard",
											ServicePort: intstr.FromString("www"),
										},
									}},
								},
							},
						}},
					},
				},
				&v1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "secret",
						Namespace: "default",
					},
					Type: "kubernetes.io/tls",
					Data: secretdata("certificate", "key"),
				},
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kuard",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Name:       "www",
							Protocol:   "TCP",
							Port:       8080,
							TargetPort: intstr.FromInt(8080),
						}},
					},
				},
			},
			want: map[string]*v2.RouteConfiguration{
				"ingress_http": {
					Name: "ingress_http",
				},
				"ingress_https": {
					Name: "ingress_https",
					VirtualHosts: []route.VirtualHost{{
						Name:    "www.example.com",
						Domains: domains("www.example.com"),
						Routes: []route.Route{{
							Match:               envoy.RoutePrefix("/"),
							Action:              routecluster("default/kuard/8080/da39a3ee5e"),
							RequestHeadersToAdd: envoy.RouteHeaders(),
						}},
					}},
				},
			},
		},
		"simple tls ingress with force-ssl-redirect": {
			objs: []interface{}{
				&v1beta1.Ingress{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "simple",
						Namespace: "default",
						Annotations: map[string]string{
							"ingress.kubernetes.io/force-ssl-redirect": "true",
						},
					},
					Spec: v1beta1.IngressSpec{
						TLS: []v1beta1.IngressTLS{{
							Hosts:      []string{"www.example.com"},
							SecretName: "secret",
						}},
						Rules: []v1beta1.IngressRule{{
							Host: "www.example.com",
							IngressRuleValue: v1beta1.IngressRuleValue{
								HTTP: &v1beta1.HTTPIngressRuleValue{
									Paths: []v1beta1.HTTPIngressPath{{
										Backend: v1beta1.IngressBackend{
											ServiceName: "kuard",
											ServicePort: intstr.FromString("www"),
										},
									}},
								},
							},
						}},
					},
				},
				&v1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "secret",
						Namespace: "default",
					},
					Type: "kubernetes.io/tls",
					Data: secretdata("certificate", "key"),
				},
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kuard",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Name:       "www",
							Protocol:   "TCP",
							Port:       8080,
							TargetPort: intstr.FromInt(8080),
						}},
					},
				},
			},
			want: map[string]*v2.RouteConfiguration{
				"ingress_http": {
					Name: "ingress_http",
					VirtualHosts: []route.VirtualHost{{
						Name:    "www.example.com",
						Domains: domains("www.example.com"),
						Routes: []route.Route{{
							Match: envoy.RoutePrefix("/"),
							Action: &route.Route_Redirect{
								Redirect: &route.RedirectAction{
									SchemeRewriteSpecifier: &route.RedirectAction_HttpsRedirect{
										HttpsRedirect: true,
									},
								},
							},
						}},
					}},
				},
				"ingress_https": {
					Name: "ingress_https",
					VirtualHosts: []route.VirtualHost{{
						Name:    "www.example.com",
						Domains: domains("www.example.com"),
						Routes: []route.Route{{
							Match:               envoy.RoutePrefix("/"),
							Action:              routecluster("default/kuard/8080/da39a3ee5e"),
							RequestHeadersToAdd: envoy.RouteHeaders(),
						}},
					}},
				},
			},
		},
		"ingress with websocket annotation": {
			objs: []interface{}{
				&v1beta1.Ingress{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "simple",
						Namespace: "default",
						Annotations: map[string]string{
							"contour.heptio.com/websocket-routes": "/ws1 , /ws2",
						},
					},
					Spec: v1beta1.IngressSpec{
						Rules: []v1beta1.IngressRule{{
							Host: "www.example.com",
							IngressRuleValue: v1beta1.IngressRuleValue{
								HTTP: &v1beta1.HTTPIngressRuleValue{
									Paths: []v1beta1.HTTPIngressPath{{
										Path: "/",
										Backend: v1beta1.IngressBackend{
											ServiceName: "kuard",
											ServicePort: intstr.FromString("www"),
										},
									}, {
										Path: "/ws1",
										Backend: v1beta1.IngressBackend{
											ServiceName: "kuard",
											ServicePort: intstr.FromString("www"),
										},
									}},
								},
							},
						}},
					},
				},
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kuard",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Name:       "www",
							Protocol:   "TCP",
							Port:       8080,
							TargetPort: intstr.FromInt(8080),
						}},
					},
				},
			},
			want: map[string]*v2.RouteConfiguration{
				"ingress_http": {
					Name: "ingress_http",
					VirtualHosts: []route.VirtualHost{{
						Name:    "www.example.com",
						Domains: domains("www.example.com"),
						Routes: []route.Route{{
							Match:               envoy.RoutePrefix("/ws1"),
							Action:              websocketroute("default/kuard/8080/da39a3ee5e"),
							RequestHeadersToAdd: envoy.RouteHeaders(),
						}, {
							Match:               envoy.RoutePrefix("/"),
							Action:              routecluster("default/kuard/8080/da39a3ee5e"),
							RequestHeadersToAdd: envoy.RouteHeaders(),
						}},
					}},
				},
				"ingress_https": {
					Name: "ingress_https",
				},
			},
		},
		"ingress invalid timeout": {
			objs: []interface{}{
				&v1beta1.Ingress{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kuard",
						Namespace: "default",
						Annotations: map[string]string{
							"contour.heptio.com/request-timeout": "heptio",
						},
					},
					Spec: v1beta1.IngressSpec{
						Backend: &v1beta1.IngressBackend{
							ServiceName: "kuard",
							ServicePort: intstr.FromInt(8080),
						},
					},
				},
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kuard",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Protocol:   "TCP",
							Port:       8080,
							TargetPort: intstr.FromInt(8080),
						}},
					},
				},
			},
			want: map[string]*v2.RouteConfiguration{
				"ingress_http": {
					Name: "ingress_http",
					VirtualHosts: []route.VirtualHost{{
						Name:    "*",
						Domains: []string{"*"},
						Routes: []route.Route{{
							Match:               envoy.RoutePrefix("/"),
							Action:              routetimeout("default/kuard/8080/da39a3ee5e", duration(0)),
							RequestHeadersToAdd: envoy.RouteHeaders(),
						}},
					}},
				},
				"ingress_https": {
					Name: "ingress_https",
				},
			},
		},
		"ingress infinite timeout": {
			objs: []interface{}{
				&v1beta1.Ingress{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kuard",
						Namespace: "default",
						Annotations: map[string]string{
							"contour.heptio.com/request-timeout": "infinity",
						},
					},
					Spec: v1beta1.IngressSpec{
						Backend: &v1beta1.IngressBackend{
							ServiceName: "kuard",
							ServicePort: intstr.FromInt(8080),
						},
					},
				},
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kuard",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Protocol:   "TCP",
							Port:       8080,
							TargetPort: intstr.FromInt(8080),
						}},
					},
				},
			},
			want: map[string]*v2.RouteConfiguration{
				"ingress_http": {
					Name: "ingress_http",
					VirtualHosts: []route.VirtualHost{{
						Name:    "*",
						Domains: []string{"*"},
						Routes: []route.Route{{
							Match:               envoy.RoutePrefix("/"),
							Action:              routetimeout("default/kuard/8080/da39a3ee5e", duration(0)),
							RequestHeadersToAdd: envoy.RouteHeaders(),
						}},
					}},
				},
				"ingress_https": {
					Name: "ingress_https",
				},
			},
		},
		"ingress 90 second timeout": {
			objs: []interface{}{
				&v1beta1.Ingress{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kuard",
						Namespace: "default",
						Annotations: map[string]string{
							"contour.heptio.com/request-timeout": "1m30s",
						},
					},
					Spec: v1beta1.IngressSpec{
						Backend: &v1beta1.IngressBackend{
							ServiceName: "kuard",
							ServicePort: intstr.FromInt(8080),
						},
					},
				},
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kuard",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Protocol:   "TCP",
							Port:       8080,
							TargetPort: intstr.FromInt(8080),
						}},
					},
				},
			},
			want: map[string]*v2.RouteConfiguration{
				"ingress_http": {
					Name: "ingress_http",
					VirtualHosts: []route.VirtualHost{{
						Name:    "*",
						Domains: []string{"*"},
						Routes: []route.Route{{
							Match:               envoy.RoutePrefix("/"),
							Action:              routetimeout("default/kuard/8080/da39a3ee5e", duration(90*time.Second)),
							RequestHeadersToAdd: envoy.RouteHeaders(),
						}},
					}},
				},
				"ingress_https": {
					Name: "ingress_https",
				},
			},
		},
		"vhost name exceeds 60 chars": { // heptio/contour#25
			objs: []interface{}{
				&v1beta1.Ingress{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "my-service-name",
						Namespace: "default",
					},
					Spec: v1beta1.IngressSpec{
						Rules: []v1beta1.IngressRule{{
							Host: "my-very-very-long-service-host-name.subdomain.boring-dept.my.company",
							IngressRuleValue: v1beta1.IngressRuleValue{
								HTTP: &v1beta1.HTTPIngressRuleValue{
									Paths: []v1beta1.HTTPIngressPath{{
										Path: "/",
										Backend: v1beta1.IngressBackend{
											ServiceName: "kuard",
											ServicePort: intstr.FromString("www"),
										},
									}},
								},
							},
						}},
					},
				},
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kuard",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Name:       "www",
							Protocol:   "TCP",
							Port:       80,
							TargetPort: intstr.FromInt(8080),
						}},
					},
				},
			},
			want: map[string]*v2.RouteConfiguration{
				"ingress_http": {
					Name: "ingress_http",
					VirtualHosts: []route.VirtualHost{{
						Name:    "d31bb322ca62bb395acad00b3cbf45a3aa1010ca28dca7cddb4f7db786fa",
						Domains: domains("my-very-very-long-service-host-name.subdomain.boring-dept.my.company"),
						Routes: []route.Route{{
							Match:               envoy.RoutePrefix("/"),
							Action:              routecluster("default/kuard/80/da39a3ee5e"),
							RequestHeadersToAdd: envoy.RouteHeaders(),
						}},
					}},
				},
				"ingress_https": {
					Name: "ingress_https",
				},
			},
		},
		"Ingress: empty ingress class": {
			objs: []interface{}{
				&v1beta1.Ingress{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "incorrect",
						Namespace: "default",
					},
					Spec: v1beta1.IngressSpec{
						Backend: &v1beta1.IngressBackend{
							ServiceName: "kuard",
							ServicePort: intstr.FromInt(8080),
						},
					},
				},
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kuard",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Protocol:   "TCP",
							Port:       8080,
							TargetPort: intstr.FromInt(8080),
						}},
					},
				},
			},
			want: map[string]*v2.RouteConfiguration{
				"ingress_http": {
					Name: "ingress_http",
					VirtualHosts: []route.VirtualHost{{
						Name:    "*",
						Domains: []string{"*"},
						Routes: []route.Route{{
							Match:               envoy.RoutePrefix("/"),
							Action:              routecluster("default/kuard/8080/da39a3ee5e"),
							RequestHeadersToAdd: envoy.RouteHeaders(),
						}},
					}},
				},
				"ingress_https": {
					Name: "ingress_https",
				},
			},
		},
		"Ingress: incorrect kubernetes.io/ingress.class": {
			objs: []interface{}{
				&v1beta1.Ingress{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "incorrect",
						Namespace: "default",
						Annotations: map[string]string{
							"kubernetes.io/ingress.class": "nginx",
						},
					},
					Spec: v1beta1.IngressSpec{
						Backend: &v1beta1.IngressBackend{
							ServiceName: "kuard",
							ServicePort: intstr.FromInt(8080),
						},
					},
				},
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kuard",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Protocol:   "TCP",
							Port:       8080,
							TargetPort: intstr.FromInt(8080),
						}},
					},
				},
			},
			want: map[string]*v2.RouteConfiguration{
				"ingress_http": {
					Name: "ingress_http", // expected to be empty, the ingress class is ignored
				},
				"ingress_https": {
					Name: "ingress_https", // expected to be empty, the ingress class is ignored
				},
			},
		},
		"Ingress: incorrect contour.heptio.com/ingress.class": {
			objs: []interface{}{
				&v1beta1.Ingress{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "incorrect",
						Namespace: "default",
						Annotations: map[string]string{
							"contour.heptio.com/ingress.class": "nginx",
						},
					},
					Spec: v1beta1.IngressSpec{
						Backend: &v1beta1.IngressBackend{
							ServiceName: "kuard",
							ServicePort: intstr.FromInt(8080),
						},
					},
				},
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kuard",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Protocol:   "TCP",
							Port:       8080,
							TargetPort: intstr.FromInt(8080),
						}},
					},
				},
			},
			want: map[string]*v2.RouteConfiguration{
				"ingress_http": {
					Name: "ingress_http", // expected to be empty, the ingress class is ignored
				},
				"ingress_https": {
					Name: "ingress_https", // expected to be empty, the ingress class is ignored
				},
			},
		},
		"Ingress: explicit kubernetes.io/ingress.class": {
			objs: []interface{}{
				&v1beta1.Ingress{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "incorrect",
						Namespace: "default",
						Annotations: map[string]string{
							"kubernetes.io/ingress.class": new(ResourceEventHandler).ingressClass(),
						},
					},
					Spec: v1beta1.IngressSpec{
						Backend: &v1beta1.IngressBackend{
							ServiceName: "kuard",
							ServicePort: intstr.FromInt(8080),
						},
					},
				},
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kuard",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Protocol:   "TCP",
							Port:       8080,
							TargetPort: intstr.FromInt(8080),
						}},
					},
				},
			},
			want: map[string]*v2.RouteConfiguration{
				"ingress_http": {
					Name: "ingress_http",
					VirtualHosts: []route.VirtualHost{{
						Name:    "*",
						Domains: []string{"*"},
						Routes: []route.Route{{
							Match:               envoy.RoutePrefix("/"),
							Action:              routecluster("default/kuard/8080/da39a3ee5e"),
							RequestHeadersToAdd: envoy.RouteHeaders(),
						}},
					}},
				},
				"ingress_https": {
					Name: "ingress_https",
				},
			},
		},
		"Ingress: explicit contour.heptio.com/ingress.class": {
			objs: []interface{}{
				&v1beta1.Ingress{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "incorrect",
						Namespace: "default",
						Annotations: map[string]string{
							"contour.heptio.com/ingress.class": new(ResourceEventHandler).ingressClass(),
						},
					},
					Spec: v1beta1.IngressSpec{
						Backend: &v1beta1.IngressBackend{
							ServiceName: "kuard",
							ServicePort: intstr.FromInt(8080),
						},
					},
				},
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kuard",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Protocol:   "TCP",
							Port:       8080,
							TargetPort: intstr.FromInt(8080),
						}},
					},
				},
			},
			want: map[string]*v2.RouteConfiguration{
				"ingress_http": {
					Name: "ingress_http",
					VirtualHosts: []route.VirtualHost{{
						Name:    "*",
						Domains: []string{"*"},
						Routes: []route.Route{{
							Match:               envoy.RoutePrefix("/"),
							Action:              routecluster("default/kuard/8080/da39a3ee5e"),
							RequestHeadersToAdd: envoy.RouteHeaders(),
						}},
					}},
				},
				"ingress_https": {
					Name: "ingress_https",
				},
			},
		},
		"IngressRoute: empty ingress annotation": {
			objs: []interface{}{
				&ingressroutev1.IngressRoute{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kuard",
						Namespace: "default",
					},
					Spec: ingressroutev1.IngressRouteSpec{
						VirtualHost: &ingressroutev1.VirtualHost{
							Fqdn: "www.example.com",
						},
						Routes: []ingressroutev1.Route{{
							Match: "/",
							Services: []ingressroutev1.Service{
								{
									Name: "kuard",
									Port: 8080,
								},
							},
						}},
					},
				},
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kuard",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Protocol:   "TCP",
							Port:       8080,
							TargetPort: intstr.FromInt(8080),
						}},
					},
				},
			},
			want: map[string]*v2.RouteConfiguration{
				"ingress_http": {
					Name: "ingress_http",
					VirtualHosts: []route.VirtualHost{{
						Name:    "www.example.com",
						Domains: domains("www.example.com"),
						Routes: []route.Route{{
							Match:               envoy.RoutePrefix("/"),
							Action:              routecluster("default/kuard/8080/da39a3ee5e"),
							RequestHeadersToAdd: envoy.RouteHeaders(),
						}},
					}},
				},
				"ingress_https": {
					Name: "ingress_https",
				},
			},
		},
		"IngressRoute: incorrect contour.heptio.com/ingress.class": {
			objs: []interface{}{
				&ingressroutev1.IngressRoute{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "simple",
						Namespace: "default",
						Annotations: map[string]string{
							"contour.heptio.com/ingress.class": "nginx",
						},
					},
					Spec: ingressroutev1.IngressRouteSpec{
						VirtualHost: &ingressroutev1.VirtualHost{
							Fqdn: "www.example.com",
						},
						Routes: []ingressroutev1.Route{{
							Match: "/",
							Services: []ingressroutev1.Service{
								{
									Name: "backend",
									Port: 80,
								},
							},
						}},
					},
				},
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kuard",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Protocol:   "TCP",
							Port:       8080,
							TargetPort: intstr.FromInt(8080),
						}},
					},
				},
			},
			want: map[string]*v2.RouteConfiguration{
				"ingress_http": {
					Name: "ingress_http", // expected to be empty, the ingress class is ignored
				},
				"ingress_https": {
					Name: "ingress_https", // expected to be empty, the ingress class is ignored
				},
			},
		},
		"IngressRoute: incorrect kubernetes.io/ingress.class": {
			objs: []interface{}{
				&ingressroutev1.IngressRoute{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "simple",
						Namespace: "default",
						Annotations: map[string]string{
							"kubernetes.io/ingress.class": "nginx",
						},
					},
					Spec: ingressroutev1.IngressRouteSpec{
						VirtualHost: &ingressroutev1.VirtualHost{
							Fqdn: "www.example.com",
						},
						Routes: []ingressroutev1.Route{{
							Match: "/",
							Services: []ingressroutev1.Service{
								{
									Name: "backend",
									Port: 80,
								},
							},
						}},
					},
				},
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kuard",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Protocol:   "TCP",
							Port:       8080,
							TargetPort: intstr.FromInt(8080),
						}},
					},
				},
			},
			want: map[string]*v2.RouteConfiguration{
				"ingress_http": {
					Name: "ingress_http", // expected to be empty, the ingress class is ignored
				},
				"ingress_https": {
					Name: "ingress_https", // expected to be empty, the ingress class is ignored
				},
			},
		},
		"IngressRoute: explicit contour.heptio.com/ingress.class": {
			objs: []interface{}{
				&ingressroutev1.IngressRoute{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kuard",
						Namespace: "default",
						Annotations: map[string]string{
							"contour.heptio.com/ingress.class": new(ResourceEventHandler).ingressClass(),
						},
					},
					Spec: ingressroutev1.IngressRouteSpec{
						VirtualHost: &ingressroutev1.VirtualHost{
							Fqdn: "www.example.com",
						},
						Routes: []ingressroutev1.Route{{
							Match: "/",
							Services: []ingressroutev1.Service{
								{
									Name: "kuard",
									Port: 8080,
								},
							},
						}},
					},
				},
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kuard",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Protocol:   "TCP",
							Port:       8080,
							TargetPort: intstr.FromInt(8080),
						}},
					},
				},
			},
			want: map[string]*v2.RouteConfiguration{
				"ingress_http": {
					Name: "ingress_http",
					VirtualHosts: []route.VirtualHost{{
						Name:    "www.example.com",
						Domains: domains("www.example.com"),
						Routes: []route.Route{{
							Match:               envoy.RoutePrefix("/"),
							Action:              routecluster("default/kuard/8080/da39a3ee5e"),
							RequestHeadersToAdd: envoy.RouteHeaders(),
						}},
					}},
				},
				"ingress_https": {
					Name: "ingress_https",
				},
			},
		},
		"IngressRoute: explicit kubernetes.io/ingress.class": {
			objs: []interface{}{
				&ingressroutev1.IngressRoute{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kuard",
						Namespace: "default",
						Annotations: map[string]string{
							"kubernetes.io/ingress.class": new(ResourceEventHandler).ingressClass(),
						},
					},
					Spec: ingressroutev1.IngressRouteSpec{
						VirtualHost: &ingressroutev1.VirtualHost{
							Fqdn: "www.example.com",
						},
						Routes: []ingressroutev1.Route{{
							Match: "/",
							Services: []ingressroutev1.Service{
								{
									Name: "kuard",
									Port: 8080,
								},
							},
						}},
					},
				},
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kuard",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Protocol:   "TCP",
							Port:       8080,
							TargetPort: intstr.FromInt(8080),
						}},
					},
				},
			},
			want: map[string]*v2.RouteConfiguration{
				"ingress_http": {
					Name: "ingress_http",
					VirtualHosts: []route.VirtualHost{{
						Name:    "www.example.com",
						Domains: domains("www.example.com"),
						Routes: []route.Route{{
							Match:               envoy.RoutePrefix("/"),
							Action:              routecluster("default/kuard/8080/da39a3ee5e"),
							RequestHeadersToAdd: envoy.RouteHeaders(),
						}},
					}},
				},
				"ingress_https": {
					Name: "ingress_https",
				},
			},
		},
		"ingress retry-on": {
			objs: []interface{}{
				&v1beta1.Ingress{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kuard",
						Namespace: "default",
						Annotations: map[string]string{
							"contour.heptio.com/retry-on": "5xx,gateway-error",
						},
					},
					Spec: v1beta1.IngressSpec{
						Backend: &v1beta1.IngressBackend{
							ServiceName: "kuard",
							ServicePort: intstr.FromInt(8080),
						},
					},
				},
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kuard",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Protocol:   "TCP",
							Port:       8080,
							TargetPort: intstr.FromInt(8080),
						}},
					},
				},
			},
			want: map[string]*v2.RouteConfiguration{
				"ingress_http": {
					Name: "ingress_http",
					VirtualHosts: []route.VirtualHost{{
						Name:    "*",
						Domains: []string{"*"},
						Routes: []route.Route{{
							Match:               envoy.RoutePrefix("/"),
							Action:              routeretry("default/kuard/8080/da39a3ee5e", "5xx,gateway-error", 0, 0),
							RequestHeadersToAdd: envoy.RouteHeaders(),
						}},
					}},
				},
				"ingress_https": {
					Name: "ingress_https",
				},
			},
		},
		"ingress retry-on, num-retries": {
			objs: []interface{}{
				&v1beta1.Ingress{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kuard",
						Namespace: "default",
						Annotations: map[string]string{
							"contour.heptio.com/retry-on":    "5xx,gateway-error",
							"contour.heptio.com/num-retries": "7", // not five or six or eight, but seven.
						},
					},
					Spec: v1beta1.IngressSpec{
						Backend: &v1beta1.IngressBackend{
							ServiceName: "kuard",
							ServicePort: intstr.FromInt(8080),
						},
					},
				},
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kuard",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Protocol:   "TCP",
							Port:       8080,
							TargetPort: intstr.FromInt(8080),
						}},
					},
				},
			},
			want: map[string]*v2.RouteConfiguration{
				"ingress_http": {
					Name: "ingress_http",
					VirtualHosts: []route.VirtualHost{{
						Name:    "*",
						Domains: []string{"*"},
						Routes: []route.Route{{
							Match:               envoy.RoutePrefix("/"),
							Action:              routeretry("default/kuard/8080/da39a3ee5e", "5xx,gateway-error", 7, 0),
							RequestHeadersToAdd: envoy.RouteHeaders(),
						}},
					}},
				},
				"ingress_https": {
					Name: "ingress_https",
				},
			},
		},
		"ingress retry-on, per-try-timeout": {
			objs: []interface{}{
				&v1beta1.Ingress{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kuard",
						Namespace: "default",
						Annotations: map[string]string{
							"contour.heptio.com/retry-on":        "5xx,gateway-error",
							"contour.heptio.com/per-try-timeout": "150ms",
						},
					},
					Spec: v1beta1.IngressSpec{
						Backend: &v1beta1.IngressBackend{
							ServiceName: "kuard",
							ServicePort: intstr.FromInt(8080),
						},
					},
				},
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kuard",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Protocol:   "TCP",
							Port:       8080,
							TargetPort: intstr.FromInt(8080),
						}},
					},
				},
			},
			want: map[string]*v2.RouteConfiguration{
				"ingress_http": {
					Name: "ingress_http",
					VirtualHosts: []route.VirtualHost{{
						Name:    "*",
						Domains: []string{"*"},
						Routes: []route.Route{{
							Match:               envoy.RoutePrefix("/"),
							Action:              routeretry("default/kuard/8080/da39a3ee5e", "5xx,gateway-error", 0, 150*time.Millisecond),
							RequestHeadersToAdd: envoy.RouteHeaders(),
						}},
					}},
				},
				"ingress_https": {
					Name: "ingress_https",
				},
			},
		},
		"ingressroute no weights defined": {
			objs: []interface{}{
				&ingressroutev1.IngressRoute{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "simple",
						Namespace: "default",
					},
					Spec: ingressroutev1.IngressRouteSpec{
						VirtualHost: &ingressroutev1.VirtualHost{
							Fqdn: "www.example.com",
						},
						Routes: []ingressroutev1.Route{{
							Match: "/",
							Services: []ingressroutev1.Service{
								{
									Name: "backend",
									Port: 80,
								},
								{
									Name: "backendtwo",
									Port: 80,
								},
							},
						}},
					},
				},
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "backend",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Protocol:   "TCP",
							Port:       80,
							TargetPort: intstr.FromInt(8080),
						}},
					},
				},
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "backendtwo",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Protocol:   "TCP",
							Port:       80,
							TargetPort: intstr.FromInt(8080),
						}},
					},
				},
			},
			want: map[string]*v2.RouteConfiguration{
				"ingress_http": {
					Name: "ingress_http",
					VirtualHosts: []route.VirtualHost{{
						Name:    "www.example.com",
						Domains: domains("www.example.com"),
						Routes: []route.Route{{
							Match: envoy.RoutePrefix("/"),
							Action: &route.Route_Route{
								Route: &route.RouteAction{
									ClusterSpecifier: &route.RouteAction_WeightedClusters{
										WeightedClusters: &route.WeightedCluster{
											Clusters: weightedClusters(
												weightedCluster("default/backend/80/da39a3ee5e", 1),
												weightedCluster("default/backendtwo/80/da39a3ee5e", 1),
											),
											TotalWeight: u32(2),
										},
									},
								},
							},
							RequestHeadersToAdd: envoy.RouteHeaders(),
						}},
					}},
				},
				"ingress_https": {
					Name: "ingress_https",
				},
			},
		},
		"ingressroute one weight defined": {
			objs: []interface{}{
				&ingressroutev1.IngressRoute{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "simple",
						Namespace: "default",
					},
					Spec: ingressroutev1.IngressRouteSpec{
						VirtualHost: &ingressroutev1.VirtualHost{
							Fqdn: "www.example.com",
						},
						Routes: []ingressroutev1.Route{{
							Match: "/",
							Services: []ingressroutev1.Service{
								{
									Name: "backend",
									Port: 80,
								},
								{
									Name:   "backendtwo",
									Port:   80,
									Weight: 50,
								},
							},
						}},
					},
				},
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "backend",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Protocol:   "TCP",
							Port:       80,
							TargetPort: intstr.FromInt(8080),
						}},
					},
				},
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "backendtwo",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Protocol:   "TCP",
							Port:       80,
							TargetPort: intstr.FromInt(8080),
						}},
					},
				},
			},
			want: map[string]*v2.RouteConfiguration{
				"ingress_http": {
					Name: "ingress_http",
					VirtualHosts: []route.VirtualHost{{
						Name:    "www.example.com",
						Domains: domains("www.example.com"),
						Routes: []route.Route{{
							Match: envoy.RoutePrefix("/"),
							Action: &route.Route_Route{
								Route: &route.RouteAction{
									ClusterSpecifier: &route.RouteAction_WeightedClusters{
										WeightedClusters: &route.WeightedCluster{
											Clusters: weightedClusters(
												weightedCluster("default/backend/80/da39a3ee5e", 0),
												weightedCluster("default/backendtwo/80/da39a3ee5e", 50),
											),
											TotalWeight: u32(50),
										},
									},
								},
							},
							RequestHeadersToAdd: envoy.RouteHeaders(),
						}},
					}},
				},
				"ingress_https": {
					Name: "ingress_https",
				},
			},
		},
		"ingressroute all weights defined": {
			objs: []interface{}{
				&ingressroutev1.IngressRoute{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "simple",
						Namespace: "default",
					},
					Spec: ingressroutev1.IngressRouteSpec{
						VirtualHost: &ingressroutev1.VirtualHost{
							Fqdn: "www.example.com",
						},
						Routes: []ingressroutev1.Route{{
							Match: "/",
							Services: []ingressroutev1.Service{
								{
									Name:   "backend",
									Port:   80,
									Weight: 22,
								},
								{
									Name:   "backendtwo",
									Port:   80,
									Weight: 50,
								},
							},
						}},
					},
				},
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "backend",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Protocol:   "TCP",
							Port:       80,
							TargetPort: intstr.FromInt(8080),
						}},
					},
				},
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "backendtwo",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Protocol:   "TCP",
							Port:       80,
							TargetPort: intstr.FromInt(8080),
						}},
					},
				},
			},
			want: map[string]*v2.RouteConfiguration{
				"ingress_http": {
					Name: "ingress_http",
					VirtualHosts: []route.VirtualHost{{
						Name:    "www.example.com",
						Domains: domains("www.example.com"),
						Routes: []route.Route{{
							Match: envoy.RoutePrefix("/"),
							Action: &route.Route_Route{
								Route: &route.RouteAction{
									ClusterSpecifier: &route.RouteAction_WeightedClusters{
										WeightedClusters: &route.WeightedCluster{
											Clusters: weightedClusters(
												weightedCluster("default/backend/80/da39a3ee5e", 22),
												weightedCluster("default/backendtwo/80/da39a3ee5e", 50),
											),
											TotalWeight: u32(72),
										},
									},
								},
							},
							RequestHeadersToAdd: envoy.RouteHeaders(),
						}},
					}},
				},
				"ingress_https": {
					Name: "ingress_https",
				},
			},
		},
		"ingressroute w/ missing fqdn": {
			objs: []interface{}{
				&ingressroutev1.IngressRoute{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "simple",
						Namespace: "default",
					},
					Spec: ingressroutev1.IngressRouteSpec{
						VirtualHost: &ingressroutev1.VirtualHost{},
						Routes: []ingressroutev1.Route{{
							Match: "/",
							Services: []ingressroutev1.Service{
								{
									Name: "backend",
									Port: 80,
								},
							},
						}},
					},
				},
				&v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "backend",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{{
							Protocol:   "TCP",
							Port:       80,
							TargetPort: intstr.FromInt(8080),
						}},
					},
				},
			},
			want: map[string]*v2.RouteConfiguration{
				"ingress_http": {
					Name: "ingress_http", // should be blank, no fqdn defined.
				},
				"ingress_https": {
					Name: "ingress_https",
				},
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			reh := ResourceEventHandler{
				FieldLogger: testLogger(t),
				Notifier:    new(nullNotifier),
				Metrics:     metrics.NewMetrics(prometheus.NewRegistry()),
			}
			for _, o := range tc.objs {
				reh.OnAdd(o)
			}
			root := dag.BuildDAG(&reh.KubernetesCache)
			got := visitRoutes(root)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestSortLongestRouteFirst(t *testing.T) {
	tests := map[string]struct {
		routes []route.Route
		want   []route.Route
	}{
		"two prefixes": {
			routes: []route.Route{{
				Match: envoy.RoutePrefix("/"),
			}, {
				Match: envoy.RoutePrefix("/longer"),
			}},
			want: []route.Route{{
				Match: envoy.RoutePrefix("/longer"),
			}, {
				Match: envoy.RoutePrefix("/"),
			}},
		},
		"two regexes": {
			routes: []route.Route{{
				Match: envoy.RouteRegex("/v2"),
			}, {
				Match: envoy.RouteRegex("/v1/.+"),
			}},
			want: []route.Route{{
				Match: envoy.RouteRegex("/v2"),
			}, {
				Match: envoy.RouteRegex("/v1/.+"),
			}},
		},
		"regex sorts before prefix": {
			routes: []route.Route{{
				Match: envoy.RouteRegex("/api/v?"),
			}, {
				Match: envoy.RoutePrefix("/"),
			}, {
				Match: envoy.RouteRegex(".*"),
			}},
			want: []route.Route{{
				Match: envoy.RouteRegex("/api/v?"),
			}, {
				Match: envoy.RouteRegex(".*"),
			}, {
				Match: envoy.RoutePrefix("/"),
			}},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := append([]route.Route{}, tc.routes...) // shallow copy
			sort.Stable(longestRouteFirst(got))
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func domains(hostname string) []string {
	return []string{hostname, hostname + ":*"}
}

func routecluster(cluster string) *route.Route_Route {
	return &route.Route_Route{
		Route: &route.RouteAction{
			ClusterSpecifier: &route.RouteAction_Cluster{
				Cluster: cluster,
			},
		},
	}

}

func websocketroute(c string) *route.Route_Route {
	r := routecluster(c)
	r.Route.UpgradeConfigs = append(r.Route.UpgradeConfigs,
		&route.RouteAction_UpgradeConfig{
			UpgradeType: "websocket",
		},
	)
	return r
}

func routetimeout(cluster string, timeout *time.Duration) *route.Route_Route {
	r := routecluster(cluster)
	r.Route.Timeout = timeout
	return r
}

func routeretry(cluster string, retryOn string, numRetries int, perTryTimeout time.Duration) *route.Route_Route {
	r := routecluster(cluster)
	r.Route.RetryPolicy = &route.RetryPolicy{
		RetryOn: retryOn,
	}
	if numRetries > 0 {
		r.Route.RetryPolicy.NumRetries = u32(numRetries)
	}
	if perTryTimeout > 0 {
		r.Route.RetryPolicy.PerTryTimeout = &perTryTimeout
	}
	return r
}

func weightedClusters(first, second *route.WeightedCluster_ClusterWeight, rest ...*route.WeightedCluster_ClusterWeight) []*route.WeightedCluster_ClusterWeight {
	return append([]*route.WeightedCluster_ClusterWeight{first, second}, rest...)
}

func weightedCluster(name string, weight int) *route.WeightedCluster_ClusterWeight {
	return &route.WeightedCluster_ClusterWeight{
		Name:   name,
		Weight: u32(weight),
	}
}

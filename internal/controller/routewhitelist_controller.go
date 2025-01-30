/*
Copyright 2025.

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

package controller

import (
	"context"
	route "github.com/openshift/api/route/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	set "github.com/deckarep/golang-set/v2"
	networkingv1alpha1 "github.com/stakater/ipshield-operator/api/v1alpha1"
)

const IPShieldWatchedResourceLabel = "ip-whitelist.stakater.cloud/enabled"
const RouteWhitelistFinalizer = "ip-whitelist.stakater.cloud/finalizer"
const WhiteListAnnotation = "haproxy.router.openshift.io/ip_whitelist"

type RouteWhitelistReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=networking.stakater.com,resources=routewhitelists,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=networking.stakater.com,resources=routewhitelists/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=networking.stakater.com,resources=routewhitelists/finalizers,verbs=update

func (r *RouteWhitelistReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithName("ipShield-controller")
	logger.Info("Reconciling IPShield")

	cr := &networkingv1alpha1.RouteWhitelist{}
	err := r.Get(ctx, req.NamespacedName, cr)
	if err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, err
	}

	// Get routes
	routes := &route.RouteList{}
	err = r.List(ctx, routes, &client.ListOptions{
		LabelSelector: labels.SelectorFromSet(cr.Spec.LabelSelector.MatchLabels),
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	// Handle delete
	if cr.DeletionTimestamp != nil {
		for _, watchedRoute := range routes.Items {
			next := diffSet(strings.Split(watchedRoute.
				Annotations[WhiteListAnnotation], " "), cr.Spec.IPRanges)

			if next == "" {
				delete(watchedRoute.Annotations, WhiteListAnnotation)
			} else {
				watchedRoute.Annotations[WhiteListAnnotation] = diffSet(strings.Split(watchedRoute.
					Annotations[WhiteListAnnotation], " "), cr.Spec.IPRanges)
			}

			err = r.Update(ctx, &watchedRoute)
			if err != nil {
				return ctrl.Result{Requeue: true}, err
			}
		}

		controllerutil.RemoveFinalizer(cr, RouteWhitelistFinalizer)
		return ctrl.Result{}, r.Update(ctx, cr)
	} else {
		controllerutil.AddFinalizer(cr, RouteWhitelistFinalizer)
	}

	for _, watchedRoute := range routes.Items {
		watchedRoute.Annotations[WhiteListAnnotation] = mergeSet(strings.Split(watchedRoute.
			Annotations[WhiteListAnnotation], " "), cr.Spec.IPRanges)

		err = r.Update(ctx, &watchedRoute)
		if err != nil {
			return ctrl.Result{
				RequeueAfter: 3 * time.Minute,
			}, err
		}
	}

	return ctrl.Result{}, r.Update(ctx, cr)
}

func mergeSet(s1 []string, s2 []string) string {
	merged := set.NewSet(s1...).Union(set.NewSet(s2...))
	return setToIPString(merged)
}

func diffSet(s1 []string, s2 []string) string {
	diff := set.NewSet(s1...).Difference(set.NewSet(s2...))
	return setToIPString(diff)
}

func setToIPString(s set.Set[string]) string {
	s.Remove("")
	return strings.Join(s.ToSlice(), " ")
}

// SetupWithManager sets up the controller with the Manager.
func (r *RouteWhitelistReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&networkingv1alpha1.RouteWhitelist{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		// Watch for route labels and annotations changes
		Watches(&route.Route{}, &handler.EnqueueRequestForObject{},
			builder.WithPredicates(predicate.AnnotationChangedPredicate{}, predicate.LabelChangedPredicate{})).
		Complete(r)
}

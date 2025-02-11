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
	"fmt"
	"strings"
	"time"

	route "github.com/openshift/api/route/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	set "github.com/deckarep/golang-set/v2"
	networkingv1alpha1 "github.com/stakater/ipshield-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	IPShieldWatchedResourceLabel = "ip-whitelist.stakater.cloud/enabled"
	RouteWhitelistFinalizer      = "ip-whitelist.stakater.cloud/finalizer"
	WhiteListAnnotation          = "haproxy.router.openshift.io/ip_whitelist"

	OperatorNamespace          = "ipshield-operator-namespace"
	WatchedRoutesConfigMapName = "watched-routes"
)

type RouteWhitelistReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func setCondition(conditions *[]metav1.Condition, conditionType, status, reason, message string) {
	condition := metav1.Condition{
		Type:    conditionType,
		Status:  metav1.ConditionStatus(status),
		Reason:  reason,
		Message: message,
	}
	apimeta.SetStatusCondition(conditions, condition)
}

func setSuccessful(conditions *[]metav1.Condition, conditionType string) {
	setCondition(conditions, conditionType, string(metav1.ConditionTrue), "ReconcileSuccessful", "Reconciliation successful")
}

func setFailed(conditions *[]metav1.Condition, reconcileType string, err error) {
	setCondition(conditions, reconcileType, string(metav1.ConditionFalse), "ReconcileError", fmt.Errorf("failed due to error %s", err).Error())
}

func setWarning(conditions *[]metav1.Condition, reconcileType string, err error) {
	setCondition(conditions, reconcileType, string(metav1.ConditionFalse), "ReconcileWarning", fmt.Errorf("an error occurred %s", err).Error())
}

//+kubebuilder:rbac:groups=networking.stakater.com,resources=routewhitelists,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=networking.stakater.com,resources=routewhitelists/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=networking.stakater.com,resources=routewhitelists/finalizers,verbs=update
//+kubebuilder:rbac:groups=route.openshift.io,resources=routes,verbs=get;list;watch;update
//+kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete

func (r *RouteWhitelistReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithName("ipShield-controller")
	logger.Info("Reconciling IPShield")

	cr := &networkingv1alpha1.RouteWhitelist{}

	err := r.Get(ctx, req.NamespacedName, cr)
	if err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		} else {
			return ctrl.Result{}, err
		}
	}

	crPatchBase := client.MergeFrom(cr.DeepCopy())
	apimeta.RemoveStatusCondition(&cr.Status.Conditions, "Admitted")
	apimeta.RemoveStatusCondition(&cr.Status.Conditions, "Updating")
	setCondition(&cr.Status.Conditions, "WhiteListReconciling", "True", "ProcessingWhitelist", "Searching for routes")
	err = r.Status().Patch(ctx, cr, crPatchBase)

	// Get routes
	routes := &route.RouteList{}

	err = r.Client.List(ctx, routes, &client.ListOptions{
		LabelSelector: labels.SelectorFromSet(cr.Spec.LabelSelector.MatchLabels),
	})

	if err != nil {
		setFailed(&cr.Status.Conditions, "RouteFetchError", err)
		return ctrl.Result{}, err
	} else {
		apimeta.RemoveStatusCondition(&cr.Status.Conditions, "RouteFetchError")
	}

	// Handle delete
	if cr.DeletionTimestamp != nil {
		return r.handleDelete(ctx, routes, cr, &crPatchBase)
	} else {
		controllerutil.AddFinalizer(cr, RouteWhitelistFinalizer)
	}

	if len(routes.Items) == 0 {
		setSuccessful(&cr.Status.Conditions, "NoRoutesFound")
		_ = r.Patch(ctx, cr, crPatchBase)
		return ctrl.Result{}, nil
	}
	_ = r.Patch(ctx, cr, crPatchBase)
	return r.handleUpdate(ctx, routes, cr)
}

func (r *RouteWhitelistReconciler) handleUpdate(ctx context.Context, routes *route.RouteList, cr *networkingv1alpha1.RouteWhitelist) (ctrl.Result, error) {
	var err error
	patchBase := client.MergeFrom(cr.DeepCopy())

	apimeta.RemoveStatusCondition(&cr.Status.Conditions, "ConfigMapUpdateFailure")
	apimeta.RemoveStatusCondition(&cr.Status.Conditions, "RouteUpdateFailure")

	configMap := &corev1.ConfigMap{}
	configMapAvailable, err := r.getConfigMap(ctx, configMap)

	if err != nil {
		setWarning(&cr.Status.Conditions, "ConfigMapFetchFailure", fmt.Errorf("failed to get config map %e", err))
		_ = r.Status().Patch(ctx, cr, patchBase)
	}

	for _, watchedRoute := range routes.Items {

		apimeta.RemoveStatusCondition(&cr.Status.Conditions, "Updating") // removing previous route condition
		setCondition(&cr.Status.Conditions, "Updating", "True", "UpdatingRoute", fmt.Sprintf("Updating route '%s'", watchedRoute.Name))

		err = r.Status().Patch(ctx, cr, patchBase)

		if val, ok := watchedRoute.Labels[IPShieldWatchedResourceLabel]; !ok || val != "true" {
			err = r.unwatchRoute(ctx, watchedRoute, cr, configMapAvailable, configMap)
			if err != nil {
				apimeta.RemoveStatusCondition(&cr.Status.Conditions, "Updating")
				setFailed(&cr.Status.Conditions, "RouteUpdateFailure", err)
				_ = r.Status().Patch(ctx, cr, patchBase)
				return ctrl.Result{RequeueAfter: 3 * time.Minute}, nil
			}
			continue
		}

		if configMapAvailable {
			r.updateConfigMap(ctx, watchedRoute, cr, configMap, &patchBase)
		}

		if watchedRoute.Annotations == nil {
			watchedRoute.Annotations = make(map[string]string)
		}
		watchedRoute.Annotations[WhiteListAnnotation] = mergeSet(strings.Split(watchedRoute.Annotations[WhiteListAnnotation], " "), cr.Spec.IPRanges)

		err = r.Update(ctx, &watchedRoute)

		if err != nil {
			apimeta.RemoveStatusCondition(&cr.Status.Conditions, "Updating")
			setFailed(&cr.Status.Conditions, "RouteUpdateFailure", err)
			_ = r.Status().Patch(ctx, cr, patchBase)
			return ctrl.Result{
				RequeueAfter: 3 * time.Minute,
			}, nil
		}
	}
	apimeta.RemoveStatusCondition(&cr.Status.Conditions, "Updating")
	apimeta.RemoveStatusCondition(&cr.Status.Conditions, "WhiteListReconciling")
	setSuccessful(&cr.Status.Conditions, "Admitted")

	err = r.Status().Patch(ctx, cr, patchBase)
	return ctrl.Result{}, r.Patch(ctx, cr, patchBase)
}

func (r *RouteWhitelistReconciler) updateConfigMap(ctx context.Context, watchedRoute route.Route, cr *networkingv1alpha1.RouteWhitelist, configMap *corev1.ConfigMap, patchBase *client.Patch) {
	var err error
	apimeta.RemoveStatusCondition(&cr.Status.Conditions, "ConfigMapUpdateFailure")

	routeFullName := fmt.Sprintf("%s__%s", watchedRoute.Namespace, watchedRoute.Name)

	if _, ok := configMap.Data[routeFullName]; ok {
		return
	}

	if configMap.Data == nil {
		configMap.Data = make(map[string]string)
	}

	if originalWhitelist, ok := watchedRoute.Annotations[WhiteListAnnotation]; ok && originalWhitelist != "" {
		configMap.Data[routeFullName] = originalWhitelist
	}

	if err = r.Update(ctx, configMap); err != nil {
		apimeta.RemoveStatusCondition(&cr.Status.Conditions, "Updating")
		setWarning(&cr.Status.Conditions, "ConfigMapUpdateFailure", err)
		_ = r.Status().Patch(ctx, cr, *patchBase)
		*patchBase = client.MergeFrom(cr.DeepCopy())
	}
}

func (r *RouteWhitelistReconciler) handleDelete(ctx context.Context, routes *route.RouteList, cr *networkingv1alpha1.RouteWhitelist, crPatchBase *client.Patch) (ctrl.Result, error) {
	var err error
	configMap := &corev1.ConfigMap{}
	configMapAvailable, err := r.getConfigMap(ctx, configMap)

	if err != nil {
		setWarning(&cr.Status.Conditions, "ConfigMapFetchFailure", fmt.Errorf("failed to get config map %e", err))
		_ = r.Status().Patch(ctx, cr, *crPatchBase)
	}

	for _, watchedRoute := range routes.Items {
		if err = r.unwatchRoute(ctx, watchedRoute, cr, configMapAvailable, configMap); err != nil {
			return ctrl.Result{RequeueAfter: 3 * time.Minute}, err
		}
	}

	setSuccessful(&cr.Status.Conditions, "Deleted")
	err = r.Status().Patch(ctx, cr, *crPatchBase)

	controllerutil.RemoveFinalizer(cr, RouteWhitelistFinalizer)
	return ctrl.Result{}, r.Patch(ctx, cr, *crPatchBase)
}

func (r *RouteWhitelistReconciler) unwatchRoute(ctx context.Context, watchedRoute route.Route, cr *networkingv1alpha1.RouteWhitelist, configMapAvailable bool, configMap *corev1.ConfigMap) error {
	patchBase := client.MergeFrom(watchedRoute.DeepCopy())
	routeFullName := fmt.Sprintf("%s__%s", watchedRoute.Namespace, watchedRoute.Name)

	diff := diffSet(strings.Split(watchedRoute.Annotations[WhiteListAnnotation], " "), cr.Spec.IPRanges)
	configMapValues := configMap.Data[routeFullName]

	if diff == "" {
		diff = configMapValues
	}

	if diffSet(strings.Split(diff, " "), strings.Split(configMapValues, " ")) == "" {
		delete(configMap.Data, routeFullName)
		_ = r.Update(ctx, configMap)
	}

	if diff == "" {
		delete(watchedRoute.Annotations, WhiteListAnnotation)
	} else {
		if watchedRoute.Annotations == nil {
			watchedRoute.Annotations = make(map[string]string)
		}
		watchedRoute.Annotations[WhiteListAnnotation] = diff
	}
	return r.Patch(ctx, &watchedRoute, patchBase)
}

func (r *RouteWhitelistReconciler) getConfigMap(ctx context.Context, configMap *corev1.ConfigMap) (bool, error) {
	err := r.Get(ctx, types.NamespacedName{Name: WatchedRoutesConfigMapName, Namespace: OperatorNamespace}, configMap)

	if err != nil && errors.IsNotFound(err) {
		err = r.createConfigMap(ctx, configMap)
		if err != nil {
			return false, err
		}
		err = r.Get(ctx, types.NamespacedName{Name: WatchedRoutesConfigMapName, Namespace: OperatorNamespace}, configMap)
	}

	return err == nil, err
}

func (r *RouteWhitelistReconciler) createConfigMap(ctx context.Context, configMap *corev1.ConfigMap) error {
	configMap.ObjectMeta.Name = WatchedRoutesConfigMapName
	configMap.ObjectMeta.Namespace = OperatorNamespace

	return r.Client.Create(ctx, configMap)
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

func (r *RouteWhitelistReconciler) mapRouteToRouteWhiteList(ctx context.Context, obj client.Object) []reconcile.Request {
	logger := log.FromContext(ctx).WithName("mapRouteToRouteWhiteList")
	logger.Info("Mapping route to CR")
	openshiftRoute := obj.(*route.Route)
	var result []reconcile.Request

	if val, ok := openshiftRoute.Labels[IPShieldWatchedResourceLabel]; !ok || val != "true" {
		return result
	}

	crds := &networkingv1alpha1.RouteWhitelistList{}
	err := r.List(ctx, crds)

	if err != nil {
		logger.Error(err, "failed to fetch crd list")
		return result
	}

	for _, crd := range crds.Items {
		result = append(result, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      crd.Name,
				Namespace: crd.Namespace,
			},
		})
	}

	return result
}

// SetupWithManager sets up the controller with the Manager.
func (r *RouteWhitelistReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&networkingv1alpha1.RouteWhitelist{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		// Watch for route labels and annotations changes
		Watches(&route.Route{}, handler.EnqueueRequestsFromMapFunc(r.mapRouteToRouteWhiteList),
			builder.WithPredicates(predicate.Or(predicate.LabelChangedPredicate{}, predicate.AnnotationChangedPredicate{}))).
		Complete(r)
}

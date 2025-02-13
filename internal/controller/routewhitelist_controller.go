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

	"github.com/go-logr/logr"
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

func (r *RouteWhitelistReconciler) patchResourceAndStatus(ctx context.Context, obj client.Object, patch client.Patch, logger logr.Logger) error {
	// Sending a deep copy because the object will be updated according to the remote server state
	// so we need to keep the original object for the status update otherwise conditions will be lost
	err := r.Status().Patch(ctx, obj.DeepCopyObject().(client.Object), patch)

	if err != nil {
		logger.Error(err, "failed to update resource")
		return err
	}

	return r.Patch(ctx, obj, patch)
}

//+kubebuilder:rbac:groups=networking.stakater.com,resources=routewhitelists,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=networking.stakater.com,resources=routewhitelists/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=networking.stakater.com,resources=routewhitelists/finalizers,verbs=update
//+kubebuilder:rbac:groups=route.openshift.io,resources=routes,verbs=get;list;watch;update
//+kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete

func (r *RouteWhitelistReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithName("ipShield-controller")
	logger.Info("Reconciling IPShield")

	if req.Name == "" && req.Namespace == "" {
		return ctrl.Result{}, nil
	}

	cr := &networkingv1alpha1.RouteWhitelist{}
	err := r.Get(ctx, req.NamespacedName, cr)
	if err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	patchBase := client.MergeFrom(cr.DeepCopy())

	apimeta.RemoveStatusCondition(&cr.Status.Conditions, "Admitted")
	apimeta.RemoveStatusCondition(&cr.Status.Conditions, "Updating")
	setCondition(&cr.Status.Conditions, "WhiteListReconciling", "True", "ProcessingWhitelist", "Searching for routes")

	// Get routes
	routes := &route.RouteList{}

	err = r.List(ctx, routes, &client.ListOptions{
		LabelSelector: labels.SelectorFromSet(cr.Spec.LabelSelector.MatchLabels),
	})

	if err != nil {
		setFailed(&cr.Status.Conditions, "RouteFetchError", err)
		return r.patchErrorStatus(ctx, cr, patchBase, err)
	} else {
		apimeta.RemoveStatusCondition(&cr.Status.Conditions, "RouteFetchError")
	}

	// Handle delete
	if cr.DeletionTimestamp != nil {
		return r.handleDelete(ctx, routes, cr, patchBase, logger)
	} else {
		controllerutil.AddFinalizer(cr, RouteWhitelistFinalizer)
	}

	if len(routes.Items) == 0 {
		setSuccessful(&cr.Status.Conditions, "NoRoutesFound")
		return ctrl.Result{}, r.patchResourceAndStatus(ctx, cr, patchBase, logger)
	}

	return r.handleUpdate(ctx, routes, cr, patchBase, logger)
}

func (r *RouteWhitelistReconciler) handleUpdate(ctx context.Context, routes *route.RouteList, cr *networkingv1alpha1.RouteWhitelist, patch client.Patch, logger logr.Logger) (ctrl.Result, error) {
	apimeta.RemoveStatusCondition(&cr.Status.Conditions, "ConfigMapUpdateFailure")
	apimeta.RemoveStatusCondition(&cr.Status.Conditions, "RouteUpdateFailure")

	configMap := &corev1.ConfigMap{}
	configMapAvailable, err := r.getConfigMap(ctx, configMap)

	if err != nil {
		setWarning(&cr.Status.Conditions, "ConfigMapFetchFailure", fmt.Errorf("failed to get config map %e", err))
		return r.patchErrorStatus(ctx, cr, patch, err)
	}

	for _, watchedRoute := range routes.Items {
		routePatchBase := client.MergeFrom(watchedRoute.DeepCopy())
		apimeta.RemoveStatusCondition(&cr.Status.Conditions, "Updating") // removing previous route condition
		setCondition(&cr.Status.Conditions, "Updating", "True", "UpdatingRoute", fmt.Sprintf("Updating route '%s'", watchedRoute.Name))

		if val, ok := watchedRoute.Labels[IPShieldWatchedResourceLabel]; !ok || val != "true" {
			err = r.unwatchRoute(ctx, watchedRoute, client.MergeFrom(watchedRoute.DeepCopy()), cr, configMap, configMapAvailable, logger)

			if err != nil {
				apimeta.RemoveStatusCondition(&cr.Status.Conditions, "Updating")
				setFailed(&cr.Status.Conditions, "RouteUpdateFailure", err)
				logger.Error(err, "failed to unwatch route")
				return r.patchErrorStatus(ctx, cr, patch, err)
			}
			continue
		}

		if configMapAvailable {
			if err = r.updateConfigMap(ctx, watchedRoute, cr, configMap); err != nil {
				return ctrl.Result{}, err
			}
		}

		if watchedRoute.Annotations == nil {
			watchedRoute.Annotations = make(map[string]string)
		}

		watchedRoute.Annotations[WhiteListAnnotation] = mergeSet(strings.Split(watchedRoute.Annotations[WhiteListAnnotation], " "), cr.Spec.IPRanges)

		err = r.Patch(ctx, &watchedRoute, routePatchBase)

		if err != nil {
			apimeta.RemoveStatusCondition(&cr.Status.Conditions, "Updating")
			setFailed(&cr.Status.Conditions, "RouteUpdateFailure", err)
			logger.Error(err, "failed to update route")
			return r.patchErrorStatus(ctx, cr, patch, err)
		}
	}

	apimeta.RemoveStatusCondition(&cr.Status.Conditions, "Updating")
	apimeta.RemoveStatusCondition(&cr.Status.Conditions, "WhiteListReconciling")
	setSuccessful(&cr.Status.Conditions, "Admitted")

	return ctrl.Result{}, r.patchResourceAndStatus(ctx, cr, patch, logger)
}

func (r *RouteWhitelistReconciler) updateConfigMap(ctx context.Context, watchedRoute route.Route, cr *networkingv1alpha1.RouteWhitelist, configMap *corev1.ConfigMap) error {
	apimeta.RemoveStatusCondition(&cr.Status.Conditions, "ConfigMapUpdateFailure")

	patchBase := client.MergeFrom(configMap.DeepCopy())
	routeFullName := fmt.Sprintf("%s__%s", watchedRoute.Namespace, watchedRoute.Name)

	if _, ok := configMap.Data[routeFullName]; ok {
		return nil
	}

	if configMap.Data == nil {
		configMap.Data = make(map[string]string)
	}

	if originalWhitelist, ok := watchedRoute.Annotations[WhiteListAnnotation]; ok && originalWhitelist != "" {
		configMap.Data[routeFullName] = originalWhitelist
	}

	return r.Patch(ctx, configMap, patchBase)
}

func (r *RouteWhitelistReconciler) handleDelete(ctx context.Context, routes *route.RouteList, cr *networkingv1alpha1.RouteWhitelist, patch client.Patch, logger logr.Logger) (ctrl.Result, error) {
	var err error
	apimeta.RemoveStatusCondition(&cr.Status.Conditions, "RouteDeleteFailure")

	configMap := &corev1.ConfigMap{}
	configMapAvailable, err := r.getConfigMap(ctx, configMap)

	if err != nil {
		setWarning(&cr.Status.Conditions, "ConfigMapFetchFailure", fmt.Errorf("failed to get config map %e", err))
	}

	for _, watchedRoute := range routes.Items {
		routePatch := client.MergeFrom(watchedRoute.DeepCopy())
		if err = r.unwatchRoute(ctx, watchedRoute, routePatch, cr, configMap, configMapAvailable, logger); err != nil {
			setFailed(&cr.Status.Conditions, "RouteDeleteFailure", err)
			return r.patchErrorStatus(ctx, cr, patch, err)
		}
	}

	setSuccessful(&cr.Status.Conditions, "Deleted")
	controllerutil.RemoveFinalizer(cr, RouteWhitelistFinalizer)

	return ctrl.Result{}, r.patchResourceAndStatus(ctx, cr, patch, logger)
}

func (r *RouteWhitelistReconciler) patchErrorStatus(ctx context.Context, cr *networkingv1alpha1.RouteWhitelist, patch client.Patch, err error) (ctrl.Result, error) {
	patchErr := r.Status().Patch(ctx, cr, patch)

	if patchErr != nil {
		return ctrl.Result{}, patchErr
	}
	return ctrl.Result{}, err
}

func (r *RouteWhitelistReconciler) unwatchRoute(ctx context.Context, watchedRoute route.Route, routePatch client.Patch,
	cr *networkingv1alpha1.RouteWhitelist, configMap *corev1.ConfigMap, configMapAvailable bool, logger logr.Logger) error {
	routeFullName := fmt.Sprintf("%s__%s", watchedRoute.Namespace, watchedRoute.Name)

	configMapPatch := client.MergeFrom(configMap.DeepCopy())

	diff := diffSet(strings.Split(watchedRoute.Annotations[WhiteListAnnotation], " "), cr.Spec.IPRanges)
	configMapValues := configMap.Data[routeFullName]

	if diff == "" {
		diff = configMapValues
	}

	if diffSet(strings.Split(diff, " "), strings.Split(configMapValues, " ")) == "" {
		delete(configMap.Data, routeFullName)
	}

	if diff == "" {
		delete(watchedRoute.Annotations, WhiteListAnnotation)
	} else {
		if watchedRoute.Annotations == nil {
			watchedRoute.Annotations = make(map[string]string)
		}
		watchedRoute.Annotations[WhiteListAnnotation] = diff
	}

	if configMapAvailable {
		err := r.Patch(ctx, configMap, configMapPatch)
		if err != nil {
			logger.Error(err, "failed to update config map")
			return err
		}
	}

	return r.Patch(ctx, &watchedRoute, routePatch)
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
	openshiftRoute := obj.(*route.Route)

	if val, ok := openshiftRoute.Labels[IPShieldWatchedResourceLabel]; !ok || val != "true" {
		return nil
	}

	whitelists := &networkingv1alpha1.RouteWhitelistList{}
	err := r.List(ctx, whitelists)

	if err != nil {
		logger.Error(err, "failed to fetch crd list")
		return nil
	}

	if len(whitelists.Items) == 0 {
		return nil
	}

	result := make([]reconcile.Request, len(whitelists.Items))
	for _, crd := range whitelists.Items {

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

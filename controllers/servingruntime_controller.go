// Copyright 2021 IBM Corporation
//
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
/*
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

package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/kserve/kserve/pkg/apis/serving/v1beta1"
	"github.com/kserve/modelmesh-serving/pkg/config"

	"github.com/kserve/modelmesh-serving/pkg/mmesh"
	"github.com/kserve/modelmesh-serving/pkg/predictor_source"

	"sigs.k8s.io/controller-runtime/pkg/event"

	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/go-logr/logr"
	mf "github.com/manifestival/manifestival"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/source"

	kserveapi "github.com/kserve/kserve/pkg/apis/serving/v1alpha1"
	api "github.com/kserve/modelmesh-serving/apis/serving/v1alpha1"
	"github.com/kserve/modelmesh-serving/controllers/modelmesh"
)

// ServingRuntimeReconciler reconciles a ServingRuntime object
type ServingRuntimeReconciler struct {
	client.Client
	Log                 logr.Logger
	Scheme              *runtime.Scheme
	ConfigProvider      *config.ConfigProvider
	ConfigMapName       types.NamespacedName
	ControllerName      string
	ControllerNamespace string
	// whether the controller has cluster scope permissions
	ClusterScope bool
	// whether the controller is enabled to read and watch ClusterServingRuntimes
	EnableCSRWatch bool
	// store some information about current runtimes for making scaling decisions
	runtimeInfoMap      map[types.NamespacedName]*runtimeInfo
	runtimeInfoMapMutex sync.Mutex

	RegistryMap map[string]predictor_source.PredictorRegistry
}

type runtimeInfo struct {
	// used to implement the scale down grace period
	// nil signals that the last check had predictors
	TimeTransitionedToNoPredictors *time.Time
}

// +kubebuilder:rbac:groups=serving.kserve.io,resources=servingruntimes;servingruntimes/finalizers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=serving.kserve.io,resources=servingruntimes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments;deployments/finalizers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

func (r *ServingRuntimeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("servingruntime", req.NamespacedName)
	log.V(1).Info("ServingRuntime reconciler called")
	// Make sure the namespace has serving enabled
	mmEnabled, err := modelMeshEnabled2(ctx, req.Namespace, r.ControllerNamespace, r.Client, r.ClusterScope)
	if err != nil {
		return RequeueResult, err
	}
	runtimes := &kserveapi.ServingRuntimeList{}
	csrs := &kserveapi.ClusterServingRuntimeList{}

	if mmEnabled {
		if err = r.Client.List(ctx, runtimes, client.InNamespace(req.Namespace)); err != nil {
			return RequeueResult, err
		}
		if r.EnableCSRWatch {
			if err = r.Client.List(ctx, csrs); err != nil {
				return RequeueResult, err
			}
		}
	}

	// build the map for ServingRuntimeSpec
	srSpecs := make(map[string]*kserveapi.ServingRuntimeSpec)
	for i := range csrs.Items {
		crt := &csrs.Items[i]
		if crt.Spec.IsMultiModelRuntime() {
			srSpecs[crt.GetName()] = &crt.Spec
		}
	}
	// rt spec would override crt spec by design
	for i := range runtimes.Items {
		rt := &runtimes.Items[i]
		srSpecs[rt.GetName()] = &rt.Spec
	}

	cfg := r.ConfigProvider.GetConfig()
	cc := modelmesh.ClusterConfig{SRSpecs: srSpecs, Scheme: r.Scheme}
	if err = cc.Reconcile(ctx, req.Namespace, r.Client, cfg); err != nil {
		return RequeueResult, fmt.Errorf("could not reconcile the modelmesh type-constraints configmap: %w", err)
	}

	// Delete etcd secret when there is no ServingRuntimes in a namespace
	etcdSecretName := cfg.GetEtcdSecretName()
	if len(srSpecs) == 0 {
		// We don't delete the etcd secret in the controller namespace
		if req.Namespace != r.ControllerNamespace {
			s := &corev1.Secret{}
			err = r.Client.Get(ctx, types.NamespacedName{
				Name:      etcdSecretName,
				Namespace: req.Namespace,
			}, s)

			if err == nil {
				err = r.Delete(ctx, s)
			} else if errors.IsNotFound(err) {
				err = nil
			}
			if err != nil {
				return RequeueResult, err
			}
		}
	} else if req.Namespace != r.ControllerNamespace {
		// If not controller namespace then read etcd secret from controller namespace,
		// replace rootprefix with ns-specific one, and then create/update etcd secret (with same name)
		// in _this_ namespace and include labels similar to the tc-config configmap
		s := &corev1.Secret{}
		if err = r.Client.Get(ctx, types.NamespacedName{
			Name:      etcdSecretName,
			Namespace: r.ControllerNamespace,
		}, s); err != nil {
			return RequeueResult, fmt.Errorf("Could not get the controller etcd secret: %w", err)
		}

		data := s.Data[modelmesh.EtcdSecretKey]
		etcdConfig := mmesh.EtcdConfig{}
		if err = json.Unmarshal(data, &etcdConfig); err != nil {
			return RequeueResult, fmt.Errorf("failed to parse etcd config json: %w", err)
		}

		es := mmesh.EtcdSecret{
			Log:                 ctrl.Log.WithName("etcdSecret"),
			Name:                etcdSecretName,
			Namespace:           req.Namespace,
			ControllerNamespace: r.ControllerNamespace,
			EtcdConfig:          &etcdConfig,
			Scheme:              r.Scheme,
		}

		if err = es.Apply(ctx, r.Client); err != nil {
			return RequeueResult, fmt.Errorf("Could not apply the modelmesh etcd secret: %w", err)
		}
	}

	// Reconcile this serving runtime
	rt := &kserveapi.ServingRuntime{}
	var owner mf.Owner
	var spec *kserveapi.ServingRuntimeSpec

	if err = r.Client.Get(ctx, req.NamespacedName, rt); err == nil {
		spec = &rt.Spec
		owner = rt
	} else if errors.IsNotFound(err) {
		log.Info("Runtime is not found in namespace")

		if !r.EnableCSRWatch {
			return r.removeRuntimeFromInfoMap(req)
		}
		// try to find the runtime in cluster ServingRuntimes
		crt := &kserveapi.ClusterServingRuntime{}
		if err = r.Client.Get(ctx, types.NamespacedName{Name: req.Name}, crt); err == nil {
			spec = &crt.Spec
			owner = crt
		} else if errors.IsNotFound(err) {
			log.Info("Runtime is not found in cluster")

			// remove runtime from info map
			return r.removeRuntimeFromInfoMap(req)
		} else {
			return ctrl.Result{}, fmt.Errorf("error retrieving ClusterServingRuntime %s: %w", req.Name, err)
		}
	} else {
		return ctrl.Result{}, fmt.Errorf("error retrieving ServingRuntime %s: %w", req.NamespacedName, err)
	}

	// Check that ServerType is provided in runtime spec and that this value matches that of the specified container
	if err = validateServingRuntimeSpec(spec, cfg); err != nil {
		return ctrl.Result{}, fmt.Errorf("Invalid runtime Spec: %w", err)
	}

	// construct the deployment
	mmDeployment := modelmesh.Deployment{
		ServiceName:                cfg.InferenceServiceName,
		ServicePort:                cfg.InferenceServicePort,
		Name:                       req.Name,
		Namespace:                  req.Namespace,
		Owner:                      owner,
		SRSpec:                     spec,
		DefaultVModelOwner:         PredictorCRSourceId,
		Log:                        log,
		Metrics:                    cfg.Metrics.Enabled,
		PrometheusPort:             cfg.Metrics.Port,
		PrometheusScheme:           cfg.Metrics.Scheme,
		ModelMeshImage:             cfg.ModelMeshImage.TaggedImage(),
		ModelMeshResources:         cfg.ModelMeshResources.ToKubernetesType(),
		ModelMeshAdditionalEnvVars: cfg.InternalModelMeshEnvVars.ToKubernetesType(),
		RESTProxyEnabled:           cfg.RESTProxy.Enabled,
		RESTProxyImage:             cfg.RESTProxy.Image.TaggedImage(),
		RESTProxyPort:              cfg.RESTProxy.Port,
		RESTProxyResources:         cfg.RESTProxy.Resources.ToKubernetesType(),
		PullerImage:                cfg.StorageHelperImage.TaggedImage(),
		PullerImageCommand:         cfg.StorageHelperImage.Command,
		PullerResources:            cfg.StorageHelperResources.ToKubernetesType(),
		Port:                       cfg.InferenceServicePort,
		GrpcMaxMessageSize:         cfg.GrpcMaxMessageSizeBytes,
		// Replicas is set below
		TLSSecretName:       cfg.TLS.SecretName,
		TLSClientAuth:       cfg.TLS.ClientAuth,
		EtcdSecretName:      cfg.GetEtcdSecretName(),
		ServiceAccountName:  cfg.ServiceAccountName,
		EnableAccessLogging: cfg.EnableAccessLogging,
		Client:              r.Client,
		AnnotationsMap:      cfg.RuntimePodAnnotations,
		LabelsMap:           cfg.RuntimePodLabels,
	}
	// if the runtime is disabled, delete the deployment
	if spec.IsDisabled() || !spec.IsMultiModelRuntime() || !mmEnabled {
		log.Info("Runtime is disabled, incompatible with modelmesh, or namespace is not modelmesh-enabled")
		if err = mmDeployment.Delete(ctx, r.Client); err != nil {
			return ctrl.Result{}, fmt.Errorf("could not delete the model mesh deployment: %w", err)
		}
		return ctrl.Result{}, nil
	}

	replicas, requeueDuration, err := r.determineReplicasAndRequeueDuration(ctx, log, cfg, spec, req.NamespacedName)
	if err != nil {
		return RequeueResult, fmt.Errorf("could not determine replicas: %w", err)
	}
	mmDeployment.Replicas = replicas
	if err = mmDeployment.Apply(ctx); err != nil {
		if errors.IsConflict(err) {
			// this can occur during normal operations if the deployment was updated
			// during this reconcile loop
			log.Info("could not apply model mesh deployment due to resource conflict")
			return RequeueResult, nil
		}
		return ctrl.Result{}, fmt.Errorf("could not apply the model mesh deployment: %w", err)
	}
	return ctrl.Result{RequeueAfter: requeueDuration}, nil
}

func (r *ServingRuntimeReconciler) removeRuntimeFromInfoMap(req ctrl.Request) (ctrl.Result, error) {
	// remove runtime from info map
	r.runtimeInfoMapMutex.Lock()
	defer r.runtimeInfoMapMutex.Unlock()

	if r.runtimeInfoMap != nil {
		// this is safe even if the entry doesn't exist
		delete(r.runtimeInfoMap, req.NamespacedName)
	}
	return ctrl.Result{}, nil
}

func (r *ServingRuntimeReconciler) determineReplicasAndRequeueDuration(ctx context.Context, log logr.Logger,
	config *config.Config, rt *kserveapi.ServingRuntimeSpec, rtName types.NamespacedName) (uint16, time.Duration, error) {

	var err error
	const scaledToZero = uint16(0)
	scaledUp := r.determineReplicas(rt)

	if !config.ScaleToZero.Enabled {
		return scaledUp, time.Duration(0), nil
	}

	// check if the runtime has predictors before locking the mutex
	hasPredictors, err := r.runtimeHasPredictors(ctx, rt, rtName)
	if err != nil {
		return 0, 0, err
	}

	// we'll need to inspect/update the runtime info as well
	// lock the mutex while we may be accessing the runtimeInfoMap
	r.runtimeInfoMapMutex.Lock()
	defer r.runtimeInfoMapMutex.Unlock()

	// initialize runtime information map if it is nil
	// eg. if this is the first reconcile for any runtime
	if r.runtimeInfoMap == nil {
		r.runtimeInfoMap = make(map[types.NamespacedName]*runtimeInfo)
	}

	runtimeInfoMapKey := rtName
	targetRuntimeInfo := r.runtimeInfoMap[runtimeInfoMapKey]

	// initialize this runtime's info if it is nil
	//  set the transition time to the zero value, then, if there are no
	//  predictors, the runtime will be scaled to zero
	if targetRuntimeInfo == nil {
		targetRuntimeInfo = &runtimeInfo{
			TimeTransitionedToNoPredictors: &time.Time{},
		}
		r.runtimeInfoMap[runtimeInfoMapKey] = targetRuntimeInfo
	}

	// if the runtime has predictors, it shouldn't be scaled down
	if hasPredictors {
		// update runtime info to have transition time set to nil
		targetRuntimeInfo.TimeTransitionedToNoPredictors = nil
		return scaledUp, time.Duration(0), nil
	}

	// if this is the first time we see no predictors update the runtime info with
	// this transition
	if targetRuntimeInfo.TimeTransitionedToNoPredictors == nil {
		log.Info("Runtime no longer has any predictors, will scale to zero after grace period",
			"gracePeriod", time.Duration(config.ScaleToZero.GracePeriodSeconds)*time.Second)
		t := time.Now()
		targetRuntimeInfo.TimeTransitionedToNoPredictors = &t
	}

	// check if we are in the grace period and will requeue a reconciliation to
	// trigger after the grace period has elapsed but won't scale to zero now
	gracePeriodDuration := time.Duration(config.ScaleToZero.GracePeriodSeconds) * time.Second
	durationSinceLastTransition := time.Since(*targetRuntimeInfo.TimeTransitionedToNoPredictors)
	if durationSinceLastTransition < gracePeriodDuration {
		requeueAfter := gracePeriodDuration - durationSinceLastTransition
		log.Info("Runtime has no predictors, will scale to zero after grace period",
			"gracePeriod", gracePeriodDuration, "timeRemaning", requeueAfter)
		return scaledUp, requeueAfter, nil
	}

	// finally, if we get here, the grace period has elapsed and we should scale
	// the deployment to zero
	log.Info("Scaling runtime to zero")
	return scaledToZero, time.Duration(0), nil
}

func (r *ServingRuntimeReconciler) determineReplicas(rt *kserveapi.ServingRuntimeSpec) uint16 {
	if rt.Replicas == nil {
		return r.ConfigProvider.GetConfig().PodsPerRuntime
	}

	return *rt.Replicas
}

// runtimeHasPredictors returns true if the runtime supports an existing Predictor
func (r *ServingRuntimeReconciler) runtimeHasPredictors(ctx context.Context, rt *kserveapi.ServingRuntimeSpec, rtName types.NamespacedName) (bool, error) {
	restProxyEnabled := r.ConfigProvider.GetConfig().RESTProxy.Enabled
	f := func(p *api.Predictor) bool {
		return runtimeSupportsPredictor(rt, p, restProxyEnabled, rtName.Name)
	}

	for _, pr := range r.RegistryMap {
		if found, err := pr.Find(ctx, rtName.Namespace, f); found || err != nil {
			return found, err
		}
	}
	return false, nil
}

func runtimeSupportsPredictor(rt *kserveapi.ServingRuntimeSpec, p *api.Predictor, restProxyEnabled bool, rtName string) bool {
	// assignment to a runtime depends on the model type labels
	runtimeLabelSet := modelmesh.GetServingRuntimeLabelSet(rt, restProxyEnabled, rtName)
	predictorTypeString := modelmesh.GetPredictorTypeLabel(p)
	for _, label := range strings.Split(predictorTypeString, "|") {
		// if the runtime does not have the predictor's label, then it does not support that predictor
		if !runtimeLabelSet.Has(label) {
			return false
		}
	}
	return true
}

// getRuntimesSupportingPredictor returns a map of keys for runtimes that support the predictor p
//
// A predictor may be supported by multiple runtimes.
func (r *ServingRuntimeReconciler) getRuntimesSupportingPredictor(ctx context.Context, p *api.Predictor) (map[string]struct{}, error) {
	// list all runtimes
	runtimes := &kserveapi.ServingRuntimeList{}
	if err := r.Client.List(ctx, runtimes, client.InNamespace(p.Namespace)); err != nil {
		return nil, err
	}

	restProxyEnabled := r.ConfigProvider.GetConfig().RESTProxy.Enabled
	srnns := make(map[string]struct{})

	// list all cluster serving runtimes
	if r.EnableCSRWatch {
		csrs := &kserveapi.ClusterServingRuntimeList{}
		if err := r.Client.List(ctx, csrs); err != nil {
			return nil, err
		}
		for i := range csrs.Items {
			crt := &csrs.Items[i]
			if crt.Spec.IsMultiModelRuntime() && runtimeSupportsPredictor(&crt.Spec, p, restProxyEnabled, crt.Name) {
				srnns[crt.Name] = struct{}{}
			}
		}
	}

	for i := range runtimes.Items {
		rt := &runtimes.Items[i]
		if rt.Spec.IsMultiModelRuntime() && runtimeSupportsPredictor(&rt.Spec, p, restProxyEnabled, rt.Name) {
			srnns[rt.Name] = struct{}{}
		}
	}

	return srnns, nil
}

func (r *ServingRuntimeReconciler) SetupWithManager(mgr ctrl.Manager,
	watchInferenceServices bool, sourcePluginEvents <-chan event.GenericEvent) error {
	builder := ctrl.NewControllerManagedBy(mgr).
		Named("ServingRuntimeReconciler").
		For(&kserveapi.ServingRuntime{}).
		Owns(&appsv1.Deployment{}).
		// watch the user configmap and reconcile all runtimes when it changes
		Watches(&source.Kind{Type: &corev1.ConfigMap{}},
			config.ConfigWatchHandler(r.ConfigMapName, func() []reconcile.Request {
				return r.requestsForRuntimes("", func(namespace string) bool {
					mme, err := modelMeshEnabled2(context.TODO(), namespace,
						r.ControllerNamespace, r.Client, r.ClusterScope)
					return err != nil || mme // in case of error just reconcile anyhow
				})
			}, r.ConfigProvider, &r.Client)).
		// watch predictors and reconcile the corresponding runtime(s) it could be assigned to
		Watches(&source.Kind{Type: &api.Predictor{}},
			handler.EnqueueRequestsFromMapFunc(func(o client.Object) []reconcile.Request {
				return r.runtimeRequestsForPredictor(o.(*api.Predictor), "Predictor")
			}))

	if r.ClusterScope {
		// watch namespaces to check the modelmesh-enabled flag
		builder = builder.Watches(&source.Kind{Type: &corev1.Namespace{}}, handler.EnqueueRequestsFromMapFunc(
			func(o client.Object) []reconcile.Request {
				return r.requestsForRuntimes(o.GetName(), nil)
			}))
	}

	if watchInferenceServices {
		builder = builder.Watches(&source.Kind{Type: &v1beta1.InferenceService{}},
			handler.EnqueueRequestsFromMapFunc(func(o client.Object) []reconcile.Request {
				if p, _ := predictor_source.BuildBasePredictorFromInferenceService(o.(*v1beta1.InferenceService)); p != nil {
					return r.runtimeRequestsForPredictor(p, "InferenceService")
				}
				return []reconcile.Request{}
			}))
	}

	if r.EnableCSRWatch {
		builder = builder.Watches(&source.Kind{Type: &kserveapi.ClusterServingRuntime{}},
			handler.EnqueueRequestsFromMapFunc(func(o client.Object) []reconcile.Request {
				return r.clusterServingRuntimeRequests(o.(*kserveapi.ClusterServingRuntime))
			}))
	}

	if sourcePluginEvents != nil {
		builder.Watches(&source.Channel{Source: sourcePluginEvents},
			handler.EnqueueRequestsFromMapFunc(func(o client.Object) []reconcile.Request {
				nn, src := predictor_source.ResolveSource(types.NamespacedName{
					Name: o.GetName(), Namespace: o.GetNamespace()}, PredictorCRSourceId)
				if registry, ok := r.RegistryMap[src]; ok {
					if p, _ := registry.Get(context.TODO(), nn); p != nil {
						return r.runtimeRequestsForPredictor(p, registry.GetSourceName())
					}
				}
				return []reconcile.Request{}
			}))
	}

	return builder.Complete(r)
}

func (r *ServingRuntimeReconciler) requestsForRuntimes(namespace string,
	filter func(string) bool) []reconcile.Request {
	var opts []client.ListOption
	if namespace != "" {
		opts = []client.ListOption{client.InNamespace(namespace)}
	}
	runtimes := &kserveapi.ServingRuntimeList{}
	if err := r.Client.List(context.TODO(), runtimes, opts...); err != nil {
		r.Log.Error(err, "Error listing ServingRuntimes to reconcile", "namespace", namespace)
		return []reconcile.Request{}
	}

	var requests []reconcile.Request
	var csrs *kserveapi.ClusterServingRuntimeList
	if r.EnableCSRWatch {
		csrs = &kserveapi.ClusterServingRuntimeList{}
		if err := r.Client.List(context.TODO(), csrs); err != nil {
			r.Log.Error(err, "Error listing ClusterServingRuntimes to reconcile")
			return []reconcile.Request{}
		}
	}
	if csrs != nil && len(csrs.Items) > 0 {
		srnns := make(map[types.NamespacedName]struct{})
		var namespaces []string
		if namespace != "" {
			namespaces = []string{namespace}
		} else {
			list := &corev1.NamespaceList{}
			if err := r.Client.List(context.TODO(), list); err != nil {
				r.Log.Error(err, "Error listing namespaces to reconcile")
				return []reconcile.Request{}
			}
			for i := range list.Items {
				ns := &list.Items[i]
				if filter == nil || filter(ns.Name) {
					namespaces = append(namespaces, ns.Name)
				}
			}
		}
		for i := range csrs.Items {
			csr := &csrs.Items[i]
			if csr.Spec.IsMultiModelRuntime() {
				for _, ns := range namespaces {
					srnns[types.NamespacedName{Namespace: ns, Name: csr.Name}] = struct{}{}
				}
			}
		}
		for i := range runtimes.Items {
			rt := &runtimes.Items[i]
			if filter == nil || filter(rt.Namespace) {
				srnns[types.NamespacedName{Namespace: rt.Namespace, Name: rt.Name}] = struct{}{}
			}
		}
		for srnn := range srnns {
			requests = append(requests, reconcile.Request{NamespacedName: srnn})
		}
	} else {
		for i := range runtimes.Items {
			rt := &runtimes.Items[i]
			if filter == nil || filter(rt.Namespace) {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: rt.Name, Namespace: rt.Namespace},
				})
			}
		}
	}

	return requests
}

func (r *ServingRuntimeReconciler) runtimeRequestsForPredictor(p *api.Predictor, source string) []reconcile.Request {
	srnns, err := r.getRuntimesSupportingPredictor(context.TODO(), p)
	if err != nil {
		r.Log.Error(err, "Error getting runtimes that support predictor", "name", p.GetName(), "source", source)
		return []reconcile.Request{}
	}
	if len(srnns) == 0 {
		r.Log.Info("No runtime found to support predictor", "name", p.GetName(), "source", source)
		return []reconcile.Request{}
	}

	requests := make([]reconcile.Request, 0, len(srnns))
	for n := range srnns {
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: n, Namespace: p.GetNamespace()}})
	}
	return requests
}

func (r *ServingRuntimeReconciler) clusterServingRuntimeRequests(csr *kserveapi.ClusterServingRuntime) []reconcile.Request {
	if csr.Spec.MultiModel == nil || !*csr.Spec.MultiModel {
		return []reconcile.Request{}
	}

	// get list of namespaces
	list := &corev1.NamespaceList{}

	// return nothing if can't get namespaces
	if err := r.Client.List(context.TODO(), list); err != nil || len(list.Items) == 0 {
		r.Log.Error(err, "Error listing namespaces to reconcile")
		return []reconcile.Request{}
	}
	requests := make([]reconcile.Request, 0, len(list.Items))

	for i := range list.Items {
		ns := &list.Items[i]
		mme, err := modelMeshEnabled2(context.TODO(), ns.Name, r.ControllerNamespace, r.Client, r.ClusterScope)
		if err == nil && mme {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{
				Namespace: ns.Name,
				Name:      csr.Name,
			}})
		}
	}

	return requests
}

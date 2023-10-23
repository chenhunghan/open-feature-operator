/*
Copyright 2022.

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

package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	v1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	corev1beta1 "github.com/open-feature/open-feature-operator/apis/core/v1beta1"
	controllercommon "github.com/open-feature/open-feature-operator/common"
	"github.com/open-feature/open-feature-operator/controllers/core/flagsourceconfiguration"
	webhooks "github.com/open-feature/open-feature-operator/webhooks"
	"go.uber.org/zap/zapcore"
	appsV1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	//+kubebuilder:scaffold:imports
)

const (
	healthProbeBindAddressFlagName = "health-probe-bind-address"
	metricsBindAddressFlagName     = "metrics-bind-address"
	verboseFlagName                = "verbose"
	leaderElectFlagName            = "leader-elect"
	sidecarCpuLimitFlagName        = "sidecar-cpu-limit"
	sidecarRamLimitFlagName        = "sidecar-ram-limit"
	sidecarCpuRequestFlagName      = "sidecar-cpu-request"
	sidecarRamRequestFlagName      = "sidecar-ram-request"
	sidecarCpuLimitDefault         = "0.5"
	sidecarRamLimitDefault         = "64M"
	sidecarCpuRequestDefault       = "0.2"
	sidecarRamRequestDefault       = "32M"
)

var (
	scheme                                                                 = runtime.NewScheme()
	setupLog                                                               = ctrl.Log.WithName("setup")
	metricsAddr                                                            string
	enableLeaderElection                                                   bool
	probeAddr                                                              string
	verbose                                                                bool
	sidecarCpuLimit, sidecarRamLimit, sidecarCpuRequest, sidecarRamRequest string
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(corev1beta1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

func main() {
	flag.StringVar(&metricsAddr, metricsBindAddressFlagName, ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, healthProbeBindAddressFlagName, ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&verbose, verboseFlagName, true, "Disable verbose logging")
	flag.BoolVar(&enableLeaderElection, leaderElectFlagName, false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")

	// the following default values are chosen as a result of load testing: https://github.com/open-feature/flagd/blob/main/tests/loadtest/README.MD#performance-observations
	flag.StringVar(&sidecarCpuLimit, sidecarCpuLimitFlagName, sidecarCpuLimitDefault, "sidecar CPU limit, in cores. (500m = .5 cores)")
	flag.StringVar(&sidecarRamLimit, sidecarRamLimitFlagName, sidecarRamLimitDefault, "sidecar memory limit, in bytes. (500Gi = 500GiB = 500 * 1024 * 1024 * 1024)")
	flag.StringVar(&sidecarCpuRequest, sidecarCpuRequestFlagName, sidecarCpuRequestDefault, "sidecar CPU minimum, in cores. (500m = .5 cores)")
	flag.StringVar(&sidecarRamRequest, sidecarRamRequestFlagName, sidecarRamRequestDefault, "sidecar memory minimum, in bytes. (500Gi = 500GiB = 500 * 1024 * 1024 * 1024)")

	level := zapcore.InfoLevel
	if verbose {
		level = zapcore.DebugLevel
	}
	opts := zap.Options{
		Development: verbose,
		Level:       level,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	cpuLimitResource, err := resource.ParseQuantity(sidecarCpuLimit)
	if err != nil {
		setupLog.Error(err, "parse sidecar cpu limit", sidecarCpuLimitFlagName, sidecarCpuLimit)
		os.Exit(1)
	}

	ramLimitResource, err := resource.ParseQuantity(sidecarRamLimit)
	if err != nil {
		setupLog.Error(err, "parse sidecar ram limit", sidecarRamLimitFlagName, sidecarRamLimit)
		os.Exit(1)
	}

	cpuRequestResource, err := resource.ParseQuantity(sidecarCpuRequest)
	if err != nil {
		setupLog.Error(err, "parse sidecar cpu request", sidecarCpuRequestFlagName, sidecarCpuRequest)
		os.Exit(1)
	}

	ramRequestResource, err := resource.ParseQuantity(sidecarRamRequest)
	if err != nil {
		setupLog.Error(err, "parse sidecar ram request", sidecarRamRequestFlagName, sidecarRamRequest)
		os.Exit(1)
	}

	if cpuRequestResource.Value() > cpuLimitResource.Value() ||
		ramRequestResource.Value() > ramLimitResource.Value() {
		setupLog.Error(err, "sidecar resource request is higher than the resource maximum")
		os.Exit(1)
	}

	disableCacheFor := []client.Object{&v1.ClusterRoleBinding{}}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		MetricsBindAddress:     metricsAddr,
		Port:                   9443,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "131bf64c.openfeature.dev",
		ClientDisableCacheFor:  disableCacheFor,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// setup indexer for backfilling permissions on the flagd-kubernetes-sync role binding
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&corev1.Pod{},
		fmt.Sprintf("%s/%s", webhooks.OpenFeatureAnnotationPath, webhooks.AllowKubernetesSyncAnnotation),
		webhooks.OpenFeatureEnabledAnnotationIndex,
	); err != nil {
		setupLog.Error(
			err,
			"unable to create indexer",
			"webhook",
			fmt.Sprintf("%s/%s", webhooks.OpenFeatureAnnotationPath, webhooks.AllowKubernetesSyncAnnotation),
		)
		os.Exit(1)
	}

	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&appsV1.Deployment{},
		fmt.Sprintf("%s/%s", controllercommon.OpenFeatureAnnotationPath, controllercommon.FlagSourceConfigurationAnnotation),
		controllercommon.FlagSourceConfigurationIndex,
	); err != nil {
		setupLog.Error(
			err,
			"unable to create indexer",
			"webhook",
			fmt.Sprintf("%s/%s", webhooks.OpenFeatureAnnotationPath, webhooks.FlagSourceConfigurationAnnotation),
		)
		os.Exit(1)
	}

	// if err := (&corev1alpha1.FeatureFlagConfiguration{}).SetupWebhookWithManager(mgr); err != nil {
	// 	setupLog.Error(err, "unable to create webhook", "webhook", "FeatureFlagConfiguration")
	// 	os.Exit(1)
	// }
	cnfg, err := controllercommon.NewFlagdProxyConfiguration()
	if err != nil {
		setupLog.Error(err, "unable to create kube proxy handler configuration", "controller", "FlagSourceConfiguration")
		os.Exit(1)
	}
	kph := controllercommon.NewFlagdProxyHandler(
		cnfg,
		mgr.GetClient(),
		ctrl.Log.WithName("FlagSourceConfiguration FlagdProxyHandler"),
	)

	flagSourceController := &flagsourceconfiguration.FlagSourceConfigurationReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		Log:        ctrl.Log.WithName("FlagSourceConfiguration Controller"),
		FlagdProxy: kph,
	}
	if err = flagSourceController.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "FlagSourceConfiguration")
		os.Exit(1)
	}

	//+kubebuilder:scaffold:builder
	hookServer := mgr.GetWebhookServer()
	podMutator := &webhooks.PodMutator{
		Client:           mgr.GetClient(),
		Log:              ctrl.Log.WithName("mutating-pod-webhook"),
		FlagdProxyConfig: kph.Config(),
		FlagdInjector: &controllercommon.FlagdContainerInjector{
			Client:           mgr.GetClient(),
			Logger:           ctrl.Log.WithName("flagd-container injector"),
			FlagdProxyConfig: kph.Config(),
			FlagDResourceRequirements: corev1.ResourceRequirements{
				Limits: map[corev1.ResourceName]resource.Quantity{
					corev1.ResourceCPU:    cpuLimitResource,
					corev1.ResourceMemory: ramLimitResource,
				},
				Requests: map[corev1.ResourceName]resource.Quantity{
					corev1.ResourceCPU:    cpuRequestResource,
					corev1.ResourceMemory: ramRequestResource,
				},
			},
		},
	}
	hookServer.Register("/mutate-v1-pod", &webhook.Admission{Handler: podMutator})

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", podMutator.IsReady); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	ctx := ctrl.SetupSignalHandler()
	errChan := make(chan error, 1)
	go func(chan error) {
		if err := mgr.Start(ctx); err != nil {
			errChan <- err
		}
	}(errChan)

	setupLog.Info("restoring flagd-kubernetes-sync cluster role binding subjects from current cluster state")
	// backfill can be handled asynchronously, so we do not need to block via the channel
	go func() {
		if err := podMutator.BackfillPermissions(ctx); err != nil {
			setupLog.Error(err, "podMutator backfill permissions error")
		}
	}()

	if err := <-errChan; err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

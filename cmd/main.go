// SPDX-License-Identifier: AGPL-3.0-only

// Command stripe-provider runs the Stripe payment provider controllers
// and webhook server.
package main

import (
	"flag"
	"fmt"
	"os"

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"
	"go.miloapis.com/stripe-provider/internal/controller"
	stripewebhook "go.miloapis.com/stripe-provider/internal/webhook"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(billingv1alpha1.AddToScheme(scheme))
	utilruntime.Must(stripev1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr        string
		probeAddr          string
		enableLeader       bool
		leaderNS           string
		providerName       string
		providerConfigName string
		webhookPort        int
		webhookCertDir     string
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Prometheus metrics address.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Health/readiness probe address.")
	flag.BoolVar(&enableLeader, "leader-elect", false, "Enable leader election.")
	flag.StringVar(&leaderNS, "leader-elect-namespace", "", "Namespace for leader election.")
	flag.StringVar(&providerName, "provider-name", "stripe", "Value of PaymentMethodClass.spec.provider this controller claims.")
	flag.StringVar(&providerConfigName, "provider-config", "default", "Name of the cluster-scoped StripeProviderConfig the controllers + webhook use.")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "Port for the controller-runtime webhook server (hosts the Stripe webhook receiver).")
	flag.StringVar(&webhookCertDir, "webhook-cert-dir", "", "Directory holding the webhook TLS cert + key (tls.crt / tls.key). When empty, controller-runtime falls back to the default /tmp/k8s-webhook-server/serving-certs.")

	zapOpts := zap.Options{Development: true}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))

	webhookOpts := webhook.Options{Port: webhookPort}
	if webhookCertDir != "" {
		webhookOpts.CertDir = webhookCertDir
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          enableLeader,
		LeaderElectionID:        "stripe.billing.miloapis.com",
		LeaderElectionNamespace: leaderNS,
		WebhookServer:           webhook.NewServer(webhookOpts),
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controller.PaymentMethodWatcher{ProviderName: providerName}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "name", "PaymentMethodWatcher")
		os.Exit(1)
	}
	if err = (&controller.StripePaymentMethodReconciler{ProviderConfigName: providerConfigName}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "name", "StripePaymentMethod")
		os.Exit(1)
	}
	if err = (&controller.StripeProviderConfigReconciler{}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "name", "StripeProviderConfig")
		os.Exit(1)
	}

	// Register the Stripe webhook on the manager's shared webhook
	// server (port webhookPort, TLS via webhookCertDir). Public
	// ingress should front this with a TLS-terminated route.
	stripeHook := stripewebhook.NewStripeWebhook(mgr.GetClient(), providerConfigName)
	if err := stripeHook.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to register stripe webhook")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info(fmt.Sprintf("starting stripe-provider (provider=%s, providerConfig=%s, webhookPort=%d)", providerName, providerConfigName, webhookPort))
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited")
		os.Exit(1)
	}
}

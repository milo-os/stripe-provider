// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	stripev1alpha1 "go.miloapis.com/stripe-provider/api/v1alpha1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var (
	cfg       *rest.Config
	k8sClient client.Client
	testEnv   *envtest.Environment
	ctx       context.Context
	cancel    context.CancelFunc
)

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Stripe Provider Controller Suite")
}

// billingCRDPath returns the absolute path to the billing module's CRD
// directory, resolved via `go list -m`. This keeps the envtest CRD set
// in lock-step with whatever billing version go.mod pins (including via
// replace directives) without copying YAML fixtures.
func billingCRDPath() string {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "go.miloapis.com/billing").Output()
	Expect(err).NotTo(HaveOccurred(), "go list -m go.miloapis.com/billing failed")
	dir := strings.TrimSpace(string(out))
	Expect(dir).NotTo(BeEmpty())
	return filepath.Join(dir, "config", "base", "crd", "bases")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))
	ctx, cancel = context.WithCancel(context.TODO())

	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			// stripe-provider CRDs are local to this repo.
			filepath.Join("..", "..", "config", "base", "crd", "bases"),
			// billing CRDs come from the upstream module — go list picks
			// up the replace directive in go.mod.
			billingCRDPath(),
		},
		ErrorIfCRDPathMissing: true,
	}

	var err error
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	Expect(billingv1alpha1.AddToScheme(scheme.Scheme)).To(Succeed())
	Expect(stripev1alpha1.AddToScheme(scheme.Scheme)).To(Succeed())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())

	// Wire only the watcher under test. The StripePaymentMethod
	// reconciler talks to Stripe and is exercised separately.
	Expect((&PaymentMethodWatcher{ProviderName: "stripe"}).SetupWithManager(mgr)).To(Succeed())

	go func() {
		defer GinkgoRecover()
		Expect(mgr.Start(ctx)).To(Succeed())
	}()

	Eventually(func() bool {
		return mgr.GetCache().WaitForCacheSync(ctx)
	}, 10*time.Second, 100*time.Millisecond).Should(BeTrue())
})

var _ = AfterSuite(func() {
	cancel()
	By("tearing down the test environment")
	Expect(testEnv.Stop()).To(Succeed())
})
